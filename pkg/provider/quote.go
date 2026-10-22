package provider

import "time"

// Provider-neutral quote data; no service implementation or persistence ownership.
type QuoteMode string

const (
	QuoteModeFixedSource      QuoteMode = "FIXED_SOURCE"
	QuoteModeFixedDestination QuoteMode = "FIXED_DESTINATION"
)

type QuoteRequest struct {
	CustomerID             string
	CorrelationID          string
	QuoteMode              QuoteMode
	BaseAmountMinor        int64
	DestinationAmountMinor int64
	BaseCurrency           string
	QuoteCurrency          string
	DestinationRail        string
	Urgency                string
	CustomerReference      string
	ProviderIDs            []string
	IdempotencyKey         string
}

type ProviderQuote struct {
	ID                      string
	ProviderID              string
	BaseCurrency            string
	QuoteCurrency           string
	BaseAmountMinor         int64
	RateMinor               int64
	FeeMinor                int64
	AmountOutMinor          int64
	SettlementCompatibility string
	ExpiresAt               time.Time
	Evidence                map[string]string
}
