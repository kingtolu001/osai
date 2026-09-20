package ledgerstore

import (
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
	service, err := ledgerapi.NewServiceWithStorage(Postgres{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	command := ledgerapi.Command{IdempotencyKey: "pg-idem-" + suffix, SettlementID: "si_pg_" + suffix, ExternalReference: "si_pg:" + suffix + ":confirmed", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD"}
	before := len(service.Journals())
	if _, _, err = service.ConfirmCommand(command); err != nil {
		t.Fatal(err)
	}
	restarted, err := ledgerapi.NewServiceWithStorage(Postgres{DB: db})
	if err != nil {
		t.Fatal(err)
	}
	_, replayed, err := restarted.ConfirmCommand(command)
	if err != nil || !replayed {
		t.Fatalf("expected durable replay: replayed=%v err=%v", replayed, err)
	}
	if len(restarted.Journals()) != before+1 {
		t.Fatalf("expected one new durable journal, before=%d after=%d", before, len(restarted.Journals()))
	}
}
