package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/correlation"
)

type CustomerAuthContext struct {
	InstitutionID     string
	InstitutionStatus string
	KYBStatus         string
	APIAccessStatus   string
	APIEnabled        bool
	Entitlements      []string
}

type CustomerInstitution struct {
	ID         string
	Name       string
	Status     string
	KYBStatus  string
	Country    string
	APIEnabled bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type BeneficiaryRegistration struct {
	ID             string
	Status         string
	ApprovalStatus string
}

type beneficiaryRegistrar interface {
	RegisterBeneficiary(institutionID, name, bankCode, accountNumber, idempotencyKey string) (BeneficiaryRegistration, error)
}

type WebhookConfig struct {
	InstitutionID string
	WebhookURL    string
	Secret        string
	Enabled       bool
	Status        string
}

type QuoteRequest struct {
	InstitutionID   string
	BaseAmountMinor int64
	BaseCurrency    string
	QuoteCurrency   string
	DestinationRail string
	Urgency         string
	IdempotencyKey  string
	CorrelationID   string
}

type QuoteResponse struct {
	QuoteID         string
	TradeID         string
	Status          string
	BaseAmountMinor int64
	BaseCurrency    string
	QuoteCurrency   string
	DestinationRail string
	AmountOutMinor  int64
	RateMinor       int64
	FeeMinor        int64
	ExpiresAt       time.Time
	CorrelationID   string
}

type TradeReadResponse struct {
	TradeID         string
	InstitutionID   string
	QuoteID         string
	Status          string
	BaseAmountMinor int64
	BaseCurrency    string
	QuoteCurrency   string
	SettlementID    string
	CorrelationID   string
	RequestID       string
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
	InstitutionID  string
	Currency       string
	AvailableMinor int64
	HeldMinor      int64
	TotalMinor     int64
	RequestID      string
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
}

type QuoteClient interface {
	CreateQuote(req QuoteRequest) (QuoteResponse, error)
	GetQuote(institutionID, quoteID string) (QuoteResponse, error)
	AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, beneficiaryID string) (QuoteResponse, error)
}

type TradeClient interface {
	GetTrade(institutionID, tradeID string) (TradeReadResponse, error)
}

type SettlementClient interface {
	GetSettlement(institutionID, settlementID string) (SettlementReadResponse, error)
}

type ReadModelClient interface {
	GetBalances(institutionID string) (BalanceReadResponse, error)
	GetTransactions(institutionID string, page, pageSize int) (TransactionPage, error)
}

type Gateway struct {
	quoteClient      QuoteClient
	tradeClient      TradeClient
	settlementClient SettlementClient
	readModelClient  ReadModelClient
	customerClient   CustomerClient
	idempotency      *idempotencyStore
}

