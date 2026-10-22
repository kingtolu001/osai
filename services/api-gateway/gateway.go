package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/correlation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	errWebhookEventNotFound    = errors.New("webhook event not found")
	errWebhookReplayNotAllowed = errors.New("webhook event cannot be replayed")
	errCustomerAuthUnavailable = errors.New("customer authentication service unavailable")
)

type CustomerAuthContext struct {
	InstitutionID     string
	InstitutionStatus string
	KYBStatus         string
	APIAccessStatus   string
	Environment       string
	APIEnabled        bool
	Entitlements      []string
}

type WebhookConfig struct {
	InstitutionID string
	WebhookURL    string
	Secret        string
	Enabled       bool
	Status        string
}

type QuoteRequest struct {
	InstitutionID          string
	QuoteMode              string
	BaseAmountMinor        int64
	DestinationAmountMinor int64
	BaseCurrency           string
	QuoteCurrency          string
	DestinationRail        string
	CustomerReference      string
	Urgency                string
	IdempotencyKey         string
	CorrelationID          string
}

type QuoteResponse struct {
	QuoteID                  string
	TradeID                  string
	Status                   string
	BaseAmountMinor          int64
	BaseCurrency             string
	QuoteCurrency            string
	DestinationRail          string
	AmountOutMinor           int64
	RateMinor                int64
	FeeMinor                 int64
	QuoteMode                string
	SourcePrincipalMinor     int64
	TotalDebitMinor          int64
	TotalCostMinor           int64
	DestinationTargetMinor   int64
	DestinationSurplusMinor  int64
	RateDirection            string
	RateScale                int64
	FeeScheduleID            string
	FeeScheduleVersion       int64
	SourceMinorExponent      uint32
	DestinationMinorExponent uint32
	ExpiresAt                time.Time
	CorrelationID            string
}

type TradeReadResponse struct {
	TradeID           string
	InstitutionID     string
	QuoteID           string
	Status            string
	BaseAmountMinor   int64
	BaseCurrency      string
	QuoteCurrency     string
	SettlementID      string
	CustomerReference string
	CorrelationID     string
	RequestID         string
	PricingSnapshot   *TradePricingSnapshot
	FXExecutionID     string
	FXExecutorID      string
	FXExecutionRef    string
	FXExecutionStatus string
}

type TradePricingSnapshot struct {
	QuoteMode                string `json:"quote_mode"`
	RequestedAmountMinor     int64  `json:"requested_amount_minor"`
	SourcePrincipalMinor     int64  `json:"source_principal_minor"`
	FeeMinor                 int64  `json:"fee_minor"`
	TotalDebitMinor          int64  `json:"total_debit_minor"`
	DestinationAmountMinor   int64  `json:"destination_amount_minor"`
	DestinationTargetMinor   int64  `json:"destination_target_minor"`
	DestinationSurplusMinor  int64  `json:"destination_surplus_minor"`
	SourceCurrency           string `json:"source_currency"`
	DestinationCurrency      string `json:"destination_currency"`
	RateScaled               int64  `json:"rate_scaled"`
	RateDirection            string `json:"rate_direction"`
	RateScale                int64  `json:"rate_scale"`
	FeeScheduleID            string `json:"fee_schedule_id"`
	FeeScheduleVersion       int64  `json:"fee_schedule_version"`
	SourceMinorExponent      uint32 `json:"source_minor_exponent"`
	DestinationMinorExponent uint32 `json:"destination_minor_exponent"`
	ExpiresAt                string `json:"expires_at"`
}

type SettlementReadResponse struct {
	SettlementID  string
	InstitutionID string
	TradeID       string
	QuoteID       string
	Status        string
	Beneficiary   string
	AmountMinor   int64
	Currency      string
	Purpose       string
	CorrelationID string
	RequestID     string
}

type BalanceReadResponse struct {
	InstitutionID string
	Balances      []BalanceItem
	RequestID     string
}

type BalanceItem struct {
	Currency       string
	AvailableMinor int64
	HeldMinor      int64
	TotalMinor     int64
}

type TransactionItem struct {
	ID            string
	Type          string
	AmountMinor   int64
	Currency      string
	Status        string
	OccurredAt    time.Time
	CorrelationID string
}

type TransactionPage struct {
	Items     []TransactionItem
	Page      int
	PageSize  int
	Total     int
	RequestID string
}

type CustomerClient interface {
	AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error)
	GetInstitutionAccess(institutionID string) (CustomerAuthContext, error)
	GetInstitution(institutionID string) (CustomerInstitution, error)
	GetWebhookConfiguration(institutionID string) (WebhookConfig, error)
	CreateCredential(institutionID, label, environment string) (*CustomerCredential, string, error)
	RotateCredential(institutionID, publicID string) (*CustomerCredential, string, error)
	RevokeCredential(institutionID, publicID string) error
	GetCredential(institutionID, publicID string) (*CustomerCredential, error)
	ListCredentials(institutionID string) ([]*CustomerCredential, error)
}

type QuoteClient interface {
	CreateQuote(req QuoteRequest) (QuoteResponse, error)
	GetQuote(institutionID, quoteID string) (QuoteResponse, error)
	AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, beneficiaryID string) (QuoteResponse, error)
}

type TradeClient interface {
	GetTrade(institutionID, tradeID string) (TradeReadResponse, error)
	ListTrades(institutionID string, request TradeListRequest) (TradePage, error)
}

type TradeListRequest struct {
	Page              int
	PageSize          int
	From              time.Time
	To                time.Time
	Currency          string
	Status            string
	CustomerReference string
	CorrelationID     string
}

type TradePage struct {
	Items    []TradeReadResponse
	Page     int
	PageSize int
	Total    int
}

type SettlementClient interface {
	GetSettlement(institutionID, settlementID string) (SettlementReadResponse, error)
	ListCustomerSettlementReport(institutionID string, request SettlementReportRequest) (SettlementReportPage, error)
}

