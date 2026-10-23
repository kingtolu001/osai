package quotecore

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAcceptanceClaimBindsSelectionAcrossExpiry(t *testing.T) {
	testAcceptanceClaim(t, NewQuoteStore(), nil)
}

func TestAcceptanceClaimPreservesLegacySimulatorSelection(t *testing.T) {
	store := NewQuoteStore()
	now := time.Now().UTC()
	q, err := store.Create(QuoteRequest{CustomerID: "fixture", BaseAmountMinor: 100, BaseCurrency: "NGN", QuoteCurrency: "NGN", IdempotencyKey: "legacy"}, "sim_lp_1", 1, 0, 100, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginAcceptance(q.ID, "accept", "", "correlation", now); err != nil {
		t.Fatal("legacy empty simulator beneficiary rejected", err)
	}
	if _, err = store.BeginAcceptance(q.ID, "accept", "changed", "correlation", now); err == nil {
		t.Fatal("empty selection was not frozen")
	}
	if _, err = store.Accept(q.ID, "accept", "trade-one", now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Accept(q.ID, "accept", "trade-two", now); err == nil {
		t.Fatal("same acceptance key acknowledged a different trade")
	}
}

func TestPostgresAcceptanceClaimSurvivesRestartAndExpiry(t *testing.T) {
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
	testAcceptanceClaim(t, store, db)
}

func testAcceptanceClaim(t *testing.T, store *QuoteStore, db *sql.DB) {
	t.Helper()
	start := time.Now().UTC()
	institution := "sbx:acceptance:" + uuid.NewString()
	q, err := store.Create(QuoteRequest{CustomerID: institution, BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", IdempotencyKey: uuid.NewString()}, "sbx:provider", 1, 0, 100, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if db != nil {
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM quote_outbox WHERE institution_id=$1`, institution)
			_, _ = db.Exec(`DELETE FROM quotes WHERE institution_id=$1`, institution)
		})
	}
	// Concurrent conflicting selections must establish exactly one authority.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, beneficiary := range []string{"beneficiary-a", "beneficiary-b"} {
		wg.Add(1)
		go func(i int, beneficiary string) {
			defer wg.Done()
			_, errs[i] = store.BeginAcceptance(q.ID, "accept-key", beneficiary, "original-correlation", start)
		}(i, beneficiary)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatal("conflicting acceptance claims did not serialize")
	}
	if db != nil {
		store, err = NewPostgresStore(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.ExpireDue(context.Background(), start.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	claimed, ok := store.Get(q.ID)
	if !ok || claimed.Status != QuoteStatusQuoted || claimed.AcceptanceStartedAt == nil {
		t.Fatal("pending selection lost or incorrectly expired")
	}
	if _, err = store.BeginAcceptance(q.ID, "different-key", claimed.AcceptanceBeneficiary, "other-correlation", start.Add(2*time.Hour)); err == nil {
		t.Fatal("acceptance key changed after expiry")
	}
	replay, err := store.BeginAcceptance(q.ID, "accept-key", claimed.AcceptanceBeneficiary, "other-correlation", start.Add(2*time.Hour))
	if err != nil || replay.AcceptanceCorrelation != "original-correlation" {
		t.Fatal("original selection could not resume", err)
	}
	if _, err = store.AcceptWithCorrelation(q.ID, "wrong-key", "trade", "original-correlation", start.Add(2*time.Hour)); err == nil {
		t.Fatal("unrelated acceptance bypassed expiry")
	}
	accepted, err := store.AcceptWithCorrelation(q.ID, "accept-key", "trade", "original-correlation", start.Add(2*time.Hour))
	if err != nil || accepted.Status != QuoteStatusAccepted {
		t.Fatal("durable Trade acknowledgement could not complete after expiry", err)
	}
	late, err := store.Create(QuoteRequest{CustomerID: institution, BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", IdempotencyKey: uuid.NewString()}, "sbx:provider", 1, 0, 100, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginAcceptance(late.ID, "late-key", "beneficiary", "correlation", start.Add(2*time.Hour)); err == nil {
		t.Fatal("fresh expired acceptance allowed")
	}
}
