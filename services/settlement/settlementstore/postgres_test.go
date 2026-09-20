package settlementstore

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/services/settlement/settlementcore"
)

func TestPostgresSettlementStateSurvivesStoreRestart(t *testing.T) {
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
