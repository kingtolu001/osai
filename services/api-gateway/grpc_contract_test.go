package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubCustomerClient struct{}

type customerReadEntitledClient struct{ stubCustomerClient }

type providerStatusEntitledClient struct{ stubCustomerClient }

type stubProviderStatusClient struct {
	items []ProviderRouteStatus
	err   error
}

func (s stubProviderStatusClient) GetProviderStatuses(context.Context) ([]ProviderRouteStatus, error) {
	return s.items, s.err
}

type productionCredentialCustomerClient struct{ stubCustomerClient }

func (providerStatusEntitledClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID: instID, InstitutionStatus: "ACTIVE", APIEnabled: true,
		Entitlements: []string{"providers:read"},
	}, nil
}

func (productionCredentialCustomerClient) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	if publicID != "ck_ok" || secret != "s3cr3t" {
		return CustomerAuthContext{}, errors.New("invalid credentials")
	}
	return CustomerAuthContext{
		InstitutionID: "inst_1", InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED",
		APIAccessStatus: "ACTIVE", Environment: "production", APIEnabled: true,
		Entitlements: []string{"quotes:read", "quotes:write"},
	}, nil
}

func (customerReadEntitledClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED",
		APIAccessStatus: "ACTIVE", APIEnabled: true,
		Entitlements: []string{
			"quotes:read", "quotes:write", "trades:read", "settlements:read",
			"reports:read", "balances:read", "transactions:read", "beneficiaries:write",
		},
	}, nil
}

func (stubCustomerClient) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	if publicID == "ck_ok" && secret == "s3cr3t" {
		return CustomerAuthContext{InstitutionID: "inst_1", InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED", APIAccessStatus: "ACTIVE", APIEnabled: true, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
	}
	return CustomerAuthContext{}, errors.New("invalid credentials")
}

func (stubCustomerClient) CreateCredential(institutionID, label, environment string) (*CustomerCredential, string, error) {
	return nil, "", errors.New("not implemented")
}

func (stubCustomerClient) RotateCredential(institutionID, publicID string) (*CustomerCredential, string, error) {
	return nil, "", errors.New("not implemented")
}

func (stubCustomerClient) RevokeCredential(institutionID, publicID string) error {
	return errors.New("not implemented")
}

func (stubCustomerClient) GetCredential(institutionID, publicID string) (*CustomerCredential, error) {
	return nil, errors.New("not implemented")
}

func (stubCustomerClient) ListCredentials(institutionID string) ([]*CustomerCredential, error) {
	return nil, errors.New("not implemented")
}

type failingCustomerAuthClient struct {
	stubCustomerClient
	err error
}

func (c failingCustomerAuthClient) AuthenticateAPIClient(string, string) (CustomerAuthContext, error) {
	return CustomerAuthContext{}, c.err
}

func TestAuthenticationErrorsAreSafeAndDistinguishUnavailableService(t *testing.T) {
	tests := []struct {
		name       string
		client     CustomerClient
		wantStatus int
		wantCode   string
	}{
		{
			name:       "backend error is not leaked",
			client:     failingCustomerAuthClient{err: errors.New("database password leaked")},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "UNAUTHENTICATED",
		},
		{
			name:       "authentication service outage is unavailable",
			client:     failingCustomerAuthClient{err: status.Error(codes.Unavailable, "private backend detail")},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "SERVICE_UNAVAILABLE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gateway := NewGatewayWithClients(tt.client, stubQuoteClient{})
			server := httptest.NewServer(gateway)
			defer server.Close()

			result := doJSONGET(t, server, "/v1/balances", "ApiKey ck_ok:s3cr3t")
			apiError, ok := result.Body["error"].(map[string]any)
			if result.StatusCode != tt.wantStatus || !ok || apiError["code"] != tt.wantCode {
				t.Fatalf("unexpected auth failure response: %+v", result)
			}
			body, err := json.Marshal(result.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "database password") || strings.Contains(string(body), "private backend detail") {
				t.Fatalf("authentication response leaked internal detail: %s", body)
			}
		})
	}
}