type SettlementReportRequest struct {
	Page          int
	PageSize      int
	From          time.Time
	To            time.Time
	Currency      string
	Status        string
	CorrelationID string
}

type SettlementReportItem struct {
	SettlementID string
	TradeID      string
	QuoteID      string
	Status       string
	AmountMinor  int64
	Currency     string
	OccurredAt   time.Time
}

type SettlementReportPage struct {
	Items    []SettlementReportItem
	Page     int
	PageSize int
	Total    int
}

type ReadModelClient interface {
	GetBalances(institutionID string) (BalanceReadResponse, error)
	GetTransactions(institutionID string, page, pageSize int) (TransactionPage, error)
}

type WebhookConfigClient interface {
	ConfigureWebhook(ctx context.Context, institutionID, endpointURL, signingSecret string, eventTypes []string) error
}

type WebhookReplayClient interface {
	ReplayWebhook(ctx context.Context, institutionID, eventID, idempotencyKey string) error
}

type Gateway struct {
	quoteClient           QuoteClient
	tradeClient           TradeClient
	settlementClient      SettlementClient
	readModelClient       ReadModelClient
	customerClient        CustomerClient
	webhookClient         WebhookConfigClient
	webhookReplayClient   WebhookReplayClient
	providerStatusClient  ProviderStatusClient
	idempotency           *idempotencyStore
	rateLimiter           *rateLimiterStore // per-institution token bucket
	credentialEnvironment string
}

func (g *Gateway) SetCustomerClient(cc CustomerClient) {
	if cc != nil {
		g.customerClient = cc
	}
}

func (g *Gateway) SetCredentialEnvironment(environment string) error {
	environment = strings.TrimSpace(environment)
	if environment != "sandbox" && environment != "production" {
		return errors.New("credential environment must be sandbox or production")
	}
	g.credentialEnvironment = environment
	return nil
}

func (g *Gateway) SetQuoteService(qc QuoteClient) {
	if qc != nil {
		g.quoteClient = qc
	}
}

func (g *Gateway) SetTradeClient(tc TradeClient) {
	if tc != nil {
		g.tradeClient = tc
	}
}

func (g *Gateway) SetSettlementClient(sc SettlementClient) {
	if sc != nil {
		g.settlementClient = sc
	}
}

func (g *Gateway) SetReadModelClient(rc ReadModelClient) {
	if rc != nil {
		g.readModelClient = rc
	}
}

func (g *Gateway) SetWebhookConfigClient(client WebhookConfigClient) {
	if client != nil {
		g.webhookClient = client
	}
}

func (g *Gateway) SetWebhookReplayClient(client WebhookReplayClient) {
	if client != nil {
		g.webhookReplayClient = client
	}
}

