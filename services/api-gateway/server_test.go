package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osai/osai/services/quote/quotecore"
)

type recordingQuoteClient struct {
	stubQuoteClient
	requests []QuoteRequest
}

func (c *recordingQuoteClient) CreateQuote(request QuoteRequest) (QuoteResponse, error) {
	c.requests = append(c.requests, request)
	return c.stubQuoteClient.CreateQuote(request)
}

func TestGatewayQuoteFlowAndIdempotency(t *testing.T) {
	store := quotecore.NewQuoteStore()
	gateway := NewGateway(store)
	customer := NewCustomerService()
	gateway.SetCustomerService(customer)
	server := httptest.NewServer(gateway)
	defer server.Close()

	inst, err := customer.CreateInstitution("Test Institution", "US", "ACTIVE")
	if err != nil {
		t.Fatalf("create institution: %v", err)
	}

	cred, secret, err := customer.CreateCredential(inst.ID, "test", "sandbox")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	clientHeader := cred.PublicID + ":" + secret

	quoteReq := map[string]any{
		"base_amount_minor": 10000,
		"base_currency":     "NGN",
		"quote_currency":    "USD",
		"destination_rail":  "sim_lp_1",
		"urgency":           "normal",
	}
	body, _ := json.Marshal(quoteReq)

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/quotes", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "ApiKey "+clientHeader)
	request.Header.Set("Idempotency-Key", "idem-quote-1")
	request.Header.Set("X-Osai-Correlation-Id", "req-otel-phase5-final")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("quote POST request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on quote create, got %d", response.StatusCode)
	}
	if got := response.Header.Get("X-Osai-Correlation-Id"); got != "req-otel-phase5-final" {
		t.Fatalf("expected correlation header to be preserved, got %q", got)
	}

	first := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatalf("decode quote response: %v", err)
	}
	quoteID := first["quote_id"].(string)

	request, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/quotes", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "ApiKey "+clientHeader)
	request.Header.Set("Idempotency-Key", "idem-quote-1")
	request.Header.Set("X-Osai-Correlation-Id", "req-otel-phase5-final")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("second quote POST request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected replay to return 200, got %d", response.StatusCode)
	}

	quoteReq["base_amount_minor"] = 20000
	body, _ = json.Marshal(quoteReq)
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/quotes", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "ApiKey "+clientHeader)
	request.Header.Set("Idempotency-Key", "idem-quote-1")
	request.Header.Set("X-Osai-Correlation-Id", "req-otel-phase5-final")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("conflicting quote request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on idempotency conflict, got %d", response.StatusCode)
	}

	acceptReq := map[string]any{"idempotency_key": "idem-accept-1"}
	acceptBody, _ := json.Marshal(acceptReq)
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/quotes/"+quoteID+"/accept", strings.NewReader(string(acceptBody)))
	request.Header.Set("Authorization", "ApiKey "+clientHeader)
	request.Header.Set("Idempotency-Key", "idem-accept-1")
	request.Header.Set("X-Osai-Correlation-Id", "req-otel-phase5-final")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("accept request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on quote accept, got %d", response.StatusCode)
	}
}

func TestQuoteAmountUsesExactIntegerMinorUnits(t *testing.T) {
	quoteClient := &recordingQuoteClient{}
	gateway := NewGatewayWithClients(stubCustomerClient{}, quoteClient)
	gateway.idempotency = &idempotencyStore{rows: map[string]*idempotencyEntry{}}
	server := httptest.NewServer(gateway)
	defer server.Close()
	auth := "ApiKey ck_ok:s3cr3t"
	largeMinorAmount := `{"base_amount_minor":9007199254740993,"base_currency":"NGN","quote_currency":"USD"}`
	response := doJSON(t, server, http.MethodPost, "/v1/quotes", auth, "exact-minor-units", largeMinorAmount)
	if response.StatusCode != http.StatusOK || len(quoteClient.requests) != 1 || quoteClient.requests[0].BaseAmountMinor != 9007199254740993 {
		t.Fatalf("minor-unit integer was not preserved: status=%d captured=%+v body=%v", response.StatusCode, quoteClient.requests, response.Body)
	}
	fractional := doJSON(t, server, http.MethodPost, "/v1/quotes", auth, "fractional-minor-units", `{"base_amount_minor":100.5,"base_currency":"NGN","quote_currency":"USD"}`)
	if fractional.StatusCode != http.StatusBadRequest || len(quoteClient.requests) != 1 {
		t.Fatalf("fractional minor units should be rejected before calling quote service: status=%d calls=%d body=%v", fractional.StatusCode, len(quoteClient.requests), fractional.Body)
	}
}

func TestRateLimitingPerInstitution(t *testing.T) {
	gateway := NewGateway(quotecore.NewQuoteStore())
	customer := NewCustomerService()
	gateway.SetCustomerService(customer)
	server := httptest.NewServer(gateway)
	defer server.Close()

	instA, _ := customer.CreateInstitution("Inst A", "US", "ACTIVE")
	credA, secretA, _ := customer.CreateCredential(instA.ID, "test", "sandbox")
	headerA := "ApiKey " + credA.PublicID + ":" + secretA

	instB, _ := customer.CreateInstitution("Inst B", "US", "ACTIVE")
	credB, secretB, _ := customer.CreateCredential(instB.ID, "test", "sandbox")
	headerB := "ApiKey " + credB.PublicID + ":" + secretB

	body := `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"USD","destination_rail":"sim_lp_1"}`

	// Burst 11 requests from A (burst limit is 10, so 11th should be rejected)
	okCount := 0
	for i := 0; i < 11; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/quotes", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", headerA)
		req.Header.Set("Idempotency-Key", fmt.Sprintf("idem-a-%d", i))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			okCount++
		} else if resp.StatusCode != http.StatusTooManyRequests {
			t.Logf("unexpected status for request %d: %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if okCount != 10 {
		t.Fatalf("expected exactly 10 successful requests (burst limit), got %d", okCount)
	}

	// B should still have full burst capacity (institutions are independent)
	respB, err := http.NewRequest(http.MethodPost, server.URL+"/v1/quotes", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respB.Header.Set("Authorization", headerB)
	respB.Header.Set("Idempotency-Key", "idem-b-1")
	respB.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(respB)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("institution B should not be rate limited; got %d", httpResp.StatusCode)
	}
}

func TestRateLimiterTracksInstitutionAndCredentialScopes(t *testing.T) {
	limiter := newRateLimiterStore()
	for i := 0; i < int(rateLimitBurst); i++ {
		if !limiter.allow("institution:inst_a", "credential:client_a") {
			t.Fatalf("request %d unexpectedly rate limited", i+1)
		}
	}
	if limiter.allow("institution:inst_a", "credential:client_a") {
		t.Fatal("same institution and credential should be rate limited after burst")
	}
	if limiter.allow("institution:inst_a", "credential:client_b") {
		t.Fatal("institution bucket should limit a second credential in the same institution")
	}
	if !limiter.allow("institution:inst_b", "credential:client_b") {
		t.Fatal("credential bucket should be independent across institutions")
	}
	if limiter.allow("institution:inst_c", "credential:client_a") {
		t.Fatal("credential bucket should limit reuse across institutions")
	}
}
