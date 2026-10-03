package flutterwave

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/osai/osai/pkg/provider"
)

const defaultBaseURL = "https://api.flutterwave.com/v3"

type transferData struct {
	ID          json.Number `json:"id"`
	Reference   string      `json:"reference"`
	Status      string      `json:"status"`
	Amount      json.Number `json:"amount"`
	Fee         json.Number `json:"fee"`
	Currency    string      `json:"currency"`
	Account     string      `json:"account_number"`
	CreatedAt   string      `json:"created_at"`
	CompletedAt string      `json:"date_completed"`
	Message     string      `json:"complete_message"`
}

type transferEnvelope struct {
	Status string       `json:"status"`
	Data   transferData `json:"data"`
}

func (a *Adapter) outboundReady() error {
	if a == nil || !a.config.Enabled {
		return provider.ErrProviderUnavailable
	}
	return a.queryReady()
}

// Queries remain available after the new-transfer kill switch is disabled.
// Existing submitted instructions must still reach an authoritative outcome.
func (a *Adapter) queryReady() error {
	if a == nil || a.config.SecretKey == "" {
		return provider.ErrProviderUnavailable
	}
	if a.config.Env != "test" && (a.config.Env != "sandbox" || !strings.HasPrefix(a.config.SecretKey, "FLWSECK_TEST-")) {
		return provider.ErrProviderUnavailable
	}
	base := a.baseURL()
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && a.config.Env == "test")) {
		return provider.ErrProviderUnavailable
	}
	return nil
}

func (a *Adapter) baseURL() string {
	if strings.TrimSpace(a.config.BaseURL) != "" {
		return strings.TrimRight(a.config.BaseURL, "/")
	}
	return defaultBaseURL
}

func (a *Adapter) httpClient() *http.Client {
	if a.config.HTTPClient != nil {
		return a.config.HTTPClient
	}
	timeout := a.config.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// ProbeTransferHealth checks live authentication and availability before new
// settlements are admitted. It never creates money movement.
func (a *Adapter) ProbeTransferHealth(ctx context.Context) error {
	if err := a.outboundReady(); err != nil {
		return err
	}
	timeout := a.httpClient().Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := a.request(ctx, http.MethodGet, "/transfers?page_size=1", nil)
	if err != nil {
		return err
	}
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Status != "success" || len(envelope.Data) == 0 {
		a.providerUnavailable.Store(true)
		return providerError(provider.ErrorProviderUnknown, "HEALTH_RESPONSE", "provider health response ambiguous")
	}
	return nil
}

// CreateTransfer sends at most one POST for a persisted client reference. An
// ambiguous outcome is never retried inline or on a repeated activity call.
func (a *Adapter) CreateTransfer(in provider.TransferInstruction) (provider.TransferAck, error) {
	if err := a.outboundReady(); err != nil {
		return provider.TransferAck{}, err
	}
	if a.config.Attempts == nil {
		return provider.TransferAck{}, provider.ErrProviderUnavailable
	}
	bank := in.Metadata["account_bank"]
	account := in.Metadata["account_number"]
	if in.Currency != "NGN" || in.AmountMinor <= 0 || in.AmountMinor%100 != 0 || in.AmountMinor/100 > 2147483647 || !asciiDigits(bank, 3, 6) || !asciiDigits(account, 10, 10) || !validReference(in.ClientRef) {
		return provider.TransferAck{}, providerError(provider.ErrorValidation, "INVALID_TRANSFER", "invalid NGN transfer instruction")
	}
	body, err := json.Marshal(struct {
		Bank      string `json:"account_bank"`
		Account   string `json:"account_number"`
		Amount    int64  `json:"amount"`
		Currency  string `json:"currency"`
		Reference string `json:"reference"`
		Narration string `json:"narration"`
	}{bank, account, in.AmountMinor / 100, "NGN", in.ClientRef, "Osai sandbox settlement"})
	if err != nil {
		return provider.TransferAck{}, providerError(provider.ErrorInternal, "ENCODE", "transfer encoding failed")
	}
	hash := sha256.Sum256(body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := a.config.Attempts.Claim(ctx, in.ClientRef, hex.EncodeToString(hash[:]))
	if err != nil {
		return provider.TransferAck{}, providerError(provider.ErrorInternal, "CREATE_CLAIM", "transfer attempt could not be persisted")
	}
	if !first {
		result, err := a.GetTransfer(in.ClientRef)
		if err != nil {
			return provider.TransferAck{}, err
		}
		return provider.TransferAck{ProviderRef: result.ProviderRef, Status: result.Status, ExpectedFinality: "provider_status", Evidence: result.Evidence}, nil
	}
	raw, err := a.request(ctx, http.MethodPost, "/transfers", body)
	if err != nil {
		return provider.TransferAck{}, err
	}
	var envelope transferEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.Status != "success" || envelope.Data.ID.String() == "" || envelope.Data.Reference != in.ClientRef {
		return provider.TransferAck{}, providerError(provider.ErrorProviderUnknown, "CREATE_AMBIGUOUS", "provider create outcome ambiguous")
	}
	result, err := normalizeTransfer(envelope.Data, raw, "create_transfer")
	if err != nil {
		return provider.TransferAck{}, err
	}
	if result.AmountMinor != in.AmountMinor || result.Currency != in.Currency || result.ClientRef != in.ClientRef {
		return provider.TransferAck{}, providerError(provider.ErrorProviderUnknown, "CREATE_MISMATCH", "provider create response mismatch")
	}
	if err := a.config.Attempts.RecordProviderRef(ctx, in.ClientRef, result.ProviderRef); err != nil {
		return provider.TransferAck{}, providerError(provider.ErrorProviderUnknown, "CREATE_RECORD", "provider create acknowledgement could not be persisted")
	}
	return provider.TransferAck{ProviderRef: result.ProviderRef, Status: result.Status, ExpectedFinality: "provider_status", Evidence: result.Evidence}, nil
}

// GetTransfer queries by the stable merchant reference, then fetches the
// unique provider ID to obtain current authoritative status.
func (a *Adapter) GetTransfer(clientRef string) (provider.TransferResult, error) {
	if err := a.queryReady(); err != nil {
		return provider.TransferResult{}, err
	}
	if !validReference(clientRef) {
		return provider.TransferResult{}, providerError(provider.ErrorValidation, "INVALID_REFERENCE", "invalid transfer reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := a.request(ctx, http.MethodGet, "/transfers?reference="+url.QueryEscape(clientRef)+"&page_size=100", nil)
	if err != nil {
		return provider.TransferResult{}, err
	}
	var listing struct {
		Status string         `json:"status"`
		Data   []transferData `json:"data"`
	}
	if json.Unmarshal(raw, &listing) != nil || listing.Status != "success" {
		return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "LOOKUP_AMBIGUOUS", "provider lookup outcome ambiguous")
	}
	var found *transferData
	for i := range listing.Data {
		if listing.Data[i].Reference == clientRef {
			if found != nil {
				return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "DUPLICATE_REFERENCE", "multiple provider transfers share reference")
			}
			found = &listing.Data[i]
		}
	}
	if found == nil {
		return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "NOT_FOUND", "transfer not yet found by provider")
	}
	id := found.ID.String()
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "BAD_PROVIDER_ID", "provider transfer ID invalid")
	}
	raw, err = a.request(ctx, http.MethodGet, "/transfers/"+id, nil)
	if err != nil {
		return provider.TransferResult{}, err
	}
	var envelope transferEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.Status != "success" || envelope.Data.Reference != clientRef || envelope.Data.ID.String() != id {
		return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "LOOKUP_MISMATCH", "provider transfer detail mismatch")
	}
	return normalizeTransfer(envelope.Data, raw, "get_transfer")
}