func (g *Gateway) SetProviderStatusClient(client ProviderStatusClient) {
	g.providerStatusClient = client
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := strings.TrimSpace(r.Header.Get("X-Osai-Correlation-Id"))
	if requestID == "" {
		requestID = strings.TrimSpace(r.Header.Get("X-Request-Id"))
	}
	if requestID == "" {
		requestID = "req_" + uuid.NewString()
	}
	ctx := correlation.WithContext(r.Context(), requestID)
	r = r.WithContext(ctx)
	w.Header().Set("X-Osai-Correlation-Id", requestID)
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("Content-Type", "application/json")

	if r.URL.Path == "/v1/health" {
		writeHTTPJSON(w, http.StatusOK, map[string]any{"status": "ok", "request_id": requestID})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("NOT_FOUND", "route not found", requestID))
		return
	}
	if g.customerClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer service unavailable", requestID))
		return
	}

	customerID, err := g.authenticate(r)
	if err != nil {
		if errors.Is(err, errCustomerAuthUnavailable) {
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer authentication service unavailable", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusUnauthorized, errorEnvelope("UNAUTHENTICATED", "missing or invalid credentials", requestID))
		return
	}
	authCtx, err := g.customerClient.GetInstitutionAccess(customerID)
	if err != nil || authCtx.InstitutionStatus != "ACTIVE" || !authCtx.APIEnabled {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("CUSTOMER_DISABLED", "customer disabled", requestID))
		return
	}

	// Rate limiting
	credentialID := authenticatedCredentialID(r.Header.Get("Authorization"))
	if g.rateLimiter != nil && !g.rateLimiter.allow("institution:"+customerID, "credential:"+credentialID) {
		w.Header().Set("Retry-After", "1")
		writeHTTPJSON(w, http.StatusTooManyRequests, errorEnvelope("RATE_LIMIT_EXCEEDED", "too many requests", requestID))
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/v1/webhook-endpoints" &&
		!hasEntitlement(authCtx.Entitlements, "webhooks:write") {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "webhook configuration is not entitled", requestID))
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/providers/status" &&
		!hasEntitlement(authCtx.Entitlements, "providers:read") {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "provider status is not entitled", requestID))
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/webhook-deliveries/") &&
		strings.HasSuffix(r.URL.Path, "/replay") && !hasEntitlement(authCtx.Entitlements, "webhooks:write") {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "webhook replay is not entitled", requestID))
		return
	}
	if entitlement := quoteRouteEntitlement(r.Method, r.URL.Path); entitlement != "" &&
		!hasEntitlement(authCtx.Entitlements, entitlement) {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "quote operation is not entitled", requestID))
		return
	}
	if entitlement := customerRouteEntitlement(r.Method, r.URL.Path); entitlement != "" &&
		!hasEntitlement(authCtx.Entitlements, entitlement) {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "operation is not entitled", requestID))
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/credentials" &&
		!hasEntitlement(authCtx.Entitlements, "credentials:write") {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "credential creation is not entitled", requestID))
		return
	}
	if (r.Method == http.MethodPost || r.Method == http.MethodDelete) && strings.HasPrefix(r.URL.Path, "/v1/credentials/") &&
		!hasEntitlement(authCtx.Entitlements, "credentials:write") {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "credential operation is not entitled", requestID))
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/credentials" || strings.HasPrefix(r.URL.Path, "/v1/credentials/")) &&
		!hasEntitlement(authCtx.Entitlements, "credentials:read") {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "credential read is not entitled", requestID))
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/quotes/health":
		writeHTTPJSON(w, http.StatusOK, map[string]any{"status": "ok", "request_id": requestID, "latency_ms": time.Since(start).Milliseconds()})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/providers/status":
		g.handleProviderStatus(w, r, requestID)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/beneficiaries":
		g.handleRegisterBeneficiary(w, r, customerID, requestID)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/webhook-endpoints":
		g.handleConfigureWebhook(w, r, customerID, requestID)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/webhook-deliveries/") && strings.HasSuffix(r.URL.Path, "/replay"):
		g.handleReplayWebhook(w, r, customerID, requestID)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/quotes":
		g.handleCreateQuote(w, r, customerID, requestID)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/quotes/") && strings.HasSuffix(r.URL.Path, "/accept"):
		g.handleAcceptQuote(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/quotes/"):
		g.handleGetQuote(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/trades":
		g.handleListTrades(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/trades/"):
		g.handleGetTrade(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/settlements/"):
		g.handleGetSettlement(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/reports/settlements":
		g.handleSettlementReport(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/balances":
		g.handleGetBalances(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/transactions":
		g.handleGetTransactions(w, r, customerID, requestID)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials":
		g.handleCreateCredential(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/credentials":
		g.handleListCredentials(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/credentials/"):
		g.handleGetCredential(w, r, customerID, requestID)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/credentials/") && strings.HasSuffix(r.URL.Path, "/rotate"):
		g.handleRotateCredential(w, r, customerID, requestID)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/credentials/"):
		g.handleRevokeCredential(w, r, customerID, requestID)
	default:
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("NOT_FOUND", "route not found", requestID))
	}
}

func quoteRouteEntitlement(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/v1/quotes":
		return "quotes:write"
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/quotes/") && strings.HasSuffix(path, "/accept"):
		return "quotes:write"
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/quotes/") && path != "/v1/quotes/health":
		return "quotes:read"
	default:
		return ""
	}
}

func customerRouteEntitlement(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/v1/beneficiaries":
		return "beneficiaries:write"
	case method == http.MethodGet && (path == "/v1/trades" || strings.HasPrefix(path, "/v1/trades/")):
		return "trades:read"
	case method == http.MethodGet && strings.HasPrefix(path, "/v1/settlements/"):
		return "settlements:read"
	case method == http.MethodGet && path == "/v1/reports/settlements":
		return "reports:read"
	case method == http.MethodGet && path == "/v1/balances":
		return "balances:read"
	case method == http.MethodGet && path == "/v1/transactions":
		return "transactions:read"
	default:
		return ""
	}
}

func hasEntitlement(entitlements []string, required string) bool {
	for _, entitlement := range entitlements {
		if entitlement == required {
			return true
		}
	}
	return false
}

func (g *Gateway) handleConfigureWebhook(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.webhookClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "webhook configuration service unavailable", requestID))
		return
	}
	if g.idempotency == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "missing Idempotency-Key", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed request body", requestID))
		return
	}
	var payload struct {
		EndpointURL          string   `json:"endpoint_url"`
		SigningSecret        string   `json:"signing_secret"`
		SubscribedEventTypes []string `json:"subscribed_event_types"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed JSON", requestID))
		return
	}
	payload.EndpointURL = strings.TrimSpace(payload.EndpointURL)
	payload.SigningSecret = strings.TrimSpace(payload.SigningSecret)
	for i := range payload.SubscribedEventTypes {
		payload.SubscribedEventTypes[i] = strings.TrimSpace(payload.SubscribedEventTypes[i])
	}
	if payload.EndpointURL == "" || payload.SigningSecret == "" || len(payload.SubscribedEventTypes) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "endpoint_url, signing_secret, and subscribed_event_types are required", requestID))
		return
	}
	replayed, responseCode, response, lockToken, err := g.idempotency.CheckAndStore(
		customerID,
		"/v1/webhook-endpoints",
		idempotencyKey,
		payload,
		http.StatusOK,
		nil,
	)
	if err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
			return
		}
		if errors.Is(err, errIdempotencyInProgress) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_IN_PROGRESS", "request with this idempotency key is still in progress", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		return
	}
	if replayed {
		writeHTTPJSON(w, responseCode, response)
		return
	}
	if err := g.webhookClient.ConfigureWebhook(r.Context(), customerID, payload.EndpointURL, payload.SigningSecret, payload.SubscribedEventTypes); err != nil {
		if errors.Is(err, errInvalidWebhookConfiguration) {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "invalid webhook configuration", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "webhook configuration could not be persisted", requestID))
		return
	}
	response = map[string]any{"configured": true, "request_id": requestID}
	if err := g.idempotency.SaveResponse(customerID, "/v1/webhook-endpoints", idempotencyKey, payload, lockToken, http.StatusOK, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "webhook configuration result could not be stored", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, response)
}

func (g *Gateway) handleReplayWebhook(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.webhookReplayClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "webhook replay service unavailable", requestID))
		return
	}
	if g.idempotency == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/webhook-deliveries/")
	if !strings.HasSuffix(path, "/replay") {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("NOT_FOUND", "webhook delivery not found", requestID))
		return
	}
	eventID := strings.TrimSuffix(path, "/replay")
	eventID = strings.TrimSuffix(eventID, "/")
	if strings.TrimSpace(eventID) == "" || strings.Contains(eventID, "/") {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("NOT_FOUND", "webhook delivery not found", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) != 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "replay request body must be empty", requestID))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "missing Idempotency-Key", requestID))
		return
	}
	endpoint := "/v1/webhook-deliveries/" + eventID + "/replay"
	payload := map[string]string{"event_id": eventID}
	replayed, responseCode, response, lockToken, err := g.idempotency.CheckAndStore(customerID, endpoint, idempotencyKey, payload, http.StatusAccepted, nil)
	if err != nil {
		switch {
		case errors.Is(err, errIdempotencyConflict):
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
		case errors.Is(err, errIdempotencyInProgress):
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_IN_PROGRESS", "request with this idempotency key is still in progress", requestID))
		default:
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		}
		return
	}
	if replayed {
		writeHTTPJSON(w, responseCode, response)
		return
	}
	if err := g.webhookReplayClient.ReplayWebhook(r.Context(), customerID, eventID, idempotencyKey); err != nil {
		switch {
		case errors.Is(err, errWebhookEventNotFound):
			writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("WEBHOOK_EVENT_NOT_FOUND", "webhook event not found", requestID))
		case errors.Is(err, errWebhookReplayNotAllowed):
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("WEBHOOK_REPLAY_NOT_ALLOWED", "webhook event is not eligible for replay", requestID))
		default:
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "webhook replay could not be persisted", requestID))
		}
		return
	}
	response = map[string]any{"event_id": eventID, "status": "QUEUED", "request_id": requestID}
	if err := g.idempotency.SaveResponse(customerID, endpoint, idempotencyKey, payload, lockToken, http.StatusAccepted, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "webhook replay result could not be stored", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusAccepted, response)
}

func (g *Gateway) authenticate(r *http.Request) (string, error) {
	if g.customerClient == nil {
		return "", errCustomerAuthUnavailable
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return "", errors.New("missing credentials")
	}
	if !strings.HasPrefix(auth, "ApiKey ") {
		return "", errors.New("invalid credentials")
	}
	parts := strings.SplitN(strings.TrimPrefix(auth, "ApiKey "), ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("invalid credentials")
	}
	ctx, err := g.customerClient.AuthenticateAPIClient(parts[0], parts[1])
	if err != nil {
		if grpcCode := status.Code(err); grpcCode != codes.Unknown && grpcCode != codes.Unauthenticated {
			return "", errCustomerAuthUnavailable
		}
		return "", err
	}
	if ctx.InstitutionID == "" {
		return "", errors.New("invalid credentials")
	}
	if g.credentialEnvironment != "" && ctx.Environment != g.credentialEnvironment {
		return "", errors.New("invalid credentials")
	}
	return ctx.InstitutionID, nil
}

func (g *Gateway) authenticateHeader(authHeader string) (string, error) {
	if g.customerClient == nil {
		return "", errors.New("customer service unavailable")
	}
	if authHeader == "" {
		return "", errors.New("missing credentials")
	}
	if !strings.HasPrefix(authHeader, "ApiKey ") {
		return "", errors.New("invalid credentials")
	}
	parts := strings.SplitN(strings.TrimPrefix(authHeader, "ApiKey "), ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("invalid credentials")
	}
	ctx, err := g.customerClient.AuthenticateAPIClient(parts[0], parts[1])
	if err != nil {
		return "", err
	}
	if ctx.InstitutionID == "" {
		return "", errors.New("invalid credentials")
	}
	return ctx.InstitutionID, nil
}

func (g *Gateway) handleRegisterBeneficiary(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	registrar, ok := g.customerClient.(interface {
		RegisterBeneficiary(context.Context, string, string, string, string, string) (map[string]any, error)
	})
	if !ok {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer beneficiary service unavailable", requestID))
		return
	}
	if g.idempotency == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "missing Idempotency-Key", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed request body", requestID))
		return
	}
	var payload struct {
		Name          string `json:"name"`
		BankCode      string `json:"bank_code"`
		AccountNumber string `json:"account_number"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed JSON", requestID))
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	payload.BankCode = strings.TrimSpace(payload.BankCode)
	payload.AccountNumber = strings.TrimSpace(payload.AccountNumber)
	if payload.Name == "" || !customerDigits(payload.BankCode, 3, 6) || !customerDigits(payload.AccountNumber, 10, 10) {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "invalid beneficiary details", requestID))
		return
	}
	replayed, responseCode, response, lockToken, err := g.idempotency.CheckAndStore(customerID, "/v1/beneficiaries", idempotencyKey, payload, http.StatusAccepted, nil)
	if err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
			return
		}
		if errors.Is(err, errIdempotencyInProgress) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_IN_PROGRESS", "request with this idempotency key is still in progress", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		return
	}
	if replayed {
		writeHTTPJSON(w, responseCode, response)
		return
	}
	response, err = registrar.RegisterBeneficiary(r.Context(), customerID, payload.Name, payload.BankCode, payload.AccountNumber, idempotencyKey)
	if err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "beneficiary registration unavailable", requestID))
		return
	}
	response["request_id"] = requestID
	if err := g.idempotency.SaveResponse(customerID, "/v1/beneficiaries", idempotencyKey, payload, lockToken, http.StatusAccepted, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "beneficiary registration result could not be stored", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusAccepted, response)
}

