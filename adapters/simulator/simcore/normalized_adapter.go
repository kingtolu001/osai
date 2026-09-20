package simcore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/osai/osai/pkg/provider"
	quotecore "github.com/osai/osai/services/quote/quotecore"
)

// NormalizedAdapter exposes the Phase 2 simulator through the Phase 3 contract.
// The simulator remains the backing provider for the controlled sandbox pair.
type NormalizedAdapter struct {
	Simulator *Simulator
}

func NewNormalizedAdapter(profile FailureProfile) *NormalizedAdapter {
	return &NormalizedAdapter{Simulator: NewSimulator(profile)}
}

func (a *NormalizedAdapter) GetQuote(req quotecore.QuoteRequest) (quotecore.ProviderQuote, error) {
	return a.Simulator.GetQuote(req)
}

func (a *NormalizedAdapter) AcceptQuote(providerQuoteID string) (map[string]string, error) {
	return a.Simulator.AcceptQuote(providerQuoteID)
}

func (a *NormalizedAdapter) CreateTransfer(in provider.TransferInstruction) (provider.TransferAck, error) {
	ack, err := a.Simulator.CreateTransfer(TransferInstruction{ClientRef: in.ClientRef, Beneficiary: in.Beneficiary, AmountMinor: in.AmountMinor, Currency: in.Currency, Purpose: in.Purpose, Metadata: in.Metadata})
	if err != nil {
		return provider.TransferAck{}, normalizeError(err, "create_transfer", in.ClientRef)
	}
	return provider.TransferAck{ProviderRef: ack.ProviderRef, Status: provider.TransferStatus(ack.Status), ExpectedFinality: ack.ExpectedFinality, Evidence: evidence("create_transfer", in.ClientRef, ack.Evidence)}, nil
}

func (a *NormalizedAdapter) GetTransfer(clientRef string) (provider.TransferResult, error) {
	result, err := a.Simulator.GetTransfer(clientRef)
	if err != nil {
		return provider.TransferResult{}, normalizeError(err, "get_transfer", clientRef)
	}
	return provider.TransferResult{Status: provider.TransferStatus(result.Status), AmountMinor: result.AmountMinor, Currency: result.Currency, Beneficiary: result.Beneficiary, CompletedAt: result.CompletedAt, FailureReason: result.FailureReason, Evidence: evidence("get_transfer", clientRef, result.Evidence)}, nil
}

func (a *NormalizedAdapter) VerifyWebhook(raw []byte, headers map[string]string) (provider.WebhookEvent, error) {
	timestamp, err := time.Parse(time.RFC3339, headers["X-Osai-Timestamp"])
	if err != nil || time.Since(timestamp) > 5*time.Minute || time.Since(timestamp) < -5*time.Minute {
		return provider.WebhookEvent{}, &provider.Error{Class: provider.ErrorValidation, Code: "STALE_WEBHOOK", Message: "stale or invalid timestamp", Evidence: evidence("verify_webhook", headers["X-Provider-Event-Id"], nil)}
	}
	event, err := a.Simulator.VerifyWebhook(raw, headers)
	if err != nil {
		return provider.WebhookEvent{}, normalizeError(err, "verify_webhook", headers["X-Provider-Event-Id"])
	}
	status := provider.TransferStatus(event.Payload["status"])
	if status == "SUCCESS" {
		status = provider.TransferConfirmed
	}
	return provider.WebhookEvent{ProviderEventID: event.ProviderEventID, ClientRef: event.Payload["client_ref"], Status: status, AmountMinor: parseMinor(event.Payload["amount_minor"]), Currency: event.Payload["currency"], Beneficiary: event.Payload["beneficiary"], Evidence: evidence("verify_webhook", event.ProviderEventID, event.Payload)}, nil
}

func (a *NormalizedAdapter) ListTransactions(window any) ([]provider.ExternalTransaction, error) {
	transactions, err := a.Simulator.ListTransactions(window)
	if err != nil {
		return nil, normalizeError(err, "list_transactions", "")
	}
	result := make([]provider.ExternalTransaction, 0, len(transactions))
	for _, transaction := range transactions {
		result = append(result, provider.ExternalTransaction{ID: transaction.ID, AmountMinor: transaction.AmountMinor, Currency: transaction.Currency, Direction: transaction.Direction, OccurredAt: transaction.OccurredAt, Evidence: evidence("list_transactions", transaction.ID, nil)})
	}
	return result, nil
}

func (a *NormalizedAdapter) Health() provider.Health {
	health := a.Simulator.Health()
	return provider.Health{Up: health.Up, LatencyMs: health.LatencyMs, Capabilities: health.Capabilities, DegradedReasons: health.DegradedReasons}
}

func evidence(method, requestRef string, fields map[string]string) provider.Evidence {
	return provider.Evidence{ID: "ev_" + requestRef, Provider: "sim_lp_1", Method: method, RequestRef: requestRef, RawPayloadHash: hash(fields), ParsedAt: time.Now().UTC()}
}

func hash(fields map[string]string) string {
	value := fmt.Sprint(fields)
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func normalizeError(err error, method, requestRef string) error {
	class := provider.ErrorInternal
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "timeout"):
		class = provider.ErrorProviderTimeout
	case strings.Contains(message, "not found") && method == "get_transfer":
		class = provider.ErrorProviderUnknown
	case strings.Contains(message, "rate"):
		class = provider.ErrorRateLimited
	case strings.Contains(message, "auth"):
		class = provider.ErrorAuthFailure
	case strings.Contains(message, "invalid") || strings.Contains(message, "missing"):
		class = provider.ErrorValidation
	}
	return &provider.Error{Class: class, Code: "SIMULATOR_ERROR", Message: err.Error(), Evidence: evidence(method, requestRef, nil)}
}

func parseMinor(value string) int64 {
	var result int64
	_, _ = fmt.Sscanf(value, "%d", &result)
	return result
}