func (stubSettlementClient) ListCustomerSettlementReport(institutionID string, request SettlementReportRequest) (SettlementReportPage, error) {
	if institutionID != "inst_1" || request.Status != "CONFIRMED" || request.Currency != "USD" {
		return SettlementReportPage{}, errors.New("unexpected settlement report filters")
	}
	return SettlementReportPage{Items: []SettlementReportItem{{
		SettlementID: "si_456", TradeID: "trd_123", QuoteID: "quo_123",
		Status: "CONFIRMED", AmountMinor: 9500, Currency: "USD",
		OccurredAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	}}, Page: request.Page, PageSize: request.PageSize, Total: 1}, nil
}

func (stubCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED", APIAccessStatus: "ACTIVE", APIEnabled: true, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
}

type readOnlyQuoteCustomerClient struct{ stubCustomerClient }

func (readOnlyQuoteCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED",
		APIAccessStatus: "ACTIVE", APIEnabled: true, Entitlements: []string{"quotes:read"},
	}, nil
}

type writeOnlyQuoteCustomerClient struct{ stubCustomerClient }

func (writeOnlyQuoteCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED",
		APIAccessStatus: "ACTIVE", APIEnabled: true, Entitlements: []string{"quotes:write"},
	}, nil
}

func TestQuoteWriteRoutesRequireWriteEntitlement(t *testing.T) {
	quoteClient := &recordingQuoteClient{}
	gateway := NewGatewayWithClients(readOnlyQuoteCustomerClient{}, quoteClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSON(t, server, http.MethodPost, "/v1/quotes", "ApiKey ck_ok:s3cr3t", "read-only-quote",
		`{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"USD"}`)
	if result.StatusCode != http.StatusForbidden || len(quoteClient.requests) != 0 {
		t.Fatalf("quote write without entitlement was not denied: status=%d requests=%d body=%+v", result.StatusCode, len(quoteClient.requests), result.Body)
	}
}

func TestQuoteReadRoutesRequireReadEntitlement(t *testing.T) {
	gateway := NewGatewayWithClients(writeOnlyQuoteCustomerClient{}, stubQuoteClient{})
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSONGET(t, server, "/v1/quotes/quo_123", "ApiKey ck_ok:s3cr3t")
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("quote read without entitlement was not denied: status=%d body=%+v", result.StatusCode, result.Body)
	}
}

func TestSettlementReportRouteReturnsOnlyReportFields(t *testing.T) {
	gateway := NewGatewayWithClients(customerReadEntitledClient{}, stubQuoteClient{})
	gateway.SetSettlementClient(stubSettlementClient{})
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSONGET(t, server, "/v1/reports/settlements?currency=USD&status=CONFIRMED", "ApiKey ck_ok:s3cr3t")
	if result.StatusCode != http.StatusOK {
		t.Fatalf("settlement report failed: %+v", result)
	}
	items, ok := result.Body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected settlement report items: %+v", result.Body["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["amount_minor"] != float64(9500) || item["currency"] != "USD" {
		t.Fatalf("report row is missing required fields: %+v", items[0])
	}
	for _, forbidden := range []string{"beneficiary", "provider_ref", "client_ref"} {
		if _, exists := item[forbidden]; exists {
			t.Fatalf("settlement report leaked forbidden field %q", forbidden)
		}
	}
}