func customerDigits(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func decodeJSONMap(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return payload, nil
}

func (g *Gateway) handleCreateQuoteForTest(customerID, idempotencyKey string, payload map[string]any) (map[string]any, error) {
	if g.quoteClient == nil {
		return nil, errors.New("quote service unavailable")
	}
	var amount int64
	switch v := payload["base_amount_minor"].(type) {
	case float64:
		amount = int64(v)
	case float32:
		amount = int64(v)
	case int:
		amount = int64(v)
	case int64:
		amount = v
	case int32:
		amount = int64(v)
	default:
		return nil, errors.New("base_amount_minor must be a positive integer")
	}
	if amount <= 0 {
		return nil, errors.New("base_amount_minor must be a positive integer")
	}
	baseCurrency := strings.TrimSpace(fmt.Sprint(payload["base_currency"]))
	quoteCurrency := strings.TrimSpace(fmt.Sprint(payload["quote_currency"]))
	if baseCurrency == "" || quoteCurrency == "" {
		return nil, errors.New("base_currency and quote_currency are required")
	}
	if !strings.EqualFold(baseCurrency, "NGN") && !strings.EqualFold(baseCurrency, "USD") {
		return nil, errors.New("unsupported source currency")
	}
	if !strings.EqualFold(quoteCurrency, "USD") && !strings.EqualFold(quoteCurrency, "NGN") {
		return nil, errors.New("unsupported destination currency")
	}
	quoteReq := QuoteRequest{InstitutionID: customerID, BaseAmountMinor: int64(amount), BaseCurrency: baseCurrency, QuoteCurrency: quoteCurrency, DestinationRail: strings.TrimSpace(fmt.Sprint(payload["destination_rail"])), Urgency: strings.TrimSpace(fmt.Sprint(payload["urgency"])), IdempotencyKey: idempotencyKey, CorrelationID: "req-test"}
	if quoteReq.DestinationRail == "" {
		quoteReq.DestinationRail = "sim_lp_1"
	}
	if quoteReq.Urgency == "" {
		quoteReq.Urgency = "normal"
	}
	quote, err := g.quoteClient.CreateQuote(quoteReq)
	if err != nil {
		return nil, err
	}
	return quoteResponsePayload(quote, "req-test"), nil
}

func (g *Gateway) handleCreateQuote(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.quoteClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "quote service unavailable", requestID))
		return
	}
	if r.Method != http.MethodPost {
		writeHTTPJSON(w, http.StatusMethodNotAllowed, errorEnvelope("VALIDATION_ERROR", "method not allowed", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed request body", requestID))
		return
	}
	payload, err := decodeJSONMap(body)
	if err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed JSON", requestID))
		return
	}
	quoteMode, _ := payload["quote_mode"].(string)
	quoteMode = strings.ToUpper(strings.TrimSpace(quoteMode))
	if quoteMode == "" {
		quoteMode = "FIXED_SOURCE"
	}
	if quoteMode != "FIXED_SOURCE" && quoteMode != "FIXED_DESTINATION" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "quote_mode must be FIXED_SOURCE or FIXED_DESTINATION", requestID))
		return
	}
	payload["quote_mode"] = quoteMode
	var sourceAmount, destinationAmount int64
	if quoteMode == "FIXED_SOURCE" {
		amountValue, ok := payload["base_amount_minor"].(json.Number)
		if !ok {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "base_amount_minor must be a positive integer for FIXED_SOURCE", requestID))
			return
		}
		sourceAmount, err = amountValue.Int64()
		if err != nil || sourceAmount <= 0 || payload["destination_amount_minor"] != nil {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "FIXED_SOURCE requires only a positive base_amount_minor", requestID))
			return
		}
	} else {
		amountValue, ok := payload["destination_amount_minor"].(json.Number)
		if !ok {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "destination_amount_minor must be a positive integer for FIXED_DESTINATION", requestID))
			return
		}
		destinationAmount, err = amountValue.Int64()
		if err != nil || destinationAmount <= 0 || payload["base_amount_minor"] != nil {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "FIXED_DESTINATION requires only a positive destination_amount_minor", requestID))
			return
		}
	}
	baseCurrency := strings.TrimSpace(fmt.Sprint(payload["base_currency"]))
	quoteCurrency := strings.TrimSpace(fmt.Sprint(payload["quote_currency"]))
	if baseCurrency == "" || quoteCurrency == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "base_currency and quote_currency are required", requestID))
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "missing Idempotency-Key", requestID))
		return
	}
	if !strings.EqualFold(baseCurrency, "NGN") && !strings.EqualFold(baseCurrency, "USD") {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("UNSUPPORTED_CORRIDOR", "unsupported source currency", requestID))
		return
	}
	if !strings.EqualFold(quoteCurrency, "USD") && !strings.EqualFold(quoteCurrency, "NGN") {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("UNSUPPORTED_CORRIDOR", "unsupported destination currency", requestID))
		return
	}
	replayed, responseCode, response, lockToken, err := g.idempotency.CheckAndStore(customerID, "/v1/quotes", idempotencyKey, payload, http.StatusOK, nil)
	if err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
		} else if errors.Is(err, errIdempotencyInProgress) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_IN_PROGRESS", "request with this idempotency key is still in progress", requestID))
		} else {
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		}
		return
	}
	if replayed {
		writeHTTPJSON(w, responseCode, response)
		return
	}
	customerReference, _ := payload["customer_reference"].(string)
	quoteReq := QuoteRequest{InstitutionID: customerID, QuoteMode: quoteMode, BaseAmountMinor: sourceAmount, DestinationAmountMinor: destinationAmount, BaseCurrency: baseCurrency, QuoteCurrency: quoteCurrency, DestinationRail: strings.TrimSpace(fmt.Sprint(payload["destination_rail"])), CustomerReference: strings.TrimSpace(customerReference), Urgency: strings.TrimSpace(fmt.Sprint(payload["urgency"])), IdempotencyKey: idempotencyKey, CorrelationID: requestID}
	if quoteReq.DestinationRail == "" {
		quoteReq.DestinationRail = "sim_lp_1"
	}
	if quoteReq.Urgency == "" {
		quoteReq.Urgency = "normal"
	}
	quote, err := g.quoteClient.CreateQuote(quoteReq)
	if err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", err.Error(), requestID))
		return
	}
	response = quoteResponsePayload(quote, requestID)
	if err := g.idempotency.SaveResponse(customerID, "/v1/quotes", idempotencyKey, payload, lockToken, http.StatusOK, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "quote result could not be stored", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, response)
}

