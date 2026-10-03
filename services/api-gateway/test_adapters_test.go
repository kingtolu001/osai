package main

import (
	"database/sql"
	"fmt"
	"github.com/osai/osai/services/quote/quotecore"
	"os"
	"strings"
	"time"
)

type customerServiceClientAdapter struct{ svc *CustomerService }
type quoteServiceClientAdapter struct{ store *quotecore.QuoteStore }
type defaultReadModelClient struct{}

func NewGateway(quoteStore *quotecore.QuoteStore) *Gateway {
	if quoteStore == nil {
		quoteStore = quotecore.NewQuoteStore()
	}
	cs := NewCustomerService()
	gw := NewGatewayWithClients(NewCustomerServiceClientAdapter(cs), NewQuoteServiceClientAdapterFromStore(quoteStore))
	return gw
}

func (g *Gateway) SetCustomerService(cs *CustomerService) {
	if cs != nil {
		g.customerClient = NewCustomerServiceClientAdapter(cs)
	}
}

func NewCustomerServiceClientAdapter(svc *CustomerService) CustomerClient {
	if svc == nil {
		svc = NewCustomerService()
	}
	return &customerServiceClientAdapter{svc: svc}
}

func NewQuoteServiceClientAdapterFromStore(store *quotecore.QuoteStore) QuoteClient {
	if store == nil {
		store = quotecore.NewQuoteStore()
	}
	return &quoteServiceClientAdapter{store: store}
}

func (c *customerServiceClientAdapter) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	ok, err := c.svc.ValidateCredential(publicID, secret)
	if err != nil || !ok {
		return CustomerAuthContext{}, fmt.Errorf("invalid credentials")
	}
	cred := c.svc.publicToCred[publicID]
	if cred == nil {
		return CustomerAuthContext{}, fmt.Errorf("invalid credentials")
	}
	inst, ok := c.svc.GetInstitution(cred.InstitutionID)
	if !ok {
		return CustomerAuthContext{}, fmt.Errorf("institution not found")
	}
	return CustomerAuthContext{InstitutionID: inst.ID, InstitutionStatus: inst.Status, KYBStatus: inst.KYBStatus, APIAccessStatus: "ACTIVE", APIEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
}

func (c *customerServiceClientAdapter) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	if err := c.svc.ValidateInstitutionAccess(instID); err != nil {
		return CustomerAuthContext{}, err
	}
	inst, _ := c.svc.GetInstitution(instID)
	return CustomerAuthContext{InstitutionID: inst.ID, InstitutionStatus: inst.Status, KYBStatus: inst.KYBStatus, APIAccessStatus: "ACTIVE", APIEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
}

func (c *customerServiceClientAdapter) GetInstitution(instID string) (CustomerInstitution, error) {
	inst, ok := c.svc.GetInstitution(instID)
	if !ok {
		return CustomerInstitution{}, fmt.Errorf("institution not found")
	}
	return inst, nil
}

func (c *customerServiceClientAdapter) GetWebhookConfiguration(instID string) (WebhookConfig, error) {
	_, ok := c.svc.GetInstitution(instID)
	if !ok {
		return WebhookConfig{}, fmt.Errorf("institution not found")
	}
	return WebhookConfig{InstitutionID: instID, WebhookURL: "https://example.invalid/webhook", Enabled: true, Status: "ACTIVE"}, nil
}

func (q *quoteServiceClientAdapter) CreateQuote(req QuoteRequest) (QuoteResponse, error) {
	quote, err := q.store.Create(quotecore.QuoteRequest{
		CustomerID:      req.InstitutionID,
		BaseAmountMinor: req.BaseAmountMinor,
		BaseCurrency:    req.BaseCurrency,
		QuoteCurrency:   req.QuoteCurrency,
		DestinationRail: req.DestinationRail,
		Urgency:         req.Urgency,
		IdempotencyKey:  req.IdempotencyKey,
	}, req.DestinationRail, 1200, 500, req.BaseAmountMinor/100, time.Now().Add(30*time.Minute))
	if err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: quote.ID, Status: string(quote.Status), BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, DestinationRail: quote.Request.DestinationRail, AmountOutMinor: quote.AmountOutMinor, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, ExpiresAt: quote.ExpiresAt}, nil
}