func (stubCustomerClient) GetInstitution(instID string) (CustomerInstitution, error) {
	return CustomerInstitution{ID: instID, Name: "Stub", Status: "ACTIVE", KYBStatus: "APPROVED", Country: "US", APIEnabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (stubCustomerClient) GetWebhookConfiguration(instID string) (WebhookConfig, error) {
	return WebhookConfig{InstitutionID: instID, WebhookURL: "https://example.com/hook", Enabled: true, Status: "ACTIVE"}, nil
}

type webhookEntitledCustomerClient struct {
	stubCustomerClient
}

func (webhookEntitledCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{
		InstitutionID:     instID,
		InstitutionStatus: "ACTIVE",
		APIAccessStatus:   "ACTIVE",
		APIEnabled:        true,
		Entitlements:      []string{"webhooks:write"},
	}, nil
}

type stubWebhookConfigClient struct {
	calls       int
	replayCalls int
	institution string
	endpointURL string
	secret      string
	eventTypes  []string
	err         error
	replayErr   error
	replayEvent string
}

func (c *stubWebhookConfigClient) ConfigureWebhook(_ context.Context, institutionID, endpointURL, signingSecret string, eventTypes []string) error {
	c.calls++
	c.institution = institutionID
	c.endpointURL = endpointURL
	c.secret = signingSecret
	c.eventTypes = append([]string(nil), eventTypes...)
	return c.err
}

func (c *stubWebhookConfigClient) ReplayWebhook(_ context.Context, institutionID, eventID, idempotencyKey string) error {
	c.replayCalls++
	c.institution = institutionID
	c.replayEvent = eventID
	return c.replayErr
}

type stubQuoteClient struct{}

func (stubQuoteClient) CreateQuote(req QuoteRequest) (QuoteResponse, error) {
	return QuoteResponse{QuoteID: "quo_123", Status: "OPEN", BaseAmountMinor: req.BaseAmountMinor, BaseCurrency: req.BaseCurrency, QuoteCurrency: req.QuoteCurrency, AmountOutMinor: req.BaseAmountMinor / 10, RateMinor: 1200, FeeMinor: 500}, nil
}

func (stubQuoteClient) GetQuote(institutionID, quoteID string) (QuoteResponse, error) {
	if quoteID == "quo_123" && institutionID == "inst_1" {
		return QuoteResponse{QuoteID: quoteID, Status: "OPEN", BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", AmountOutMinor: 1000, RateMinor: 1200, FeeMinor: 500}, nil
	}
	return QuoteResponse{}, errors.New("not found")
}

func (stubQuoteClient) AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, beneficiaryID string) (QuoteResponse, error) {
	return QuoteResponse{QuoteID: quoteID, Status: "ACCEPTED", BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", TradeID: "trd_999", CorrelationID: correlationID}, nil
}

type capturingQuoteClient struct {
	stubQuoteClient
	request QuoteRequest
}

func (c *capturingQuoteClient) CreateQuote(request QuoteRequest) (QuoteResponse, error) {
	c.request = request
	return QuoteResponse{
		QuoteID: "quo_capture", Status: "QUOTED", BaseAmountMinor: request.BaseAmountMinor,
		BaseCurrency: request.BaseCurrency, QuoteCurrency: request.QuoteCurrency,
		AmountOutMinor: 9500, RateMinor: 1050, FeeMinor: 50, ExpiresAt: time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC),
	}, nil
}

func TestGatewayUsesInjectedClients(t *testing.T) {
	gateway := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
	_, err := gateway.authenticateHeader("ApiKey ck_ok:s3cr3t")
	if err != nil {
		t.Fatalf("authenticateHeader should succeed via injected client: %v", err)
	}
	_, err = gateway.handleCreateQuoteForTest("inst_1", "idem-1", map[string]any{"base_amount_minor": 10000, "base_currency": "NGN", "quote_currency": "USD", "destination_rail": "sim_lp_1", "urgency": "normal"})
	if err != nil {
		t.Fatalf("quote creation should succeed via injected client: %v", err)
	}
}

func TestTradeListRouteForwardsOwnerScopedFilters(t *testing.T) {
	gateway := NewGatewayWithClients(customerReadEntitledClient{}, stubQuoteClient{})
	gateway.SetTradeClient(stubTradeClient{})
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSONGET(t, server, "/v1/trades?page=2&page_size=5&currency=NGN&status=ACCEPTED&customer_reference=partner-42", "ApiKey ck_ok:s3cr3t")
	if result.StatusCode != http.StatusOK {
		t.Fatalf("trade list failed: %+v", result)
	}
	items, ok := result.Body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected trade list items: %+v", result.Body["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["customer_reference"] != "partner-42" || result.Body["page"] != float64(2) || result.Body["total"] != float64(1) {
		t.Fatalf("trade list response does not match contract: %+v", result.Body)
	}
}

func TestQuoteRequestPersistsCustomerReferenceAndAddsAliases(t *testing.T) {
	quoteClient := &capturingQuoteClient{}
	gateway := NewGatewayWithClients(stubCustomerClient{}, quoteClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSON(t, server, http.MethodPost, "/v1/quotes", "ApiKey ck_ok:s3cr3t", "quote-ref-"+uuid.NewString(),
		`{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"USD","customer_reference":"partner-order-42"}`)
	if result.StatusCode != http.StatusOK {
		t.Fatalf("quote creation failed: %+v", result)
	}

	if quoteClient.request.CustomerReference != "partner-order-42" {
		t.Fatalf("customer reference was not forwarded: %+v", quoteClient.request)
	}
	if result.Body["sell_amount_minor"] != float64(10000) || result.Body["sell_currency"] != "NGN" ||
		result.Body["buy_amount_minor"] != float64(9500) || result.Body["buy_currency"] != "USD" {
		t.Fatalf("quote aliases do not match existing economics: %+v", result.Body)
	}
}

func TestProviderStatusRequiresEntitlementAndUsesSafeRouteContract(t *testing.T) {
	denied := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
	denied.SetProviderStatusClient(stubProviderStatusClient{items: []ProviderRouteStatus{{RouteID: "internal-provider-id", Status: "AVAILABLE"}}})
	server := httptest.NewServer(denied)
	result := doJSONGET(t, server, "/v1/providers/status", "ApiKey ck_ok:s3cr3t")
	server.Close()
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("provider status without entitlement must be forbidden: %+v", result)
	}

	gateway := NewGatewayWithClients(providerStatusEntitledClient{}, stubQuoteClient{})
	gateway.SetProviderStatusClient(stubProviderStatusClient{items: []ProviderRouteStatus{
		{RouteID: "simulator", Status: "AVAILABLE", Capabilities: []string{"quotes", "fx", "payout"}},
		{RouteID: "ngn-bank-transfer", Status: "UNAVAILABLE", Capabilities: []string{"payout"}},
	}})
	server = httptest.NewServer(gateway)
	defer server.Close()
	result = doJSONGET(t, server, "/v1/providers/status", "ApiKey ck_ok:s3cr3t")
	items, ok := result.Body["items"].([]any)
	if result.StatusCode != http.StatusOK || !ok || len(items) != 2 {
		t.Fatalf("provider status response malformed: %+v", result)
	}
	if result.Body["request_id"] == "" {
		t.Fatalf("request id missing from provider status: %+v", result.Body)
	}
	first, ok := items[0].(map[string]any)
	if !ok || first["route_id"] != "simulator" || first["status"] != "AVAILABLE" || first["provider_id"] != nil {
		t.Fatalf("provider response exposed provider identity or wrong route shape: %+v", items[0])
	}
}

func TestFixedDestinationQuoteRequestForwardsOnlyTargetAmount(t *testing.T) {
	quoteClient := &recordingQuoteClient{}
	gateway := NewGatewayWithClients(stubCustomerClient{}, quoteClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSON(t, server, http.MethodPost, "/v1/quotes", "ApiKey ck_ok:s3cr3t", "fixed-dest-"+uuid.NewString(),
		`{"quote_mode":"FIXED_DESTINATION","destination_amount_minor":15000000,"base_currency":"USD","quote_currency":"NGN"}`)
	if result.StatusCode != http.StatusOK {
		t.Fatalf("fixed-destination quote request failed: %+v", result)
	}
	if result.Body["total_cost_minor"] != result.Body["total_debit_minor"] ||
		result.Body["total_cost_currency"] != "USD" {
		t.Fatalf("quote total cost must be the source-currency all-in debit: %+v", result.Body)
	}
	if len(quoteClient.requests) != 1 || quoteClient.requests[0].QuoteMode != "FIXED_DESTINATION" ||
		quoteClient.requests[0].BaseAmountMinor != 0 ||
		quoteClient.requests[0].DestinationAmountMinor != 15_000_000 {
		t.Fatalf("fixed-destination request was not forwarded exactly: %+v", quoteClient.requests)
	}

	invalid := doJSON(t, server, http.MethodPost, "/v1/quotes", "ApiKey ck_ok:s3cr3t", "fixed-dest-invalid-"+uuid.NewString(),
		`{"quote_mode":"FIXED_DESTINATION","base_amount_minor":10000,"destination_amount_minor":15000000,"base_currency":"USD","quote_currency":"NGN"}`)
	if invalid.StatusCode != http.StatusBadRequest || len(quoteClient.requests) != 1 {
		t.Fatalf("mixed quote-mode amounts should be rejected before service call: status=%d calls=%d", invalid.StatusCode, len(quoteClient.requests))
	}
}

func TestWebhookEndpointConfigurationIsEntitledAndIdempotent(t *testing.T) {
	customer := webhookEntitledCustomerClient{}
	webhookClient := &stubWebhookConfigClient{}
	gateway := NewGatewayWithClients(customer, stubQuoteClient{})
	gateway.SetWebhookConfigClient(webhookClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	idempotencyKey := "webhook-" + uuid.NewString()
	body := `{"endpoint_url":"https://partner.example/events","signing_secret":"client-generated-secret","subscribed_event_types":["trade.accepted"]}`
	first := doJSON(t, server, http.MethodPost, "/v1/webhook-endpoints", "ApiKey ck_ok:s3cr3t", idempotencyKey, body)
	if first.StatusCode != http.StatusOK || first.Body["configured"] != true {
		t.Fatalf("webhook configuration failed: %+v", first)
	}
	responseBody, err := json.Marshal(first.Body)
	if err != nil {
		t.Fatalf("encode webhook response: %v", err)
	}
	if strings.Contains(string(responseBody), "client-generated-secret") {
		t.Fatal("webhook secret leaked in response")
	}
	replay := doJSON(t, server, http.MethodPost, "/v1/webhook-endpoints", "ApiKey ck_ok:s3cr3t", idempotencyKey, body)
	if replay.StatusCode != http.StatusOK || webhookClient.calls != 1 {
		t.Fatalf("same-key replay must configure once: response=%+v calls=%d", replay, webhookClient.calls)
	}
	conflict := doJSON(t, server, http.MethodPost, "/v1/webhook-endpoints", "ApiKey ck_ok:s3cr3t", idempotencyKey,
		`{"endpoint_url":"https://partner.example/events","signing_secret":"different-secret","subscribed_event_types":["trade.accepted"]}`)
	if conflict.StatusCode != http.StatusConflict || webhookClient.calls != 1 {
		t.Fatalf("changed payload must conflict without reconfiguration: response=%+v calls=%d", conflict, webhookClient.calls)
	}
	if webhookClient.institution != "inst_1" || webhookClient.endpointURL != "https://partner.example/events" ||
		webhookClient.secret != "client-generated-secret" || len(webhookClient.eventTypes) != 1 {
		t.Fatalf("unexpected configuration request: %+v", webhookClient)
	}
}

func TestGatewayRejectsCredentialFromDifferentEnvironment(t *testing.T) {
	gateway := NewGatewayWithClients(productionCredentialCustomerClient{}, stubQuoteClient{})
	if err := gateway.SetCredentialEnvironment("sandbox"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/quotes/quo_123", nil)
	request.Header.Set("Authorization", "ApiKey ck_ok:s3cr3t")
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("production credential accepted in sandbox environment: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCustomerReadAndBeneficiaryRoutesRequireEntitlements(t *testing.T) {
	gateway := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
	cases := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/beneficiaries"},
		{method: http.MethodGet, path: "/v1/trades"},
		{method: http.MethodGet, path: "/v1/trades/trd_1"},
		{method: http.MethodGet, path: "/v1/settlements/stl_1"},
		{method: http.MethodGet, path: "/v1/reports/settlements"},
		{method: http.MethodGet, path: "/v1/balances"},
		{method: http.MethodGet, path: "/v1/transactions"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "ApiKey ck_ok:s3cr3t")
			recorder := httptest.NewRecorder()
			gateway.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("route status=%d body=%s; want 403 without route entitlement", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestWebhookEndpointRequiresEntitlement(t *testing.T) {
	webhookClient := &stubWebhookConfigClient{}
	gateway := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
	gateway.SetWebhookConfigClient(webhookClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSON(t, server, http.MethodPost, "/v1/webhook-endpoints", "ApiKey ck_ok:s3cr3t", "webhook-no-entitlement",
		`{"endpoint_url":"https://partner.example/events","signing_secret":"client-generated-secret","subscribed_event_types":["trade.accepted"]}`)
	if result.StatusCode != http.StatusForbidden || webhookClient.calls != 0 {
		t.Fatalf("webhook configuration must require entitlement: response=%+v calls=%d", result, webhookClient.calls)
	}
}

func TestWebhookReplayIsEntitledDurableAndIdempotent(t *testing.T) {
	replayClient := &stubWebhookConfigClient{}
	gateway := NewGatewayWithClients(webhookEntitledCustomerClient{}, stubQuoteClient{})
	gateway.SetWebhookReplayClient(replayClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	eventID := "evt_" + uuid.NewString()
	key := "replay-" + uuid.NewString()
	path := "/v1/webhook-deliveries/" + eventID + "/replay"
	first := doJSON(t, server, http.MethodPost, path, "ApiKey ck_ok:s3cr3t", key, "")
	if first.StatusCode != http.StatusAccepted || first.Body["event_id"] != eventID || first.Body["status"] != "QUEUED" {
		t.Fatalf("webhook replay was not queued: %+v", first)
	}
	second := doJSON(t, server, http.MethodPost, path, "ApiKey ck_ok:s3cr3t", key, "")
	if second.StatusCode != http.StatusAccepted || replayClient.replayCalls != 1 {
		t.Fatalf("same-key replay should not queue a second attempt: response=%+v calls=%d", second, replayClient.replayCalls)
	}
	if replayClient.institution != "inst_1" || replayClient.replayEvent != eventID {
		t.Fatalf("replay was not scoped to the authenticated institution and event: %+v", replayClient)
	}
}

func TestWebhookReplayRequiresEntitlement(t *testing.T) {
	replayClient := &stubWebhookConfigClient{}
	gateway := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
	gateway.SetWebhookReplayClient(replayClient)
	server := httptest.NewServer(gateway)
	defer server.Close()

	result := doJSON(t, server, http.MethodPost, "/v1/webhook-deliveries/evt_123/replay", "ApiKey ck_ok:s3cr3t", "replay-no-entitlement", "")
	if result.StatusCode != http.StatusForbidden || replayClient.replayCalls != 0 {
		t.Fatalf("webhook replay must require entitlement: response=%+v calls=%d", result, replayClient.replayCalls)
	}
}

type stubTradeClient struct{}

func (stubTradeClient) GetTrade(institutionID, tradeID string) (TradeReadResponse, error) {
	if institutionID == "inst_1" && tradeID == "trd_123" {
		return TradeReadResponse{TradeID: tradeID, InstitutionID: institutionID, QuoteID: "quo_123", Status: "ACCEPTED", BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", SettlementID: "si_456"}, nil
	}
	if institutionID != "inst_1" {
		return TradeReadResponse{}, errors.New("forbidden")
	}
	return TradeReadResponse{}, errors.New("trade not found")
}

func (stubTradeClient) ListTrades(institutionID string, request TradeListRequest) (TradePage, error) {
	if institutionID != "inst_1" {
		return TradePage{}, errors.New("forbidden")
	}
	if request.Currency != "NGN" || request.Status != "ACCEPTED" || request.CustomerReference != "partner-42" {
		return TradePage{}, errors.New("unexpected trade filters")
	}
	return TradePage{Items: []TradeReadResponse{{TradeID: "trd_123", InstitutionID: institutionID, QuoteID: "quo_123", Status: "ACCEPTED", BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", CustomerReference: "partner-42"}}, Page: request.Page, PageSize: request.PageSize, Total: 1}, nil
}

type stubSettlementClient struct{}

func (stubSettlementClient) GetSettlement(institutionID, settlementID string) (SettlementReadResponse, error) {
	if institutionID == "inst_1" && settlementID == "si_456" {
		return SettlementReadResponse{SettlementID: settlementID, InstitutionID: institutionID, TradeID: "trd_123", QuoteID: "quo_123", Status: "CONFIRMED", Beneficiary: "acct_1", AmountMinor: 10000, Currency: "NGN", Purpose: "trade-acceptance"}, nil
	}
	if institutionID != "inst_1" {
		return SettlementReadResponse{}, errors.New("forbidden")
	}
	return SettlementReadResponse{}, errors.New("settlement not found")
}

type stubBalanceClient struct{}

type stubReportingServiceClient struct {
	balances []*reportingv1.Balance
}

func (c stubReportingServiceClient) GetBalances(context.Context, *reportingv1.GetBalancesRequest, ...grpc.CallOption) (*reportingv1.GetBalancesResponse, error) {
	return &reportingv1.GetBalancesResponse{Balances: c.balances}, nil
}

func (stubReportingServiceClient) ListTransactions(context.Context, *reportingv1.ListTransactionsRequest, ...grpc.CallOption) (*reportingv1.ListTransactionsResponse, error) {
	return &reportingv1.ListTransactionsResponse{}, nil
}

func TestGRPCReportingClientPreservesAllBalanceCurrencies(t *testing.T) {
	client := &grpcReportingClient{client: stubReportingServiceClient{balances: []*reportingv1.Balance{
		{InstitutionId: "inst_1", Currency: "USD", AvailableMinor: 5000, HeldMinor: 1000, TotalMinor: 6000},
		{InstitutionId: "inst_1", Currency: "NGN", AvailableMinor: 7000, HeldMinor: 3000, TotalMinor: 10000},
	}}}
	got, err := client.GetBalances("inst_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.InstitutionID != "inst_1" || len(got.Balances) != 2 ||
		got.Balances[0].Currency != "USD" || got.Balances[1].Currency != "NGN" ||
		got.Balances[1].TotalMinor != 10000 {
		t.Fatalf("reporting balances were not preserved: %+v", got)
	}

	emptyClient := &grpcReportingClient{client: stubReportingServiceClient{}}
	empty, err := emptyClient.GetBalances("inst_empty")
	if err != nil {
		t.Fatalf("an empty ledger read should return an empty balance collection: %v", err)
	}
	if empty.InstitutionID != "inst_empty" || len(empty.Balances) != 0 {
		t.Fatalf("unexpected empty balance response: %+v", empty)
	}
}

func (stubBalanceClient) GetBalances(institutionID string) (BalanceReadResponse, error) {
	if institutionID == "inst_1" {
		return BalanceReadResponse{InstitutionID: institutionID, Balances: []BalanceItem{
			{Currency: "USD", AvailableMinor: 5000, HeldMinor: 1000, TotalMinor: 6000},
			{Currency: "NGN", AvailableMinor: 7000, HeldMinor: 3000, TotalMinor: 10000},
		}}, nil
	}
	return BalanceReadResponse{}, errors.New("forbidden")
}

func (stubBalanceClient) GetTransactions(institutionID string, page, pageSize int) (TransactionPage, error) {
	if institutionID == "inst_1" {
		return TransactionPage{Items: []TransactionItem{{ID: "txn_1", Type: "TRADE", AmountMinor: 10000, Currency: "NGN", Status: "POSTED"}}, Page: page, PageSize: pageSize, Total: 1}, nil
	}
	return TransactionPage{}, errors.New("forbidden")
}

func TestGatewayRequiresRealReadModelClient(t *testing.T) {
	gateway := NewGatewayWithClients(customerReadEntitledClient{}, stubQuoteClient{})
	gateway.SetReadModelClient(nil)
	server := httptest.NewServer(gateway)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/balances", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "ApiKey ck_ok:s3cr3t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected production fallback to be unavailable without reporting client, got %d", resp.StatusCode)
	}
}

func TestGatewayReadBoundariesAndCustomerSafeFields(t *testing.T) {
	gateway := NewGatewayWithClients(customerReadEntitledClient{}, stubQuoteClient{})
	gateway.SetTradeClient(stubTradeClient{})
	gateway.SetSettlementClient(stubSettlementClient{})
	gateway.SetReadModelClient(stubBalanceClient{})
	server := httptest.NewServer(gateway)
	defer server.Close()

	valid := "ApiKey ck_ok:s3cr3t"
	tradeReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/trades/trd_123", nil)
	if err != nil {
		t.Fatal(err)
	}
	tradeReq.Header.Set("Authorization", valid)
	tradeResp, err := http.DefaultClient.Do(tradeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer tradeResp.Body.Close()
	if tradeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on trade read, got %d", tradeResp.StatusCode)
	}

	settlementReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/settlements/si_456", nil)
	if err != nil {
		t.Fatal(err)
	}
	settlementReq.Header.Set("Authorization", valid)
	settlementResp, err := http.DefaultClient.Do(settlementReq)
	if err != nil {
		t.Fatal(err)
	}
	defer settlementResp.Body.Close()
	if settlementResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on settlement read, got %d", settlementResp.StatusCode)
	}

	balanceReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/balances", nil)
	if err != nil {
		t.Fatal(err)
	}
	balanceReq.Header.Set("Authorization", valid)
	balanceResp, err := http.DefaultClient.Do(balanceReq)
	if err != nil {
		t.Fatal(err)
	}
	defer balanceResp.Body.Close()
	if balanceResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on balances, got %d", balanceResp.StatusCode)
	}
	var balancePayload struct {
		InstitutionID string `json:"institution_id"`
		Balances      []struct {
			Currency       string `json:"currency"`
			AvailableMinor int64  `json:"available_minor"`
			HeldMinor      int64  `json:"held_minor"`
			TotalMinor     int64  `json:"total_minor"`
		} `json:"balances"`
	}
	if err := json.NewDecoder(balanceResp.Body).Decode(&balancePayload); err != nil {
		t.Fatal(err)
	}
	if balancePayload.InstitutionID != "inst_1" || len(balancePayload.Balances) != 2 ||
		balancePayload.Balances[0].Currency != "USD" || balancePayload.Balances[1].Currency != "NGN" {
		t.Fatalf("expected all institution currency balances, got %+v", balancePayload)
	}

	transactionsReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/transactions", nil)
	if err != nil {
		t.Fatal(err)
	}
	transactionsReq.Header.Set("Authorization", valid)
	transactionsResp, err := http.DefaultClient.Do(transactionsReq)
	if err != nil {
		t.Fatal(err)
	}
	defer transactionsResp.Body.Close()
	if transactionsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on transactions, got %d", transactionsResp.StatusCode)
	}

	forbiddenHeader := "ApiKey ck_ok:s3cr3t"
	forbiddenTradeReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/trades/trd_999", nil)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenTradeReq.Header.Set("Authorization", forbiddenHeader)
	forbiddenTradeResp, err := http.DefaultClient.Do(forbiddenTradeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer forbiddenTradeResp.Body.Close()
	if forbiddenTradeResp.StatusCode != http.StatusForbidden && forbiddenTradeResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected access control on cross-institution trade read, got %d", forbiddenTradeResp.StatusCode)
	}

	forbiddenBalanceReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/balances", nil)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenBalanceReq.Header.Set("Authorization", "ApiKey ck_ok:s3cr3t")
	forbiddenBalanceResp, err := http.DefaultClient.Do(forbiddenBalanceReq)
	if err != nil {
		t.Fatal(err)
	}
	defer forbiddenBalanceResp.Body.Close()
	if forbiddenBalanceResp.StatusCode != http.StatusOK && forbiddenBalanceResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected balance route to be institution-scoped, got %d", forbiddenBalanceResp.StatusCode)
	}

	if strings.Contains(readBody(t, tradeResp), "provider") {
		t.Fatalf("trade response should avoid provider-private fields")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 4096)
	_, err := resp.Body.Read(buf)
	if err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	return string(buf)
}
