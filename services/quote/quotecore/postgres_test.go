package quotecore

import (
	"database/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresQuoteReplayAndOutboxSurviveRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local PostgreSQL not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	req := QuoteRequest{CustomerID: "inst_test_" + key, BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", DestinationRail: "sim_lp_1", IdempotencyKey: key}
	var quotes [8]*ExecutableQuote
	var errs [8]error
	var group sync.WaitGroup
	for i := range quotes {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			quotes[i], errs[i] = store.Create(req, "sim_lp_1", 1200, 500, 100, time.Now().Add(time.Hour))
		}(i)
	}
	group.Wait()
	for i := range quotes {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if quotes[i].ID != quotes[0].ID {
			t.Fatal("duplicate quote")
		}
	}
	accepted, err := store.AcceptWithCorrelation(quotes[0].ID, "accept-"+key, "trd_"+key, "corr_"+key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := restarted.Get(accepted.ID)
	if !ok || loaded.Status != QuoteStatusAccepted || loaded.AcceptedTradeID != "trd_"+key {
		t.Fatal("quote state not durable")
	}
	replay, err := restarted.AcceptWithCorrelation(accepted.ID, "accept-"+key, "trd_"+key, "corr_"+key, time.Now())
	if err != nil || replay.AcceptedTradeID != accepted.AcceptedTradeID {
		t.Fatal("accept replay failed", err)
	}
	var quoteCount, eventCount int
	if err = db.QueryRow(`SELECT count(*) FROM quotes WHERE id=$1`, accepted.ID).Scan(&quoteCount); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM quote_outbox WHERE event_id=$1`, "evt_quote_accepted_"+accepted.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if quoteCount != 1 || eventCount != 1 {
		t.Fatalf("expected one quote and event, got %d and %d", quoteCount, eventCount)
	}
}