func quoteResponsePayload(quote QuoteResponse, requestID string) map[string]any {
	return map[string]any{
		"quote_id": quote.QuoteID, "status": quote.Status,
		"base_amount_minor": quote.BaseAmountMinor, "base_currency": quote.BaseCurrency,
		"quote_currency": quote.QuoteCurrency, "destination_rail": quote.DestinationRail,
		"amount_out_minor": quote.AmountOutMinor, "rate_minor": quote.RateMinor, "fee_minor": quote.FeeMinor,
		"quote_mode": quote.QuoteMode, "source_principal_minor": quote.SourcePrincipalMinor,
		"total_debit_minor": quote.TotalDebitMinor, "destination_target_minor": quote.DestinationTargetMinor,
		"total_cost_minor": quote.TotalCostMinor, "total_cost_currency": quote.BaseCurrency,
		"destination_surplus_minor": quote.DestinationSurplusMinor, "rate_direction": quote.RateDirection,
		"rate_scale": quote.RateScale, "fee_schedule_id": quote.FeeScheduleID,
		"fee_schedule_version":  quote.FeeScheduleVersion,
		"source_minor_exponent": quote.SourceMinorExponent, "destination_minor_exponent": quote.DestinationMinorExponent,
		"sell_amount_minor": quote.BaseAmountMinor, "sell_currency": quote.BaseCurrency,
		"buy_amount_minor": quote.AmountOutMinor, "buy_currency": quote.QuoteCurrency,
		"expires_at": quote.ExpiresAt.UTC().Format(time.RFC3339), "request_id": requestID,
	}
}