func (a *Adapter) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return nil, providerError(provider.ErrorInternal, "REQUEST", "provider request could not be formed")
	}
	req.Header.Set("Authorization", "Bearer "+a.config.SecretKey)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := a.httpClient().Do(req)
	if err != nil {
		a.providerUnavailable.Store(true)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, providerError(provider.ErrorProviderTimeout, "TIMEOUT", "provider request timed out")
		}
		return nil, providerError(provider.ErrorProviderUnknown, "TRANSPORT", "provider connection failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, providerError(provider.ErrorProviderUnknown, "READ", "provider response incomplete")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			a.authFailed.Store(true)
			return nil, providerError(provider.ErrorAuthFailure, "AUTH", "provider authentication failed")
		case http.StatusTooManyRequests:
			return nil, providerError(provider.ErrorRateLimited, "RATE_LIMIT", "provider rate limit")
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return nil, providerError(provider.ErrorValidation, "REQUEST_REJECTED", "provider rejected request validation")
		case http.StatusNotFound:
			return nil, providerError(provider.ErrorProviderUnknown, "NOT_FOUND", "provider transfer not found")
		default:
			if response.StatusCode >= 500 {
				a.providerUnavailable.Store(true)
				return nil, providerError(provider.ErrorProviderUnknown, "SERVER_ERROR", "provider server outcome ambiguous")
			}
			return nil, providerError(provider.ErrorProviderRejected, "REJECTED", "provider rejected transfer")
		}
	}
	a.authFailed.Store(false)
	a.providerUnavailable.Store(false)
	return raw, nil
}

