package quotecore

import (
	"sync"
	"testing"
	"time"
)

func TestCanonicalQuoteRequestHashIsStableAndExcludesIdempotencyKey(t *testing.T) {
	first := QuoteRequest{
		CustomerID: "inst_a", QuoteMode: QuoteModeFixedSource, BaseAmountMinor: 1000,
		BaseCurrency: "USD", QuoteCurrency: "NGN", ProviderIDs: []string{"provider-b", "provider-a"},
		IdempotencyKey: "key-a",
	}
	second := first
	second.ProviderIDs = []string{"provider-a", "provider-b"}
	second.IdempotencyKey = "key-b"
	if canonicalQuoteRequestHash(first) != canonicalQuoteRequestHash(second) {
		t.Fatal("canonical request hash changed with provider ordering or idempotency key")
	}
	second.BaseAmountMinor++
	if canonicalQuoteRequestHash(first) == canonicalQuoteRequestHash(second) {
		t.Fatal("canonical request hash ignored an economic field")
	}
}

func TestInMemoryQuoteCreateIdempotencyConflictAndConcurrency(t *testing.T) {
	store := NewQuoteStore()
	request := QuoteRequest{
		CustomerID: "inst_a", QuoteMode: QuoteModeFixedSource, BaseAmountMinor: 1000,
		BaseCurrency: "USD", QuoteCurrency: "NGN", IdempotencyKey: "same-key",
	}
	const workers = 24
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			quote, err := store.Create(request, "provider-a", 100_000_000, 0, 1000, time.Now().Add(time.Minute))
			if err != nil {
				errs <- err
				return
			}
			ids <- quote.ID
		}()
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var firstID string
	for id := range ids {
		if firstID == "" {
			firstID = id
		}
		if id != firstID {
			t.Fatalf("concurrent replay returned multiple quote IDs: %s and %s", firstID, id)
		}
	}
	if firstID == "" {
		t.Fatal("no quote was created")
	}
	conflicting := request
	conflicting.BaseAmountMinor++
	if _, err := store.Create(conflicting, "provider-a", 100_000_000, 0, 1001, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("same idempotency key accepted a different economic payload")
	}
}

func TestQuotePricingSnapshotIsNotMutableThroughReturnedValues(t *testing.T) {
	store := NewQuoteStore()
	request := QuoteRequest{
		CustomerID: "inst_a", QuoteMode: QuoteModeFixedSource, BaseAmountMinor: 100,
		BaseCurrency: "NGN", QuoteCurrency: "NGN", IdempotencyKey: "immutable-snapshot",
	}
	policy := FeeSchedule{
		ID: "fee-v1", Version: 1, InstitutionID: "inst_a", SourceCurrency: "NGN", DestinationCurrency: "NGN",
		SourceExponent: 2, DestinationExponent: 2,
		MinimumSourcePrincipalMinor: 1, MaximumSourcePrincipalMinor: 1000,
		MinimumTotalDebitMinor: 1, MaximumTotalDebitMinor: 1000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 1000,
	}
	providerQuote := ProviderQuote{
		ProviderID: "provider-a", BaseCurrency: "NGN", QuoteCurrency: "NGN",
		BaseAmountMinor: 100, RateMinor: RateScale, AmountOutMinor: 100,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	priced := PricedAmounts{SourcePrincipalMinor: 100, TotalDebitMinor: 100, DestinationMinor: 100}
	quote, err := store.CreatePricedFromProvider(request, providerQuote, policy, priced)
	if err != nil {
		t.Fatal(err)
	}
	quote.Pricing.FeeSchedule.ID = "tampered"
	loaded, ok := store.Get(quote.ID)
	if !ok || loaded.Pricing.FeeSchedule.ID != "fee-v1" {
		t.Fatalf("returned quote mutated the stored pricing snapshot: %+v", loaded)
	}
	loaded.Pricing.FeeSchedule.ID = "also-tampered"
	replay, err := store.CreatePricedFromProvider(request, providerQuote, policy, priced)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Pricing.FeeSchedule.ID != "fee-v1" {
		t.Fatalf("idempotent replay did not preserve immutable pricing snapshot: %+v", replay)
	}
}