func (g *Gateway) handleAcceptQuote(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.quoteClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "quote service unavailable", requestID))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/quotes/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "accept" {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("QUOTE_NOT_FOUND", "quote path not found", requestID))
		return
	}
	quoteID := parts[0]
	idKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idKey == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "missing Idempotency-Key", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed request body", requestID))
		return
	}
	payload, err := decodeJSONMap(body)
	if err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed JSON", requestID))
		return
	}
	if quoteID != "" {
		if qid, ok := payload["quote_id"].(string); ok && qid != "" && qid != quoteID {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "quote_id mismatch", requestID))
			return
		}
		payload["quote_id"] = quoteID
	}
	payload["institution_id"] = customerID
	endpoint := "/v1/quotes/" + quoteID + "/accept"
	replayed, responseCode, response, lockToken, err := g.idempotency.CheckAndStore(customerID, endpoint, idKey, payload, http.StatusOK, nil)
	if err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
		} else if errors.Is(err, errIdempotencyInProgress) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_IN_PROGRESS", "request with this idempotency key is still in progress", requestID))
		} else {
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "idempotency service unavailable", requestID))
		}
		return
	}
	if replayed {
		writeHTTPJSON(w, responseCode, response)
		return
	}
	beneficiaryID, _ := payload["beneficiary_id"].(string)
	quote, err := g.quoteClient.AcceptQuote(customerID, quoteID, idKey, requestID, strings.TrimSpace(beneficiaryID))
	if err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", err.Error(), requestID))
		return
	}
	response = map[string]any{"quote_id": quote.QuoteID, "trade_id": quote.TradeID, "status": quote.Status, "request_id": requestID}
	if err := g.idempotency.SaveResponse(customerID, endpoint, idKey, payload, lockToken, http.StatusOK, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "quote acceptance result could not be stored", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, response)
}

func (g *Gateway) handleGetQuote(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.quoteClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "quote service unavailable", requestID))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/quotes/")
	if strings.Contains(path, "/") {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("QUOTE_NOT_FOUND", "quote not found", requestID))
		return
	}
	quote, err := g.quoteClient.GetQuote(customerID, path)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("QUOTE_NOT_FOUND", "quote not found", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "no access to quote", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, quoteResponsePayload(quote, requestID))
}

func (g *Gateway) handleGetTrade(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/trades/")
	if strings.Contains(path, "/") || strings.TrimSpace(path) == "" {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("TRADE_NOT_FOUND", "trade not found", requestID))
		return
	}
	if g.tradeClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "trade read service unavailable", requestID))
		return
	}
	trade, err := g.tradeClient.GetTrade(customerID, path)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("TRADE_NOT_FOUND", "trade not found", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "no access to trade", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, tradeResponsePayload(trade, requestID))
}

func (g *Gateway) handleListTrades(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.tradeClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "trade read service unavailable", requestID))
		return
	}
	query := r.URL.Query()
	request := TradeListRequest{Page: 1, PageSize: 25}
	if value := query.Get("page"); value != "" {
		page, err := strconv.Atoi(value)
		if err != nil || page < 1 {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "page must be a positive integer", requestID))
			return
		}
		request.Page = page
	}
	if value := query.Get("page_size"); value != "" {
		pageSize, err := strconv.Atoi(value)
		if err != nil || pageSize < 1 || pageSize > 100 {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "page_size must be between 1 and 100", requestID))
			return
		}
		request.PageSize = pageSize
	}
	var err error
	if value := query.Get("from"); value != "" {
		request.From, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "from must be RFC3339", requestID))
			return
		}
	}
	if value := query.Get("to"); value != "" {
		request.To, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "to must be RFC3339", requestID))
			return
		}
	}
	if !request.From.IsZero() && !request.To.IsZero() && !request.From.Before(request.To) {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "from must be earlier than to", requestID))
		return
	}
	request.Currency = strings.TrimSpace(query.Get("currency"))
	request.Status = strings.TrimSpace(query.Get("status"))
	request.CorrelationID = requestID
	request.CustomerReference = strings.TrimSpace(query.Get("customer_reference"))
	request.CorrelationID = requestID
	page, err := g.tradeClient.ListTrades(customerID, request)
	if err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "trade list unavailable", requestID))
		return
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, trade := range page.Items {
		items = append(items, tradeResponsePayload(trade, requestID))
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{"items": items, "page": page.Page, "page_size": page.PageSize, "total": page.Total, "request_id": requestID})
}

