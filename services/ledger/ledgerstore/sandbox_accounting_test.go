package ledgerstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/services/ledger/ledgercore"
)

func sandboxAccountingDB(t *testing.T) (*sql.DB, *SandboxAccounting) {
	t.Helper()
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof database required")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := "accounting_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(func() { db.Close(); admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	store, err := NewSandboxAccounting(context.Background(), db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return db, store
}

func sandboxCommand(event string, terms SandboxTerms) SandboxAccountingCommand {
	payload := `{"fixture":true}`
	hash := sha256.Sum256([]byte(payload))
	return SandboxAccountingCommand{PolicyVersion: SandboxAccountingVersion, EventID: uuid.NewString(), Event: event, Actor: "sbx:checker", Maker: "sbx:maker", Checker: "sbx:checker", CorrelationID: "sbx:correlation", Reason: "explicit fixture observation", EvidenceHash: hex.EncodeToString(hash[:]), EvidencePayload: payload, Terms: terms}
}
func sandboxTerms() SandboxTerms {
	return SandboxTerms{TradeID: "sbx:" + uuid.NewString(), InstitutionID: "sbx:institution", PrincipalMinor: 10000, FeeMinor: 200}
}
func sandboxApply(t *testing.T, s *SandboxAccounting, event string, terms SandboxTerms) string {
	t.Helper()
	command := sandboxCommand(event, terms)
	_ = s.db.QueryRow(`SELECT revision FROM ledger_sandbox_positions WHERE trade_id=$1`, terms.TradeID).Scan(&command.ExpectedRevision)
	state, _, err := s.Apply(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func sandboxBalance(t *testing.T, db *sql.DB, currency, role string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT COALESCE(sum(debit-credit),0) FROM ledger_entries WHERE currency=$1 AND account_name=$2`, currency, role).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSandboxAccountingAtomicFeeRecoveryAndReplay(t *testing.T) {
	db, s := sandboxAccountingDB(t)
	ctx := context.Background()
	terms := sandboxTerms()
	sandboxApply(t, s, "fund", terms)
	sandboxApply(t, s, "reserve", terms)
	if _, err := NewSandboxAccounting(ctx, db, "production"); err == nil {
		t.Fatal("production selection accepted")
	}
	sandboxApply(t, s, "unknown", terms)
	if sandboxBalance(t, db, "NGN", "held") != -10200 || sandboxBalance(t, db, "NGN", "fee_revenue") != 0 {
		t.Fatal("UNKNOWN released funds or earned fee")
	}
	// Crash/failure after the principal journal must roll back the entire bundle.
	_, err := db.Exec(`CREATE FUNCTION reject_fixture_fee() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_name='fee_revenue' THEN RAISE EXCEPTION 'injected fee write failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_fee BEFORE INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_fixture_fee()`)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sandboxCommand("payout_complete", terms)
	cmd.ExpectedRevision = 3
	if _, _, err = s.Apply(ctx, cmd); err == nil {
		t.Fatal("injected persistence failure ignored")
	}
	if sandboxBalance(t, db, "NGN", "held") != -10200 || sandboxBalance(t, db, "NGN", "custody") != 10200 {
		t.Fatal("partial bundle committed")
	}
	if _, err = db.Exec(`DROP TRIGGER reject_fee ON ledger_entries`); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, replay, err := restarted.Apply(ctx, cmd); results <- replay; errs <- err }()
	}
	wg.Wait()
	close(results)
	close(errs)
	replays := 0
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for replay := range results {
		if replay {
			replays++
		}
	}
	if replays != 1 {
		t.Fatal("concurrent event was not deduplicated")
	}
	if sandboxBalance(t, db, "NGN", "held") != 0 || sandboxBalance(t, db, "NGN", "custody") != 200 || sandboxBalance(t, db, "NGN", "fee_revenue") != -200 {
		t.Fatal("incorrect completed fee disposition")
	}
	altered := cmd
	altered.Reason = "conflicting evidence"
	if _, _, err = s.Apply(ctx, altered); err == nil {
		t.Fatal("changed payload replay accepted")
	}
	if _, _, err = s.Apply(ctx, sandboxCommand("payout_complete", terms)); err == nil {
		t.Fatal("second completion accepted")
	}
	sandboxApply(t, s, "return", terms)
	bad := sandboxCommand("release_return", terms)
	bad.Maker = bad.Checker
	if _, _, err = s.Apply(ctx, bad); err == nil {
		t.Fatal("self approval accepted")
	}
	sandboxApply(t, s, "release_return", terms)
	sandboxApply(t, s, "refund_fee", terms)
	if sandboxBalance(t, db, "NGN", "available") != -10200 || sandboxBalance(t, db, "NGN", "restricted") != 0 || sandboxBalance(t, db, "NGN", "fee_refunds") != 200 {
		t.Fatal("return or fee refund incorrect")
	}
	var version, actor, evidence string
	if err = db.QueryRow(`SELECT policy_version,actor,evidence_hash FROM ledger_sandbox_events WHERE event_id=$1`, cmd.EventID).Scan(&version, &actor, &evidence); err != nil || version != SandboxAccountingVersion || actor != cmd.Actor || evidence != cmd.EvidenceHash {
		t.Fatal("durable audit metadata missing")
	}
}

func TestSandboxAccountingFXFailureRetainsDestinationAndRecovers(t *testing.T) {
	db, s := sandboxAccountingDB(t)
	terms := sandboxTerms()
	terms.PrincipalMinor = 150000
	terms.FX = &ledgercore.FXExecutionEvidence{TradeID: terms.TradeID, InstitutionID: terms.InstitutionID, QuoteID: "sbx:quote", Executor: "sbx:simulator", SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 150000, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 100, AppliedRate: "1500", RateDirection: ledgercore.SourcePerDestination, ExecutionReference: "sbx:execution", SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
	sandboxApply(t, s, "fund", terms)
	sandboxApply(t, s, "reserve", terms)
	sandboxApply(t, s, "unknown", terms)
	sandboxApply(t, s, "fx_complete", terms)
	sandboxApply(t, s, "unknown", terms)
	sandboxApply(t, s, "payout_failed", terms)
	if sandboxBalance(t, db, "USD", "restricted") != -100 || sandboxBalance(t, db, "NGN", "held") != -200 || sandboxBalance(t, db, "NGN", "available") != 0 || sandboxBalance(t, db, "NGN", "fee_revenue") != 0 {
		t.Fatal("FX payout failure recreated source funds or earned fees")
	}
	if _, _, err := s.Apply(context.Background(), sandboxCommand("non_execution", terms)); err == nil {
		t.Fatal("source refund allowed after FX")
	}
	restarted, err := NewSandboxAccounting(context.Background(), db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	sandboxApply(t, restarted, "retry_payout", terms)
	stale := sandboxCommand("payout_complete", terms)
	stale.ExpectedRevision = 4 // observation taken before failure/retry completed
	if _, _, err := restarted.Apply(context.Background(), stale); err == nil {
		t.Fatal("old payout observation changed the newer recovery attempt")
	}
	sandboxApply(t, restarted, "payout_complete", terms)
	if sandboxBalance(t, db, "USD", "custody") != 0 || sandboxBalance(t, db, "USD", "held") != 0 || sandboxBalance(t, db, "NGN", "fee_revenue") != -200 {
		t.Fatal("FX recovery finality incorrect")
	}
	sandboxApply(t, restarted, "return", terms)
	sandboxApply(t, restarted, "release_return", terms)
	if sandboxBalance(t, db, "USD", "available") != -100 || sandboxBalance(t, db, "NGN", "available") != 0 {
		t.Fatal("FX return silently converted to source currency")
	}
}

func TestSandboxAccountingRejectsUnboundEvidenceAndTerms(t *testing.T) {
	db, store := sandboxAccountingDB(t)
	terms := sandboxTerms()
	command := sandboxCommand("fund", terms)
	command.EvidencePayload = `{"fixture":true,"changed":true}`
	if _, _, err := store.Apply(context.Background(), command); err == nil {
		t.Fatal("evidence bytes were not bound to their hash")
	}
	command = sandboxCommand("fund", terms)
	command.PolicyVersion = "unapproved-policy"
	if _, _, err := store.Apply(context.Background(), command); err == nil {
		t.Fatal("unknown policy accepted")
	}
	sandboxApply(t, store, "fund", terms)
	changed := terms
	changed.FeeMinor++
	if _, _, err := store.Apply(context.Background(), sandboxCommand("reserve", changed)); err == nil {
		t.Fatal("persisted trade economics changed")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM ledger_sandbox_events`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rejected command consumed an audit event or changed economics")
	}
}