func normalizeTransfer(data transferData, raw []byte, method string) (provider.TransferResult, error) {
	amount, err := minorUnits(data.Amount.String(), strings.ToUpper(data.Currency))
	if err != nil || amount <= 0 || data.ID.String() == "" || data.Reference == "" {
		return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "BAD_RESPONSE", "provider transfer response incomplete")
	}
	var status provider.TransferStatus
	switch strings.ToUpper(data.Status) {
	case "NEW", "ACCEPTED":
		status = provider.TransferAccepted
	case "PENDING", "PROCESSING":
		status = provider.TransferProcessing
	case "SUCCESSFUL":
		status = provider.TransferConfirmed
	case "FAILED":
		status = provider.TransferFailed
	default:
		return provider.TransferResult{}, providerError(provider.ErrorProviderUnknown, "UNKNOWN_STATUS", "provider transfer status ambiguous")
	}
	hash := sha256.Sum256(raw)
	result := provider.TransferResult{ProviderRef: data.ID.String(), ClientRef: data.Reference, Status: status, AmountMinor: amount, Currency: strings.ToUpper(data.Currency), Evidence: provider.Evidence{ID: "flw_" + data.ID.String() + "_" + method + "_" + hex.EncodeToString(hash[:8]), Provider: "flutterwave", Method: method, RequestRef: data.Reference, RawPayloadHash: "sha256:" + hex.EncodeToString(hash[:]), ParsedAt: time.Now().UTC()}}
	if data.CompletedAt != "" {
		if completed, err := time.Parse(time.RFC3339Nano, data.CompletedAt); err == nil {
			result.CompletedAt = &completed
		}
	}
	if status == provider.TransferFailed {
		result.FailureReason = data.Message
	}
	return result, nil
}

func providerError(class provider.ErrorClass, code, message string) error {
	return &provider.Error{Class: class, Code: code, Message: message}
}

func asciiDigits(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validReference(ref string) bool {
	if len(ref) == 0 || len(ref) > 100 {
		return false
	}
	for i := range ref {
		b := ref[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
			return false
		}
	}
	return true
}

// TransactionWindow is a bounded provider statement query for reconciliation.
type TransactionWindow struct {
	From, To  time.Time
	PageSize  int
	Reference string
}

func (a *Adapter) ListTransactions(window any) ([]provider.ExternalTransaction, error) {
	if err := a.queryReady(); err != nil {
		return nil, err
	}
	w, ok := window.(TransactionWindow)
	if !ok || w.From.IsZero() || w.To.IsZero() || w.To.Before(w.From) || w.PageSize < 1 || w.PageSize > 100 {
		return nil, providerError(provider.ErrorValidation, "WINDOW", "invalid provider statement window")
	}
	if w.Reference != "" && !validReference(w.Reference) {
		return nil, providerError(provider.ErrorValidation, "WINDOW_REFERENCE", "invalid statement reference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items := make([]provider.ExternalTransaction, 0)
	for page := 1; page <= 100; page++ {
		path := fmt.Sprintf("/transfers?from=%s&to=%s&page=%d&page_size=%d", url.QueryEscape(w.From.Format("2006-01-02")), url.QueryEscape(w.To.Format("2006-01-02")), page, w.PageSize)
		if w.Reference != "" {
			path += "&reference=" + url.QueryEscape(w.Reference)
		}
		raw, err := a.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var listing struct {
			Status string         `json:"status"`
			Data   []transferData `json:"data"`
		}
		if json.Unmarshal(raw, &listing) != nil || listing.Status != "success" {
			return nil, providerError(provider.ErrorProviderUnknown, "LIST_AMBIGUOUS", "provider statement response ambiguous")
		}
		for _, data := range listing.Data {
			if w.Reference != "" && data.Reference != w.Reference {
				continue
			}
			itemRaw, _ := json.Marshal(data)
			result, err := normalizeTransfer(data, itemRaw, "list_transactions")
			if err != nil {
				return nil, err
			}
			occurredAt, err := time.Parse(time.RFC3339Nano, data.CreatedAt)
			if err != nil {
				return nil, providerError(provider.ErrorProviderUnknown, "BAD_DATE", "provider statement date invalid")
			}
			beneficiary := ""
			if asciiDigits(data.Account, 10, 10) {
				beneficiary = data.Account
			}
			feeMinor := int64(0)
			if data.Fee.String() != "" {
				feeMinor, err = minorUnits(data.Fee.String(), result.Currency)
				if err != nil {
					return nil, providerError(provider.ErrorProviderUnknown, "BAD_FEE", "provider statement fee invalid")
				}
			}
			items = append(items, provider.ExternalTransaction{ID: result.ProviderRef, ClientRef: result.ClientRef, ProviderRef: result.ProviderRef, AmountMinor: result.AmountMinor, Currency: result.Currency, Beneficiary: beneficiary, FeeMinor: feeMinor, Direction: "outbound", OccurredAt: occurredAt, Evidence: result.Evidence})
		}
		if len(listing.Data) < w.PageSize {
			return items, nil
		}
	}
	return nil, providerError(provider.ErrorProviderUnknown, "PAGINATION_LIMIT", "provider statement page limit reached")
}

func (a *Adapter) Health() provider.Health {
	webhook := a != nil && a.config.SecretHash != ""
	query := a != nil && a.queryReady() == nil && !a.authFailed.Load() && !a.providerUnavailable.Load()
	transfer := a != nil && a.outboundReady() == nil && a.config.Attempts != nil && !a.authFailed.Load() && !a.providerUnavailable.Load()
	degraded := []string{}
	if !webhook {
		degraded = append(degraded, "webhook secret hash unavailable")
	}
	if !transfer {
		degraded = append(degraded, "transfer capability disabled or unavailable")
	}
	return provider.Health{Up: webhook || query, Capabilities: map[string]bool{"webhook": webhook, "transfer": transfer, "get_transfer": query, "list_transactions": query}, DegradedReasons: degraded}
}
