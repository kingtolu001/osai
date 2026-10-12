package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayRejectsMissingRuntimeDependencies(t *testing.T) {
	gw := &Gateway{}

	if _, err := gw.authenticateHeader("ApiKey ck_123:secret"); err == nil || !strings.Contains(err.Error(), "customer service unavailable") {
		t.Fatalf("expected missing customer dependency rejection, got %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/quotes", strings.NewReader(`{"base_amount_minor":1000,"base_currency":"NGN","quote_currency":"USD"}`))
	req.Header.Set("Idempotency-Key", "idem-missing-quote")
	res := httptest.NewRecorder()
	gw.handleCreateQuote(res, req, "inst_missing", "req_missing")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when quote dependency is missing, got %d", res.Code)
	}
}
