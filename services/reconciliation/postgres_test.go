package reconciliation

import (
	"database/sql"
	"github.com/google/uuid"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

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
	first := NewServiceWithStore(store).ReconcileTransaction(expected, observed)
	defer db.Exec(`DELETE FROM reconciliation_breaks WHERE id=$1`, first.ID)
	secondService := NewServiceWithStore(store)
	second := secondService.ReconcileTransaction(expected, observed)
	if second.ID != first.ID {
		t.Fatal("break identity changed after restart")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reconciliation_breaks WHERE id=$1`, first.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one SQL break, got %d: %v", count, err)
	}
	observed.AmountMinor = expected.AmountMinor
	if brk := secondService.ReconcileTransaction(expected, observed); brk != nil {
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
	service := NewServiceWithStore(store)
	row := StatementRow{ID: "ext_pg_restart", ProviderRef: "ptx_pg_restart", ClientRef: "si_pg_restart", AmountMinor: 777, Currency: "USD", Beneficiary: "acct_pg_restart", FeeMinor: 7, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatal(err)
	}
	restarted := NewServiceWithStore(store)
	if got := len(restarted.Evidence()); got != 1 {
		t.Fatalf("expected durable evidence replay after restart, got %d records", got)
	}
}
