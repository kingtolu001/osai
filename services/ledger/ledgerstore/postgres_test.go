package ledgerstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/services/ledger/ledgerapi"
)

func TestPostgresLedgerReplaySurvivesServiceRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://osai:osai@localhost:5432/osai?sslmode=disable"
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
	store := Postgres{DB: db, CustodyBankID: "test_custody"}
	service, err := ledgerapi.NewServiceWithStorage(store)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	inst, trade := "inst_pg_"+suffix, "trd_pg_"+suffix
	if _, _, err = service.PostLocalPrefund(context.Background(), ledgerapi.PrefundCommand{InstitutionID: inst, Currency: "USD", AmountMinor: 100, EvidenceReference: "bank_pg_" + suffix}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.ReserveFunding(context.Background(), ledgerapi.FundingCommand{InstitutionID: inst, TradeID: trade, Currency: "USD", AmountMinor: 100}); err != nil {
		t.Fatal(err)
	}
	command := ledgerapi.Command{IdempotencyKey: "pg-idem-" + suffix, InstitutionID: inst, TradeID: trade, SettlementID: "si_pg_" + suffix, ExternalReference: "si_pg:" + suffix + ":confirmed", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD"}
	before := len(service.Journals())
	if _, _, err = service.ConfirmCommand(command); err != nil {
		t.Fatal(err)
	}
	restarted, err := ledgerapi.NewServiceWithStorage(store)
	if err != nil {
		t.Fatal(err)
	}
	_, replayed, err := restarted.ConfirmCommand(command)
	if err != nil || !replayed {
		t.Fatalf("expected durable replay: replayed=%v err=%v", replayed, err)
	}
	if len(restarted.Journals()) != before+3 {
		t.Fatalf("expected prefund, hold, and one settlement journal, before=%d after=%d", before, len(restarted.Journals()))
	}
}
