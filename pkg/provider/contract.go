package provider

import (
	"time"

	quotecore "github.com/osai/osai/services/quote/quotecore"
)

type ErrorClass string

const (
	ErrorProviderTimeout  ErrorClass = "PROVIDER_TIMEOUT"
	ErrorProviderUnknown  ErrorClass = "PROVIDER_UNKNOWN"
	ErrorProviderRejected ErrorClass = "PROVIDER_REJECTED"
	ErrorAuthFailure      ErrorClass = "AUTH_FAILURE"
	ErrorRateLimited      ErrorClass = "RATE_LIMITED"
	ErrorValidation       ErrorClass = "VALIDATION"
	ErrorInternal         ErrorClass = "INTERNAL"
)

type Error struct {
	Class    ErrorClass
	Code     string
	Message  string
	Evidence Evidence
}

func (e *Error) Error() string { return string(e.Class) + ": " + e.Message }

type Evidence struct {
	ID             string
	Provider       string
	Method         string
	RequestRef     string
	RawPayloadHash string
	ParsedAt       time.Time
	CorrelationID  string
}

type TransferInstruction struct {
	ClientRef   string
	Beneficiary string
	AmountMinor int64
	Currency    string
	Purpose     string
	Metadata    map[string]string
}

type TransferStatus string

const (
	TransferAccepted   TransferStatus = "ACCEPTED"
	TransferProcessing TransferStatus = "PROCESSING"
	TransferConfirmed  TransferStatus = "CONFIRMED"
	TransferFailed     TransferStatus = "FAILED"
)

type TransferAck struct {
	ProviderRef      string
	Status           TransferStatus
	ExpectedFinality string
	Evidence         Evidence
}

type TransferResult struct {
	Status        TransferStatus
	AmountMinor   int64
	Currency      string
	Beneficiary   string
	CompletedAt   *time.Time
	FailureReason string
	Evidence      Evidence
}

type WebhookEvent struct {
	ProviderEventID string
	ClientRef       string
	Status          TransferStatus
	AmountMinor     int64
	Currency        string
	Beneficiary     string
	Evidence        Evidence
}

type Health struct {
	Up              bool
	LatencyMs       int
	Capabilities    map[string]bool
	DegradedReasons []string
}

type ExternalTransaction struct {
	ID          string
	AmountMinor int64
	Currency    string
	Direction   string
	OccurredAt  time.Time
	Evidence    Evidence
}

type SettlementRail interface {
	CreateTransfer(TransferInstruction) (TransferAck, error)
	GetTransfer(clientRef string) (TransferResult, error)
	VerifyWebhook(raw []byte, headers map[string]string) (WebhookEvent, error)
	ListTransactions(window any) ([]ExternalTransaction, error)
	Health() Health
}

type LiquidityProvider interface {
	GetQuote(quotecore.QuoteRequest) (quotecore.ProviderQuote, error)
	AcceptQuote(providerQuoteID string) (map[string]string, error)
	Health() Health
}

type Adapter interface {
	SettlementRail
	LiquidityProvider
}
