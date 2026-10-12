package settlementstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

func TestPostgresSettlementStateSurvivesStoreRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		dsn = "******localhost:5432/osai?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	repository := Postgres{DB: db}
	store, err := settlementcore.NewStoreWithPersistence(nil, repository)
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := store.Create("acct_pg", "USD", 101, "payout")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := settlementcore.NewStoreWithPersistence(nil, repository)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := restarted.Get(instruction.ID)
	if !ok || loaded.ClientRef != instruction.ClientRef || loaded.State != settlementcore.Created {
		t.Fatalf("state did not survive restart: %+v ok=%v", loaded, ok)
	}
}

func TestPostgresCustomerSettlementReportIsTenantScopedAndPaged(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}

	institutionID := "inst_report_" + uuid.NewString()
	now := time.Now().UTC()
	type reportTestSettlement struct {
		id            string
		institutionID string
		status        string
		currency      string
		occurredAt    time.Time
	}
	items := []reportTestSettlement{
		{id: "si_" + uuid.NewString(), institutionID: institutionID, status: "CONFIRMED", currency: "USD", occurredAt: now.Add(-time.Minute)},
		{id: "si_" + uuid.NewString(), institutionID: institutionID, status: "FAILED", currency: "USD", occurredAt: now},
		{id: "si_" + uuid.NewString(), institutionID: institutionID + "_other", status: "CONFIRMED", currency: "USD", occurredAt: now},
	}
	t.Cleanup(func() {
		for _, item := range items {
			_, _ = db.Exec(`DELETE FROM settlement_transitions WHERE settlement_id=$1`, item.id)
			_, _ = db.Exec(`DELETE FROM settlement_instructions WHERE id=$1`, item.id)
		}
	})
	for _, item := range items {
		_, err := db.Exec(`INSERT INTO settlement_instructions(id,client_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id) VALUES($1,$2,'test',9500,$3,'test',$4,$5,$5,$6,$7,$8,'corr','sim_lp_1')`,
			item.id, "client_"+uuid.NewString(), item.currency, item.status, item.occurredAt, item.institutionID,
			"trade_"+uuid.NewString(), "quote_"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO settlement_transitions(settlement_id,from_state,to_state,event,reason,source,occurred_at) VALUES($1,'PROCESSING',$2,'test','test','test',$3)`, item.id, item.status, item.occurredAt)
		if err != nil {
			t.Fatal(err)
		}
	}

	repository := Postgres{DB: db}
	rows, total, err := repository.ListCustomerSettlementReport(context.Background(), institutionID, now.Add(-time.Hour), now.Add(time.Hour), "usd", "", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 1 || rows[0].SettlementID == "" {
		t.Fatalf("unexpected customer report page: total=%d rows=%+v", total, rows)
	}
	if rows[0].SettlementID != items[1].id || rows[0].Status != "FAILED" || rows[0].AmountMinor != 9500 {
		t.Fatalf("unexpected newest customer report row: %+v", rows[0])
	}
}

func TestPostgresSettlementTransitionsCreateOwnerOutboxEvents(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	repository := Postgres{DB: db}
	for _, state := range []settlementcore.State{settlementcore.Processing, settlementcore.Confirmed} {
		settlementID := "si_" + uuid.NewString()
		tradeID := "trd_" + uuid.NewString()
		institutionID := "inst_" + uuid.NewString()
		_, err = db.Exec(`INSERT INTO settlement_instructions(id,client_ref,beneficiary,amount_minor,currency,purpose,state,created_at,updated_at,institution_id,trade_id,quote_id,correlation_id,provider_id) VALUES($1,$2,'beneficiary',100,'USD','payout','SUBMITTED',now(),now(),$3,$4,'quo_test','corr_test','sim_lp_1')`,
			settlementID, "client_"+uuid.NewString(), institutionID, tradeID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM settlement_domain_outbox WHERE settlement_id=$1`, settlementID)
			_, _ = db.Exec(`DELETE FROM settlement_finality_outbox WHERE settlement_id=$1`, settlementID)
			_, _ = db.Exec(`DELETE FROM settlement_transitions WHERE settlement_id=$1`, settlementID)
			_, _ = db.Exec(`DELETE FROM settlement_instructions WHERE id=$1`, settlementID)
		})
		if err := repository.RecordRuntimeState(context.Background(), settlementID, state, "provider_ref", provider.Evidence{}); err != nil {
			t.Fatal(err)
		}
		eventType := map[settlementcore.State]string{
			settlementcore.Processing: "trade.settlement_processing",
			settlementcore.Confirmed:  "trade.settled",
		}[state]
		var eventID, gotType, eventInstitution, eventTrade string
		err = db.QueryRow(`SELECT event_id,event_type,institution_id,trade_id FROM settlement_domain_outbox WHERE settlement_id=$1`, settlementID).
			Scan(&eventID, &gotType, &eventInstitution, &eventTrade)
		if err != nil {
			t.Fatal(err)
		}
		if eventID == "" || gotType != eventType || eventInstitution != institutionID || eventTrade != tradeID {
			t.Fatalf("unexpected settlement outbox event: id=%s type=%s institution=%s trade=%s", eventID, gotType, eventInstitution, eventTrade)
		}
	}
}