func tradeResponsePayload(trade TradeReadResponse, requestID string) map[string]any {
	payload := map[string]any{"trade_id": trade.TradeID, "quote_id": trade.QuoteID, "institution_id": trade.InstitutionID, "status": trade.Status, "base_amount_minor": trade.BaseAmountMinor, "base_currency": trade.BaseCurrency, "quote_currency": trade.QuoteCurrency, "settlement_id": trade.SettlementID, "request_id": requestID}
	if trade.PricingSnapshot != nil {
		payload["pricing_snapshot"] = trade.PricingSnapshot
	}
	if trade.FXExecutionID != "" {
		payload["fx_execution"] = map[string]string{
			"execution_id": trade.FXExecutionID, "executor_id": trade.FXExecutorID,
			"request_reference": trade.FXExecutionRef, "status": trade.FXExecutionStatus,
		}
	}
	if trade.CustomerReference != "" {
		payload["customer_reference"] = trade.CustomerReference
	}
	return payload
}

func (g *Gateway) handleGetSettlement(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/settlements/")
	if strings.Contains(path, "/") || strings.TrimSpace(path) == "" {
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("SETTLEMENT_NOT_FOUND", "settlement not found", requestID))
		return
	}
	if g.settlementClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "settlement read service unavailable", requestID))
		return
	}
	settlement, err := g.settlementClient.GetSettlement(customerID, path)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("SETTLEMENT_NOT_FOUND", "settlement not found", requestID))
			return
		}
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "no access to settlement", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{"settlement_id": settlement.SettlementID, "trade_id": settlement.TradeID, "quote_id": settlement.QuoteID, "institution_id": settlement.InstitutionID, "status": settlement.Status, "beneficiary": settlement.Beneficiary, "amount_minor": settlement.AmountMinor, "currency": settlement.Currency, "purpose": settlement.Purpose, "request_id": requestID})
}

func (g *Gateway) handleSettlementReport(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.settlementClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "settlement report service unavailable", requestID))
		return
	}
	query := r.URL.Query()
	request := SettlementReportRequest{Page: 1, PageSize: 25}
	if value := query.Get("page"); value != "" {
		page, err := strconv.Atoi(value)
		if err != nil || page < 1 {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "page must be a positive integer", requestID))
			return
		}
		request.Page = page
	}
	if value := query.Get("page_size"); value != "" {
		pageSize, err := strconv.Atoi(value)
		if err != nil || pageSize < 1 || pageSize > 100 {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "page_size must be between 1 and 100", requestID))
			return
		}
		request.PageSize = pageSize
	}
	var err error
	if value := query.Get("from"); value != "" {
		request.From, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "from must be RFC3339", requestID))
			return
		}
	}
	if value := query.Get("to"); value != "" {
		request.To, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "to must be RFC3339", requestID))
			return
		}
	}
	if !request.From.IsZero() && !request.To.IsZero() && !request.From.Before(request.To) {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "from must be earlier than to", requestID))
		return
	}
	request.Currency = strings.TrimSpace(query.Get("currency"))
	request.Status = strings.TrimSpace(query.Get("status"))
	if request.Status != "" && request.Status != "CONFIRMED" && request.Status != "FAILED" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "status must be CONFIRMED or FAILED", requestID))
		return
	}
	page, err := g.settlementClient.ListCustomerSettlementReport(customerID, request)
	if err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "settlement report unavailable", requestID))
		return
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, map[string]any{
			"settlement_id": item.SettlementID, "trade_id": item.TradeID, "quote_id": item.QuoteID,
			"status": item.Status, "amount_minor": item.AmountMinor, "currency": item.Currency,
			"occurred_at": item.OccurredAt.UTC().Format(time.RFC3339),
		})
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{"items": items, "page": page.Page, "page_size": page.PageSize, "total": page.Total, "request_id": requestID})
}

func (g *Gateway) handleGetBalances(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.readModelClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "balance read service unavailable", requestID))
		return
	}
	balances, err := g.readModelClient.GetBalances(customerID)
	if err != nil {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "no access to balances", requestID))
		return
	}
	items := make([]map[string]any, 0, len(balances.Balances))
	for _, balance := range balances.Balances {
		items = append(items, map[string]any{
			"currency":        balance.Currency,
			"available_minor": balance.AvailableMinor,
			"held_minor":      balance.HeldMinor,
			"total_minor":     balance.TotalMinor,
		})
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{"institution_id": balances.InstitutionID, "balances": items, "request_id": requestID})
}

func (g *Gateway) handleGetTransactions(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.readModelClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("READ_SERVICE_UNAVAILABLE", "transaction read service unavailable", requestID))
		return
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page = atoiDefault(p, 1)
	}
	pageSize := 25
	if s := r.URL.Query().Get("page_size"); s != "" {
		pageSize = atoiDefault(s, 25)
	}
	items, err := g.readModelClient.GetTransactions(customerID, page, pageSize)
	if err != nil {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "no access to transactions", requestID))
		return
	}
	payload := map[string]any{"items": make([]map[string]any, 0, len(items.Items)), "page": items.Page, "page_size": items.PageSize, "total": items.Total, "request_id": requestID}
	for _, item := range items.Items {
		payload["items"] = append(payload["items"].([]map[string]any), map[string]any{"transaction_id": item.ID, "type": item.Type, "amount_minor": item.AmountMinor, "currency": item.Currency, "status": item.Status, "occurred_at": item.OccurredAt.UTC().Format(time.RFC3339), "correlation_id": item.CorrelationID})
	}
	writeHTTPJSON(w, http.StatusOK, payload)
}

