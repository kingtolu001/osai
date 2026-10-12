package tradecore

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresListTradesFiltersAndPaginates(t *testing.T) {
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
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}

	institutionID := "inst_trade_list_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id IN (SELECT id FROM trades WHERE institution_id=$1)`, institutionID)
		_, _ = db.Exec(`DELETE FROM trades WHERE institution_id=$1`, institutionID)
	})
	first, err := store.CreateTrade(institutionID, "quo_"+uuid.NewString(), "key-1", "corr-1", 10000, "NGN", "USD", "", "", "order-42")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTrade(institutionID, "quo_"+uuid.NewString(), "key-2", "corr-2", 20000, "USD", "NGN", "", "", "order-99")
	if err != nil {
		t.Fatal(err)
	}
	otherInstitutionID := institutionID + "_other"
	other, err := store.CreateTrade(otherInstitutionID, "quo_"+uuid.NewString(), "key-3", "corr-3", 30000, "NGN", "USD", "", "", "order-42")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, other.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, other.ID)
	})
	if err := store.SetStatus(second.ID, StateFunded); err != nil {
		t.Fatal(err)
	}

	filter := TradeListFilter{
		Page: 1, PageSize: 10, From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC().Add(time.Hour),
		Currency: "nGn", Status: string(StateAccepted), CustomerReference: "order-42",
	}
	trades, total, err := store.ListTrades(institutionID, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(trades) != 1 || trades[0].ID != first.ID {
		t.Fatalf("unexpected filtered trades: total=%d trades=%+v", total, trades)
	}

	filter = TradeListFilter{Page: 1, PageSize: 1}
	pageOne, total, err := store.ListTrades(institutionID, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(pageOne) != 1 || pageOne[0].ID != second.ID {
		t.Fatalf("unexpected first page: total=%d trades=%+v", total, pageOne)
	}
}