func (g *Gateway) SetCustomerClient(cc CustomerClient) {
	if cc != nil {
		g.customerClient = cc
	}
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

	customerID, err := g.authenticate(r)
	if err != nil {
		writeHTTPJSON(w, http.StatusUnauthorized, errorEnvelope("UNAUTHENTICATED", err.Error(), requestID))
		return
	}
	if authCtx, err := g.customerClient.GetInstitutionAccess(customerID); err != nil || authCtx.InstitutionStatus != "ACTIVE" || !authCtx.APIEnabled {
		writeHTTPJSON(w, http.StatusForbidden, errorEnvelope("CUSTOMER_DISABLED", "customer disabled", requestID))
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/quotes/health":
		writeHTTPJSON(w, http.StatusOK, map[string]any{"status": "ok", "request_id": requestID, "latency_ms": time.Since(start).Milliseconds()})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/quotes":
		g.handleCreateQuote(w, r, customerID, requestID)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/beneficiaries":
		g.handleCreateBeneficiary(w, r, customerID, requestID)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/quotes/") && strings.HasSuffix(r.URL.Path, "/accept"):
		g.handleAcceptQuote(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/quotes/"):
		g.handleGetQuote(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/trades/"):
		g.handleGetTrade(w, r, customerID, requestID)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/settlements/"):
		g.handleGetSettlement(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/balances":
		g.handleGetBalances(w, r, customerID, requestID)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/transactions":
		g.handleGetTransactions(w, r, customerID, requestID)
	default:
		writeHTTPJSON(w, http.StatusNotFound, errorEnvelope("NOT_FOUND", "route not found", requestID))
	}
}

func (g *Gateway) handleCreateBeneficiary(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	registrar, ok := g.customerClient.(beneficiaryRegistrar)
	if !ok {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("BENEFICIARY_UNAVAILABLE", "beneficiary registration unavailable", requestID))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "missing Idempotency-Key", requestID))
		return
	}
	var body struct {
		Name          string `json:"name"`
		BankCode      string `json:"bank_code"`
		AccountNumber string `json:"account_number"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.BankCode) == "" || strings.TrimSpace(body.AccountNumber) == "" {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "invalid beneficiary details", requestID))
		return
	}
	request := map[string]any{"name": body.Name, "bank_code": body.BankCode, "account_number": body.AccountNumber}
	if replay, stored, err := g.idempotency.CheckAndStore(customerID, "/v1/beneficiaries", key, request, http.StatusAccepted, nil); err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
		} else {
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("IDEMPOTENCY_UNAVAILABLE", "idempotency storage unavailable", requestID))
		}
		return
	} else if replay && stored["beneficiary_id"] != nil {
		writeHTTPJSON(w, http.StatusAccepted, stored)
		return
	}
	beneficiary, err := registrar.RegisterBeneficiary(customerID, body.Name, body.BankCode, body.AccountNumber, key)
	if err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("BENEFICIARY_UNAVAILABLE", "beneficiary registration unavailable", requestID))
		return
	}
	response := map[string]any{"beneficiary_id": beneficiary.ID, "status": beneficiary.Status, "approval_status": beneficiary.ApprovalStatus, "request_id": requestID}
	if err := g.idempotency.SaveResponse(customerID, "/v1/beneficiaries", key, request, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("IDEMPOTENCY_UNAVAILABLE", "idempotency storage unavailable", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusAccepted, response)
}

func (g *Gateway) authenticate(r *http.Request) (string, error) {
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
		return "", err
	}
	if ctx.InstitutionID == "" {
		return "", errors.New("invalid credentials")
	}
	return ctx.InstitutionID, nil
}

func (g *Gateway) authenticateHeader(authHeader string) (string, error) {
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

func (g *Gateway) handleCreateQuoteForTest(customerID, idempotencyKey string, payload map[string]any) (map[string]any, error) {
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
	return map[string]any{"quote_id": quote.QuoteID, "status": quote.Status, "base_amount_minor": quote.BaseAmountMinor, "base_currency": quote.BaseCurrency, "quote_currency": quote.QuoteCurrency, "destination_rail": quote.DestinationRail, "amount_out_minor": quote.AmountOutMinor, "rate_minor": quote.RateMinor, "fee_minor": quote.FeeMinor}, nil
}

func (g *Gateway) handleCreateQuote(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
	if r.Method != http.MethodPost {
		writeHTTPJSON(w, http.StatusMethodNotAllowed, errorEnvelope("VALIDATION_ERROR", "method not allowed", requestID))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed request body", requestID))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "malformed JSON", requestID))
		return
	}
	amount, ok := payload["base_amount_minor"].(float64)
	if !ok || amount <= 0 {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", "base_amount_minor must be a positive integer", requestID))
		return
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
	if replay, stored, err := g.idempotency.CheckAndStore(customerID, "/v1/quotes", idempotencyKey, payload, http.StatusOK, nil); err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
		} else {
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("IDEMPOTENCY_UNAVAILABLE", "idempotency storage unavailable", requestID))
		}
		return
	} else if replay && stored["quote_id"] != nil {
		writeHTTPJSON(w, http.StatusOK, stored)
		return
	}
	quoteReq := QuoteRequest{InstitutionID: customerID, BaseAmountMinor: int64(amount), BaseCurrency: baseCurrency, QuoteCurrency: quoteCurrency, DestinationRail: strings.TrimSpace(fmt.Sprint(payload["destination_rail"])), Urgency: strings.TrimSpace(fmt.Sprint(payload["urgency"])), IdempotencyKey: idempotencyKey, CorrelationID: requestID}
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
	response := map[string]any{"quote_id": quote.QuoteID, "status": quote.Status, "base_amount_minor": quote.BaseAmountMinor, "base_currency": quote.BaseCurrency, "quote_currency": quote.QuoteCurrency, "destination_rail": quote.DestinationRail, "amount_out_minor": quote.AmountOutMinor, "rate_minor": quote.RateMinor, "fee_minor": quote.FeeMinor, "expires_at": quote.ExpiresAt.UTC().Format(time.RFC3339), "request_id": requestID}
	if err := g.idempotency.SaveResponse(customerID, "/v1/quotes", idempotencyKey, payload, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("IDEMPOTENCY_UNAVAILABLE", "idempotency storage unavailable", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, response)
}

func (g *Gateway) handleAcceptQuote(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
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
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
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
	if replay, stored, err := g.idempotency.CheckAndStore(customerID, "/v1/quotes/"+quoteID+"/accept", idKey, payload, http.StatusOK, nil); err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			writeHTTPJSON(w, http.StatusConflict, errorEnvelope("IDEMPOTENCY_CONFLICT", "idempotency key reused with different payload", requestID))
		} else {
			writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("IDEMPOTENCY_UNAVAILABLE", "idempotency storage unavailable", requestID))
		}
		return
	} else if replay && stored["trade_id"] != nil {
		writeHTTPJSON(w, http.StatusOK, stored)
		return
	}
	beneficiaryID, _ := payload["beneficiary_id"].(string)
	quote, err := g.quoteClient.AcceptQuote(customerID, quoteID, idKey, requestID, strings.TrimSpace(beneficiaryID))
	if err != nil {
		writeHTTPJSON(w, http.StatusBadRequest, errorEnvelope("VALIDATION_ERROR", err.Error(), requestID))
		return
	}
	response := map[string]any{"quote_id": quote.QuoteID, "trade_id": quote.TradeID, "status": quote.Status, "request_id": requestID}
	if err := g.idempotency.SaveResponse(customerID, "/v1/quotes/"+quoteID+"/accept", idKey, payload, response); err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("IDEMPOTENCY_UNAVAILABLE", "idempotency storage unavailable", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, response)
}

func (g *Gateway) handleGetQuote(w http.ResponseWriter, r *http.Request, customerID, requestID string) {
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
	writeHTTPJSON(w, http.StatusOK, map[string]any{"quote_id": quote.QuoteID, "status": quote.Status, "base_amount_minor": quote.BaseAmountMinor, "base_currency": quote.BaseCurrency, "quote_currency": quote.QuoteCurrency, "amount_out_minor": quote.AmountOutMinor, "rate_minor": quote.RateMinor, "fee_minor": quote.FeeMinor, "expires_at": quote.ExpiresAt.UTC().Format(time.RFC3339), "request_id": requestID})
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
	writeHTTPJSON(w, http.StatusOK, map[string]any{"trade_id": trade.TradeID, "quote_id": trade.QuoteID, "institution_id": trade.InstitutionID, "status": trade.Status, "base_amount_minor": trade.BaseAmountMinor, "base_currency": trade.BaseCurrency, "quote_currency": trade.QuoteCurrency, "settlement_id": trade.SettlementID, "request_id": requestID})
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
	writeHTTPJSON(w, http.StatusOK, map[string]any{"institution_id": balances.InstitutionID, "currency": balances.Currency, "available_minor": balances.AvailableMinor, "held_minor": balances.HeldMinor, "total_minor": balances.TotalMinor, "request_id": requestID})
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

func atoiDefault(v string, defaultValue int) int {
	var parsed int
	if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
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
