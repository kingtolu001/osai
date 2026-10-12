package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osai/osai/services/quote/quotecore"
)

func TestLoadPricingOperatorsHashesSecretsAndRequiresBothRoles(t *testing.T) {
	t.Setenv("OSAI_QUOTE_PRICING_OPERATORS_JSON", `[{"actor_id":"maker-1","token":"maker-token-123456789012345678901234","role":"maker","institutions":["inst-1"]},{"actor_id":"checker-1","token":"checker-token-12345678901234567890123","role":"checker","institutions":["inst-1"]}]`)
	operators, err := loadPricingOperators()
	if err != nil {
		t.Fatal(err)
	}
	if len(operators) != 2 || operators[0].Token != "" || operators[0].tokenHash == ([32]byte{}) {
		t.Fatalf("operator secrets were not hashed and cleared: %+v", operators)
	}

	t.Setenv("OSAI_QUOTE_PRICING_OPERATORS_JSON", `[{"actor_id":"maker-1","token":"maker-token-123456789012345678901234","role":"maker","institutions":["inst-1"]}]`)
	if _, err = loadPricingOperators(); err == nil {
		t.Fatal("pricing operator configuration without a checker must be rejected")
	}

	t.Setenv("OSAI_QUOTE_PRICING_OPERATORS_JSON", `[{"actor_id":"same","token":"maker-token-123456789012345678901234","role":"maker","institutions":["inst-1"]},{"actor_id":"same","token":"checker-token-12345678901234567890123","role":"checker","institutions":["inst-1"]}]`)
	if _, err = loadPricingOperators(); err == nil {
		t.Fatal("maker and checker must have distinct actor identities")
	}
}

func TestPricingOperatorHandlerEnforcesRoleAndInstitutionScope(t *testing.T) {
	t.Setenv("OSAI_QUOTE_PRICING_OPERATORS_JSON", `[{"actor_id":"maker-1","token":"maker-token-123456789012345678901234","role":"maker","institutions":["inst-1"]},{"actor_id":"checker-1","token":"checker-token-12345678901234567890123","role":"checker","institutions":["inst-1"]}]`)
	operators, err := loadPricingOperators()
	if err != nil {
		t.Fatal(err)
	}
	server := &grpcServer{store: quotecore.NewQuoteStore()}
	handler := server.pricingOperatorHandler(operators)
	proposal := `{"fee_schedule":{"institution_id":"inst-1","source_currency":"USD","destination_currency":"NGN"},"reason":"test"}`

	cases := []struct {
		name   string
		path   string
		token  string
		body   string
		status int
	}{
		{name: "missing token", path: "/v1/operator/pricing/propose", body: proposal, status: http.StatusUnauthorized},
		{name: "wrong role", path: "/v1/operator/pricing/approve", token: "maker-token-123456789012345678901234", body: `{"proposal_id":"fee-1","institution_id":"inst-1","reason":"test"}`, status: http.StatusUnauthorized},
		{name: "institution out of scope", path: "/v1/operator/pricing/propose", token: "maker-token-123456789012345678901234", body: strings.Replace(proposal, "inst-1", "inst-2", 1), status: http.StatusUnauthorized},
		{name: "unknown fields", path: "/v1/operator/pricing/propose", token: "maker-token-123456789012345678901234", body: `{"fee_schedule":{"institution_id":"inst-1","unexpected":true},"reason":"test"}`, status: http.StatusBadRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestPricingAdminRequiresTLSOffLoopback(t *testing.T) {
	if err := servePricingAdmin("0.0.0.0:0", "", "", http.NewServeMux()); err == nil ||
		!strings.Contains(err.Error(), "TLS is required") {
		t.Fatalf("remote admin listener should require TLS, got %v", err)
	}
	if err := servePricingAdmin("127.0.0.1:0", "cert.pem", "", http.NewServeMux()); err == nil ||
		!strings.Contains(err.Error(), "both") {
		t.Fatalf("partial TLS configuration should fail, got %v", err)
	}
}

func TestPricingOperatorConfigurationMissingFailsClosed(t *testing.T) {
	t.Setenv("OSAI_QUOTE_PRICING_OPERATORS_JSON", "")
	operators, err := loadPricingOperators()
	if err != nil || len(operators) != 0 {
		t.Fatalf("empty operator configuration should disable administration without credentials: %+v %v", operators, err)
	}
}
