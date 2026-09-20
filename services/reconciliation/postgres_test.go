package reconciliation

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

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