func (q *quoteServiceClientAdapter) GetQuote(institutionID, quoteID string) (QuoteResponse, error) {
	quote, ok := q.store.Get(quoteID)
	if !ok {
		return QuoteResponse{}, fmt.Errorf("quote not found")
	}
	if quote.Request.CustomerID != institutionID {
		return QuoteResponse{}, fmt.Errorf("forbidden")
	}
	return QuoteResponse{QuoteID: quote.ID, Status: string(quote.Status), BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, DestinationRail: quote.Request.DestinationRail, AmountOutMinor: quote.AmountOutMinor, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, ExpiresAt: quote.ExpiresAt}, nil
}

func (q *quoteServiceClientAdapter) AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, beneficiaryID string) (QuoteResponse, error) {
	quote, ok := q.store.Get(quoteID)
	if !ok {
		return QuoteResponse{}, fmt.Errorf("quote not found")
	}
	if quote.Request.CustomerID != institutionID {
		return QuoteResponse{}, fmt.Errorf("forbidden")
	}
	if quote.Status == quotecore.QuoteStatusAccepted {
		return QuoteResponse{QuoteID: quote.ID, Status: string(quote.Status), TradeID: quote.AcceptedTradeID}, nil
	}
	tradeID := "trd_" + time.Now().UTC().Format("20060102150405")
	if err := quote.Accept(tradeID, idempotencyKey, time.Now().UTC(), q.store); err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: quote.ID, TradeID: tradeID, Status: string(quote.Status), BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, DestinationRail: quote.Request.DestinationRail, AmountOutMinor: quote.AmountOutMinor, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, ExpiresAt: quote.ExpiresAt, CorrelationID: correlationID}, nil
}

func (r *defaultReadModelClient) GetBalances(institutionID string) (BalanceReadResponse, error) {
	if strings.TrimSpace(institutionID) == "" {
		return BalanceReadResponse{}, fmt.Errorf("forbidden")
	}
	return BalanceReadResponse{InstitutionID: institutionID, Currency: "USD", AvailableMinor: 0, HeldMinor: 0, TotalMinor: 0}, nil
}

func (r *defaultReadModelClient) GetTransactions(institutionID string, page, pageSize int) (TransactionPage, error) {
	if strings.TrimSpace(institutionID) == "" {
		return TransactionPage{}, fmt.Errorf("forbidden")
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 25
	}
	return TransactionPage{Items: []TransactionItem{}, Page: page, PageSize: pageSize, Total: 0}, nil
}

func seedInstitution(cs *CustomerService) {
	if cs == nil {
		return
	}
	instID := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_INSTITUTION_ID"))
	clientID := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("OSAI_SANDBOX_CLIENT_SECRET"))
	if instID == "" {
		instID = "inst_sandbox_local"
	}
	if clientID == "" {
		clientID = "ck_sandbox_local"
	}
	if secret == "" {
		secret = "secret_local_001"
	}

	cs.mu.Lock()
	if _, exists := cs.institutions[instID]; !exists {
		cs.institutions[instID] = &CustomerInstitution{ID: instID, Name: "Sandbox Institution", Status: "ACTIVE", KYBStatus: "APPROVED", Country: "US", APIEnabled: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	if _, exists := cs.publicToCred[clientID]; !exists {
		cs.publicToCred[clientID] = &CustomerCredential{ID: "cred_sandbox_local", PublicID: clientID, InstitutionID: instID, Status: "ACTIVE", SecretHash: hashCustomerSecret(secret, instID), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	cs.mu.Unlock()

	_ = os.Setenv("OSAI_SANDBOX_INSTITUTION_ID", instID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_ID", clientID)
	_ = os.Setenv("OSAI_SANDBOX_CLIENT_SECRET", secret)
}

func NewGatewayWithClients(customerClient CustomerClient, quoteClient QuoteClient) *Gateway {
	return &Gateway{customerClient: customerClient, quoteClient: quoteClient, idempotency: newIdempotencyStore()}
}

func newIdempotencyStore() *idempotencyStore {
	dsn := strings.TrimSpace(os.Getenv("OSAI_POSTGRES_DSN"))
	if dsn == "" {
		return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	}
	if err := ensureCustomerAPISchema(db); err != nil {
		_ = db.Close()
		return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	}
	return &idempotencyStore{rows: map[string]*idempotencyEntry{}, db: db}
}
