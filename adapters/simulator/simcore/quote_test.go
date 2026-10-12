package simcore

import (
	"testing"

	quotecore "github.com/osai/osai/services/quote/quotecore"
)

func TestSimulatorQuotesApplyScaledRateToMinorUnits(t *testing.T) {
	tests := []struct {
		name            string
		sourceCurrency  string
		destination     string
		sourceMinor     int64
		wantRate        int64
		wantDestination int64
	}{
		{
			name:            "USD to NGN",
			sourceCurrency:  "USD",
			destination:     "NGN",
			sourceMinor:     10_000,
			wantRate:        150_000_000_000,
			wantDestination: 15_000_000,
		},
		{
			name:            "NGN to USD",
			sourceCurrency:  "NGN",
			destination:     "USD",
			sourceMinor:     150_000,
			wantRate:        66_667,
			wantDestination: 100,
		},
		{
			name:            "same currency",
			sourceCurrency:  "USD",
			destination:     "USD",
			sourceMinor:     123_456,
			wantRate:        quotecore.RateScale,
			wantDestination: 123_456,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quote, err := NewSimulator(FailureProfile{}).GetQuote(quotecore.QuoteRequest{
				BaseAmountMinor: test.sourceMinor,
				BaseCurrency:    test.sourceCurrency,
				QuoteCurrency:   test.destination,
			})
			if err != nil {
				t.Fatal(err)
			}
			if quote.RateMinor != test.wantRate || quote.AmountOutMinor != test.wantDestination || quote.FeeMinor != 0 {
				t.Fatalf("unexpected simulator quote: rate=%d amount=%d fee=%d", quote.RateMinor, quote.AmountOutMinor, quote.FeeMinor)
			}
		})
	}
}

func TestSimulatorRejectsUnsupportedQuotePair(t *testing.T) {
	_, err := NewSimulator(FailureProfile{}).GetQuote(quotecore.QuoteRequest{
		BaseAmountMinor: 100,
		BaseCurrency:    "JPY",
		QuoteCurrency:   "USD",
	})
	if err == nil {
		t.Fatal("unsupported currency pair must fail closed")
	}
}