func (g *Gateway) handleCreateCredential(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.customerClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer service unavailable", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed request body", requestID))
		return
	}
	var payload struct {
		Label       string `json:"label"`
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "invalid json", requestID))
		return
	}
	if payload.Label == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "label is required", requestID))
		return
	}
	if payload.Environment == "" {
		payload.Environment = "sandbox"
	}
	if payload.Environment != "sandbox" && payload.Environment != "production" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "environment must be 'sandbox' or 'production'", requestID))
		return
	}

	cred, secret, err := g.customerClient.CreateCredential(customerID, payload.Label, payload.Environment)
	if err != nil {
		writeCustomerOperationError(w, err, requestID)
		return
	}

	writeHTTPJSON(w, http.StatusCreated, map[string]any{
		"public_id":   cred.PublicID,
		"secret":      secret,
		"label":       cred.Label,
		"environment": cred.Environment,
		"created_at":  cred.CreatedAt.UTC().Format(time.RFC3339),
		"request_id":  requestID,
	})
}

func (g *Gateway) handleListCredentials(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.customerClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer service unavailable", requestID))
		return
	}

	credentials, err := g.customerClient.ListCredentials(customerID)
	if err != nil {
		writeCustomerOperationError(w, err, requestID)
		return
	}

	items := make([]map[string]any, 0, len(credentials))
	for _, cred := range credentials {
		items = append(items, map[string]any{
			"public_id":   cred.PublicID,
			"label":       cred.Label,
			"environment": cred.Environment,
			"created_at":  cred.CreatedAt.UTC().Format(time.RFC3339),
			"status":      cred.Status,
		})
	}

	writeHTTPJSON(w, http.StatusOK, map[string]any{
		"credentials": items,
		"request_id":  requestID,
	})
}

func (g *Gateway) handleGetCredential(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.customerClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer service unavailable", requestID))
		return
	}

	publicID := strings.TrimPrefix(r.URL.Path, "/v1/credentials/")
	cred, err := g.customerClient.GetCredential(customerID, publicID)
	if err != nil {
		writeCustomerOperationError(w, err, requestID)
		return
	}

	writeHTTPJSON(w, http.StatusOK, map[string]any{
		"public_id":   cred.PublicID,
		"label":       cred.Label,
		"environment": cred.Environment,
		"created_at":  cred.CreatedAt.UTC().Format(time.RFC3339),
		"status":      cred.Status,
		"request_id":  requestID,
	})
}

func (g *Gateway) handleRotateCredential(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.customerClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer service unavailable", requestID))
		return
	}

	publicID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/credentials/"), "/rotate")
	cred, newSecret, err := g.customerClient.RotateCredential(customerID, publicID)
	if err != nil {
		writeCustomerOperationError(w, err, requestID)
		return
	}

	writeHTTPJSON(w, http.StatusOK, map[string]any{
		"public_id":   cred.PublicID,
		"secret":      newSecret,
		"label":       cred.Label,
		"environment": cred.Environment,
		"created_at":  cred.CreatedAt.UTC().Format(time.RFC3339),
		"rotated_at":  time.Now().UTC().Format(time.RFC3339),
		"request_id":  requestID,
	})
}

func (g *Gateway) handleRevokeCredential(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if g.customerClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer service unavailable", requestID))
		return
	}

	publicID := strings.TrimPrefix(r.URL.Path, "/v1/credentials/")
	err := g.customerClient.RevokeCredential(customerID, publicID)
	if err != nil {
		writeCustomerOperationError(w, err, requestID)
		return
	}

	writeHTTPJSON(w, http.StatusOK, map[string]any{
		"status":     "revoked",
		"request_id": requestID,
	})
}

func writeCustomerOperationError(w http.ResponseWriter, err error, requestID string) {
	switch status.Code(err) {
	case codes.NotFound:
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("NOT_FOUND", "credential not found", requestID))
	case codes.InvalidArgument:
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "invalid credential request", requestID))
	case codes.PermissionDenied:
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("FORBIDDEN", "credential operation is not permitted", requestID))
	default:
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "customer credential service unavailable", requestID))
	}
}

func atoiDefault(v string, defaultValue int) int {
	var parsed int
	if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func authenticatedCredentialID(authorization string) string {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(authorization), "ApiKey "), ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// rateLimiterStore enforces independent token buckets per institution and credential.
// Each scope allows 100 requests per minute with a burst of 10.
type rateLimiterStore struct {
	mu     sync.RWMutex
	tokens map[string]*tokenBucket
}

type tokenBucket struct {
	tokens       float64
	lastRefillAt time.Time
}

const (
	rateLimitPerMinute = 100.0
	rateLimitBurst     = 10.0
	refillInterval     = time.Minute
)

func newRateLimiterStore() *rateLimiterStore {
	return &rateLimiterStore{tokens: make(map[string]*tokenBucket)}
}

func (r *rateLimiterStore) allow(keys ...string) bool {
	if len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if key == "" || strings.HasSuffix(key, ":") {
			return false
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	tokensPerSecond := rateLimitPerMinute / 60.0
	buckets := make([]*tokenBucket, 0, len(keys))
	for _, key := range keys {
		bucket, ok := r.tokens[key]
		if !ok {
			bucket = &tokenBucket{tokens: rateLimitBurst, lastRefillAt: now}
			r.tokens[key] = bucket
		} else {
			elapsedSeconds := now.Sub(bucket.lastRefillAt).Seconds()
			bucket.tokens = math.Min(bucket.tokens+tokensPerSecond*elapsedSeconds, rateLimitBurst)
			bucket.lastRefillAt = now
		}
		buckets = append(buckets, bucket)
	}
	for _, bucket := range buckets {
		if bucket.tokens < 1.0 {
			return false
		}
	}
	for _, bucket := range buckets {
		bucket.tokens--
	}
	return true
}

func errorEnvelope(code, message, requestID string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": requestID}}
}

func writeHTTPJSON(w http.ResponseWriter, code int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func signWithSecret(secret, body string) string {
	sum := sha256.Sum256([]byte(secret + ":" + body))
	return hex.EncodeToString(sum[:])
}

func signAndVerify(secret, body string) bool {
	return hmac.Equal([]byte(signWithSecret(secret, body)), []byte(signWithSecret(secret, body)))
}
