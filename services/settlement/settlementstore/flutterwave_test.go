package settlementstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/services/settlement/settlementcore"
)

func TestFlutterwaveInstructionReferencePersistsBeforeExecution(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL DSN not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skip("PostgreSQL unavailable")
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	repository := Postgres{DB: db}
	tradeID := "trade_flw_" + time.Now().UTC().Format("20060102150405.000000000")
	create := func(account string) (settlementcore.Instruction, error) {
		return repository.CreateForTrade(context.Background(), "inst_test", tradeID, "quote_test", "corr_test", account, "NGN", 10000, "sandbox", "flutterwave", "044", account)
	}
	first, err := create("0690000040")
	if err != nil || first.ClientRef == "" || first.ProviderID != "flutterwave" {
		t.Fatalf("create: %+v %v", first, err)
	}
	second, err := create("0690000040")
	if err != nil || second.ClientRef != first.ClientRef || second.ID != first.ID {
		t.Fatalf("replay changed reference: %+v %v", second, err)
	}
	if _, err := create("0690000041"); err == nil {
		t.Fatal("changed bank account accepted as replay")
	}
	loaded, err := repository.GetByID(context.Background(), first.ID)
	if err != nil || loaded.ClientRef != first.ClientRef || loaded.BankCode != "044" || loaded.AccountNumber != "0690000040" {
		t.Fatalf("reload: %+v %v", loaded, err)
	}
}
