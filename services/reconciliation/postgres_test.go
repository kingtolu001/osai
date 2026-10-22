package reconciliation

import (
	"database/sql"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresApprovalExactReloadAndConflictingWorkers(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	schema := "recon_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	if _, err := db.Exec(`SET search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	store := &PostgresStore{DB: db}
	service := mustServiceWithStore(t, store)
	const amount int64 = 9007199254740993
	if _, err := service.ProposeAdjustment("fixture", "maker", "checker", amount, "NGN", "reviewed reason"); err != nil {
		t.Fatal(err)
	}
	stale := mustServiceWithStore(t, store)
	if err := service.ApproveAdjustment("fixture", "maker", "checker"); err != nil {
		t.Fatal(err)
	}
	if err := stale.RejectAdjustment("fixture", "checker", "stale rejection"); err == nil {
		t.Fatal("stale worker reversed durable approval")
	}
	if stale.adjustments["fixture"].State != "PROPOSED" {
		t.Fatal("rejected stale write mutated local state")
	}
	restarted := mustServiceWithStore(t, store)
	if got := restarted.adjustments["fixture"]; got.AmountMinor != amount || got.Currency != "NGN" || !got.Approved {
		t.Fatalf("exact approved economics lost: %+v", got)
	}
	conflict := &Approval{ID: "approval_fixture", BreakID: "fixture", Proposer: "maker", Approver: "checker", Status: "APPROVED", ProposedEffect: map[string]any{"amount_minor": int64(1), "currency": "NGN", "break_id": "fixture"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.SaveApproval(conflict); err == nil {
		t.Fatal("different economic proposal reused approval ID")
	}
	if _, err := service.ProposeAdjustment("rejected", "maker", "checker", 50, "NGN", "original"); err != nil {
		t.Fatal(err)
	}
	if err := service.RejectAdjustment("rejected", "checker", "durable rejection reason"); err != nil {
		t.Fatal(err)
	}
	if got := mustServiceWithStore(t, store).adjustments["rejected"]; got.State != "REJECTED" || got.Comment != "durable rejection reason" {
		t.Fatalf("rejection lost on restart: %+v", got)
	}
}

func TestPostgresBreakReplayRetainsIdentityAndLifecycle(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL DSN required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skip("local PostgreSQL unavailable")
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	ref := "si_replay_" + uuid.NewString()
	expected := ExpectedTransaction{ID: ref, Provider: "flutterwave", ClientRef: ref, AmountMinor: 10000, Currency: "NGN", Beneficiary: "0690000040"}
	observed := StatementRow{ID: "ext_" + ref, Provider: "flutterwave", ClientRef: ref, AmountMinor: 9000, Currency: "NGN", Beneficiary: "0690000040"}
	store := &PostgresStore{DB: db}
	first, err := mustServiceWithStore(t, store).ReconcileTransaction(expected, observed)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DELETE FROM reconciliation_breaks WHERE id=$1`, first.ID)
	secondService := mustServiceWithStore(t, store)
	second, err := secondService.ReconcileTransaction(expected, observed)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatal("break identity changed after restart")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reconciliation_breaks WHERE id=$1`, first.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one SQL break, got %d: %v", count, err)
	}
	observed.AmountMinor = expected.AmountMinor
	if brk, err := secondService.ReconcileTransaction(expected, observed); err != nil || brk != nil {
		t.Fatalf("matching evidence left break: %+v", brk)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM reconciliation_breaks WHERE id=$1`, first.ID).Scan(&state); err != nil || state != BreakStateResolved {
		t.Fatalf("break did not resolve in SQL: %s %v", state, err)
	}
}

func TestPostgresReconciliationEvidenceSurvivesServiceRestart(t *testing.T) {
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
	if _, err := db.Exec(`TRUNCATE TABLE reconciliation_evidence RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	store := &PostgresStore{DB: db}
	service := mustServiceWithStore(t, store)
	row := StatementRow{ID: "ext_pg_restart", ProviderRef: "ptx_pg_restart", ClientRef: "si_pg_restart", AmountMinor: 777, Currency: "USD", Beneficiary: "acct_pg_restart", FeeMinor: 7, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatal(err)
	}
	restarted := mustServiceWithStore(t, store)
	if got := len(restarted.Evidence()); got != 1 {
		t.Fatalf("expected durable evidence replay after restart, got %d records", got)
	}
}
