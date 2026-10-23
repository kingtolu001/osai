package tradecore

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
)

func TestPostgresSettlementFailureAndCustomerEventCommitAtomically(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL required")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "trade_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	trade, err := store.CreateTrade("sbx:failure-events", "sbx:quote", "sbx:accept", "sbx:correlation", 9500, "NGN", "USD", "sbx:beneficiary", "sbx:provider")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetSettlementID(trade.ID, "si_fixture"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetStatus(trade.ID, StateSettling); err != nil {
		t.Fatal(err)
	}
	event := FinalityEvent{EventID: "sbx:failure-finality", InstitutionID: trade.InstitutionID, SettlementID: "si_fixture", TradeID: trade.ID, Status: StateFailed, CorrelationID: trade.CorrelationID}
	if _, err = db.Exec(`CREATE FUNCTION reject_failure_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='trade.failed' THEN RAISE EXCEPTION 'injected outbox failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_failure_event BEFORE INSERT ON trade_outbox FOR EACH ROW EXECUTE FUNCTION reject_failure_event()`); err != nil {
		t.Fatal(err)
	}
	if err = store.ApplySettlementFinality(event); err == nil {
		t.Fatal("customer event write failure ignored")
	}
	current, found := store.GetTrade(trade.ID)
	if !found || current.State != StateSettling {
		t.Fatal("trade failure escaped customer event rollback")
	}
	var inbox int
	if err = db.QueryRow(`SELECT count(*) FROM trade_settlement_event_inbox`).Scan(&inbox); err != nil || inbox != 0 {
		t.Fatal("failed event transaction acknowledged finality")
	}
	if _, err = db.Exec(`DROP TRIGGER reject_failure_event ON trade_outbox; DROP FUNCTION reject_failure_event()`); err != nil {
		t.Fatal(err)
	}
	if err = store.ApplySettlementFinality(event); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.ApplySettlementFinality(event); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = db.QueryRow(`SELECT body FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.failed'`, trade.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var customerEvent notificationv1.Event
	var payload map[string]string
	if json.Unmarshal(raw, &customerEvent) != nil || json.Unmarshal([]byte(customerEvent.PayloadJson), &payload) != nil || payload["status"] != "FAILED" || customerEvent.CorrelationId != trade.CorrelationID {
		t.Fatal("customer failure event differs from committed trade")
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.failed'`, trade.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate finality emitted a second customer failure event")
	}
}
