package flutterwave

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osai/osai/pkg/provider"
)

// Config holds v3 provider settings. Transfers remain disabled unless Enabled
// is explicit and an attempt store is configured.
type Config struct {
	BaseURL    string
	SecretKey  string
	SecretHash string
	Env        string
	Enabled    bool
	HTTPClient *http.Client
	Timeout    time.Duration
	Attempts   AttemptStore
}

type Adapter struct {
	config              Config
	authFailed          atomic.Bool
	providerUnavailable atomic.Bool
}

var _ provider.SettlementRail = (*Adapter)(nil)

func New(config Config) *Adapter { return &Adapter{config: config} }

func (a *Adapter) WebhookAcceptedStatus() int { return http.StatusOK }

func (a *Adapter) VerifyWebhook(raw []byte, headers map[string]string) (provider.WebhookEvent, error) {
	if a == nil || a.config.SecretHash == "" {
		return provider.WebhookEvent{}, provider.ErrProviderUnavailable
	}
	var supplied string
	for key, value := range headers {
		if strings.EqualFold(key, "verif-hash") {
			supplied = value
			break
		}
	}
	if supplied == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(a.config.SecretHash)) != 1 {
		return provider.WebhookEvent{}, errors.New("webhook verification failed")
	}
	var payload struct {
		Event string `json:"event"`
		Data  struct {
			ID        json.Number     `json:"id"`
			Reference string          `json:"reference"`
			Status    string          `json:"status"`
			Amount    json.Number     `json:"amount"`
			Currency  string          `json:"currency"`
			CreatedAt string          `json:"created_at"`
			Meta      json.RawMessage `json:"meta"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return provider.WebhookEvent{}, provider.ErrMalformedWebhook
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return provider.WebhookEvent{}, provider.ErrMalformedWebhook
	} else if !errors.Is(err, io.EOF) {
		return provider.WebhookEvent{}, provider.ErrMalformedWebhook
	}
	if payload.Event != "transfer.completed" {
		return provider.WebhookEvent{}, provider.ErrWebhookIgnored
	}
	providerID := payload.Data.ID.String()
	if providerID == "" || payload.Data.Reference == "" || payload.Data.Currency == "" {
		return provider.WebhookEvent{}, provider.ErrMalformedWebhook
	}
	var status provider.TransferStatus
	switch strings.ToUpper(payload.Data.Status) {
	case "SUCCESSFUL":
		status = provider.TransferConfirmed
	case "FAILED":
		status = provider.TransferFailed
	case "NEW", "PENDING", "PROCESSING":
		status = provider.TransferProcessing
	default:
		return provider.WebhookEvent{}, provider.ErrWebhookIgnored
	}
	amount, err := minorUnits(payload.Data.Amount.String(), strings.ToUpper(payload.Data.Currency))
	if err != nil || amount <= 0 {
		return provider.WebhookEvent{}, provider.ErrMalformedWebhook
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, payload.Data.CreatedAt)
	if err != nil {
		return provider.WebhookEvent{}, provider.ErrMalformedWebhook
	}
	correlationID := ""
	var metadata map[string]json.RawMessage
	if json.Unmarshal(payload.Data.Meta, &metadata) == nil {
		_ = json.Unmarshal(metadata["correlation_id"], &correlationID)
	}
	hash := sha256.Sum256(raw)
	return provider.WebhookEvent{
		ProviderEventID: "flutterwave:transfer:" + providerID + ":" + strings.ToUpper(payload.Data.Status),
		ProviderRef:     providerID,
		ClientRef:       payload.Data.Reference,
		Status:          status,
		AmountMinor:     amount,
		Currency:        strings.ToUpper(payload.Data.Currency),
		OccurredAt:      occurredAt,
		Evidence:        provider.Evidence{ID: "flw_" + providerID + "_" + strings.ToUpper(payload.Data.Status), Provider: "flutterwave", Method: "verify_webhook", RequestRef: providerID, RawPayloadHash: "sha256:" + hex.EncodeToString(hash[:]), ParsedAt: time.Now().UTC(), CorrelationID: correlationID},
	}, nil
}

func minorUnits(amount, currency string) (int64, error) {
	exponent := -1
	switch currency {
	case "NGN", "USD", "EUR", "GBP", "GHS", "KES", "ZAR", "ETB":
		exponent = 2
	case "UGX", "RWF", "JPY":
		exponent = 0
	}
	if exponent < 0 {
		return 0, fmt.Errorf("unsupported currency")
	}
	value, ok := new(big.Rat).SetString(amount)
	if !ok || value.Sign() < 0 {
		return 0, fmt.Errorf("invalid amount")
	}
	scale := int64(1)
	for i := 0; i < exponent; i++ {
		scale *= 10
	}
	value.Mul(value, big.NewRat(scale, 1))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, fmt.Errorf("amount is not representable in minor units")
	}
	return value.Num().Int64(), nil
}
