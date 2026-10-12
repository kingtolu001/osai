package tradecore

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/pricing"
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
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE institution_id=$1`, institutionID)
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
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, other.ID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, other.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, other.ID)
	})
	if err := store.SetStatus(second.ID, StateFunded); err != nil {
		t.Fatal(err)
	}
	var acceptedEvents, fundedEvents int
	if err := db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.accepted'`, first.ID).Scan(&acceptedEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.funded'`, second.ID).Scan(&fundedEvents); err != nil {
		t.Fatal(err)
	}
	if acceptedEvents != 1 || fundedEvents != 1 {
		t.Fatalf("trade state outbox events missing or duplicated: accepted=%d funded=%d", acceptedEvents, fundedEvents)
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

func TestPostgresPricingSnapshotPersistsAndRejectsAlteredReplay(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	institutionID := "inst_pricing_snapshot_" + uuid.NewString()
	quoteID := "quo_pricing_snapshot_" + uuid.NewString()
	snapshot := persistedFeeSnapshot(t)
	trade, err := store.CreateTradeWithPricing(institutionID, quoteID, "accept-first", "corr-first",
		snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot,
		"", "sim_lp_1", "pricing-order")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
	})
	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := restarted.GetByQuoteID(quoteID)
	if !ok || loaded.PricingSnapshot == nil || *loaded.PricingSnapshot != snapshot {
		t.Fatalf("pricing snapshot did not survive Trade store restart: trade=%+v found=%v", loaded, ok)
	}

	type result struct {
		trade *Trade
		err   error
	}
	results := make(chan result, 2)
	for _, key := range []string{"accept-retry-a", "accept-retry-b"} {
		go func(key string) {
			replayed, replayErr := restarted.CreateTradeWithPricing(institutionID, quoteID, key, "corr-retry",
				snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot,
				"", "sim_lp_1", "pricing-order")
			results <- result{trade: replayed, err: replayErr}
		}(key)
	}
	for i := 0; i < 2; i++ {
		replayed := <-results
		if replayed.err != nil || replayed.trade == nil || replayed.trade.ID != trade.ID {
			t.Fatalf("concurrent pricing replay created a second trade: trade=%+v err=%v", replayed.trade, replayed.err)
		}
	}

	altered := snapshot
	altered.ExpiresAt = time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	altered.Hash, err = pricing.Hash(altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.CreateTradeWithPricing(institutionID, quoteID, "accept-altered", "corr-altered",
		altered.SourcePrincipalMinor, altered.SourceCurrency, altered.DestinationCurrency, altered,
		"", "sim_lp_1", "pricing-order"); err == nil {
		t.Fatal("altered immutable pricing snapshot replay accepted")
	}
}

func persistedFeeSnapshot(t *testing.T) pricing.Snapshot {
	t.Helper()
	snapshot := pricing.Snapshot{
		Mode: "FIXED_SOURCE", RequestedAmountMinor: 10_000, SourcePrincipalMinor: 9_500,
		FeeMinor: 500, TotalDebitMinor: 10_000, DestinationAmountMinor: 9_500,
		SourceCurrency: "NGN", DestinationCurrency: "NGN", RateScaled: pricing.RateScale,
		RateDirection: "NGN_PER_NGN", RateScale: pricing.RateScale,
		SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "fee_test", FeeScheduleVersion: 1,
		FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500,
		MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1_000_000,
		MinimumDebitMinor: 1, MaximumDebitMinor: 2_000_000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 2_000_000,
		ExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}
	hash, err := pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Hash = hash
	if err = pricing.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
