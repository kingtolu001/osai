package main

import (
	"fmt"
	"github.com/osai/osai/services/quote/quotecore"
	"os"
	"strings"
	"time"
)

// In-process service substitutes exist only in the gateway test binary.
type customerServiceClientAdapter struct{ svc *CustomerService }
type quoteServiceClientAdapter struct{ store *quotecore.QuoteStore }

func NewGateway(quoteStore *quotecore.QuoteStore) *Gateway {
	if quoteStore == nil {
		quoteStore = quotecore.NewQuoteStore()
	}
	return &Gateway{
		quoteClient: NewQuoteServiceClientAdapterFromStore(quoteStore),
		idempotency: newIdempotencyStore(),
		rateLimiter: newRateLimiterStore(),
	}
}

func (g *Gateway) SetCustomerService(cs *CustomerService) {
	if cs != nil {
		g.customerClient = NewCustomerServiceClientAdapter(cs)
	}
}

func NewCustomerServiceClientAdapter(svc *CustomerService) CustomerClient {
	if svc == nil {
		return nil
	}
	return &customerServiceClientAdapter{svc: svc}
}

func NewQuoteServiceClientAdapterFromStore(store *quotecore.QuoteStore) QuoteClient {
	if store == nil {
		return nil
	}
	return &quoteServiceClientAdapter{store: store}
}

func (c *customerServiceClientAdapter) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	ok, err := c.svc.ValidateCredential(publicID, secret)
	if err != nil || !ok {
		return CustomerAuthContext{}, fmt.Errorf("invalid credentials")
	}
	c.svc.mu.Lock()
	cred := c.svc.publicToCred[publicID]
	if cred == nil {
		c.svc.mu.Unlock()
		return CustomerAuthContext{}, fmt.Errorf("invalid credentials")
	}
	credential := *cred
	c.svc.mu.Unlock()
	inst, ok := c.svc.GetInstitution(credential.InstitutionID)
	if !ok {
		return CustomerAuthContext{}, fmt.Errorf("institution not found")
	}
	return CustomerAuthContext{InstitutionID: inst.ID, InstitutionStatus: inst.Status, KYBStatus: inst.KYBStatus, APIAccessStatus: "ACTIVE", Environment: credential.Environment, APIEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
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

func (c *customerServiceClientAdapter) CreateCredential(institutionID, label, environment string) (*CustomerCredential, string, error) {
	cred, secret, err := c.svc.CreateCredential(institutionID, label, environment)
	if err != nil {
		return nil, "", err
	}
	return &cred, secret, nil
}

func (c *customerServiceClientAdapter) RotateCredential(institutionID, publicID string) (*CustomerCredential, string, error) {
	cred, secret, err := c.svc.RotateCredential(institutionID, publicID)
	if err != nil {
		return nil, "", err
	}
	return &cred, secret, nil
}

func (c *customerServiceClientAdapter) RevokeCredential(institutionID, publicID string) error {
	return c.svc.RevokeCredential(institutionID, publicID)
}

func (c *customerServiceClientAdapter) GetCredential(institutionID, publicID string) (*CustomerCredential, error) {
	return c.svc.GetCredential(institutionID, publicID)
}

func (c *customerServiceClientAdapter) ListCredentials(institutionID string) ([]*CustomerCredential, error) {
	return c.svc.ListCredentials(institutionID)
}

func (q *quoteServiceClientAdapter) CreateQuote(req QuoteRequest) (QuoteResponse, error) {
	quote, err := q.store.Create(quotecore.QuoteRequest{
		CustomerID:             req.InstitutionID,
		QuoteMode:              quotecore.QuoteMode(req.QuoteMode),
		BaseAmountMinor:        req.BaseAmountMinor,
		DestinationAmountMinor: req.DestinationAmountMinor,
		BaseCurrency:           req.BaseCurrency,
		QuoteCurrency:          req.QuoteCurrency,
		DestinationRail:        req.DestinationRail,
		Urgency:                req.Urgency,
		IdempotencyKey:         req.IdempotencyKey,
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

func (q *quoteServiceClientAdapter) AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, _ string) (QuoteResponse, error) {
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
	return &Gateway{
		customerClient: customerClient,
		quoteClient:    quoteClient,
		idempotency:    newIdempotencyStore(),
		rateLimiter:    newRateLimiterStore(),
	}
}

func newIdempotencyStore() *idempotencyStore {
	// Database durability is exercised by the explicit PostgreSQL tests with
	// isolated schemas and cleanup, rather than leaking pools from UI fixtures.
	return &idempotencyStore{rows: map[string]*idempotencyEntry{}}
}
