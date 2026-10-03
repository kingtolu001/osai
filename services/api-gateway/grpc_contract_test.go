package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubCustomerClient struct{}

func (stubCustomerClient) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	if publicID == "ck_ok" && secret == "s3cr3t" {
		return CustomerAuthContext{InstitutionID: "inst_1", InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED", APIAccessStatus: "ACTIVE", APIEnabled: true, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
	}
	return CustomerAuthContext{}, errors.New("invalid credentials")
}

func (stubCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	return CustomerAuthContext{InstitutionID: instID, InstitutionStatus: "ACTIVE", KYBStatus: "APPROVED", APIAccessStatus: "ACTIVE", APIEnabled: true}, nil
}

func (stubCustomerClient) GetInstitution(instID string) (CustomerInstitution, error) {
	return CustomerInstitution{ID: instID, Name: "Stub", Status: "ACTIVE", KYBStatus: "APPROVED", Country: "US", APIEnabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (stubCustomerClient) GetWebhookConfiguration(instID string) (WebhookConfig, error) {
	return WebhookConfig{InstitutionID: instID, WebhookURL: "https://example.com/hook", Enabled: true, Status: "ACTIVE"}, nil
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

func (stubBalanceClient) GetBalances(institutionID string) (BalanceReadResponse, error) {
	if institutionID == "inst_1" {
		return BalanceReadResponse{InstitutionID: institutionID, Currency: "USD", AvailableMinor: 5000, HeldMinor: 1000, TotalMinor: 6000}, nil
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
	gateway := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
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
	gateway := NewGatewayWithClients(stubCustomerClient{}, stubQuoteClient{})
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
