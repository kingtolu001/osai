package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osai/osai/services/quote/quotecore"
)

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
	cred, secret, err := customer.CreateCredential(inst.ID, "sandbox")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	clientHeader := cred.PublicID + ":" + secret

	quoteReq := map[string]any{
		"base_amount_minor": 10000,
		"base_currency": "NGN",
		"quote_currency": "USD",
		"destination_rail": "sim_lp_1",
		"urgency": "normal",
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
