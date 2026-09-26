package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/osai/osai/services/quote/quotecore"
)

func TestPhase5RuntimeMatrix(t *testing.T) {
	if err := os.Setenv("OSAI_POSTGRES_DSN", "postgres://osai:osai@localhost:5432/osai?sslmode=disable"); err != nil {
		t.Fatal(err)
	}
	store := quotecore.NewQuoteStore()
	gateway := NewGateway(store)
	customer := NewCustomerService()
	gateway.SetCustomerService(customer)

	instA, err := customer.CreateInstitution("Institution A", "US", "ACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	instB, err := customer.CreateInstitution("Institution B", "US", "ACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	credA, secretA, err := customer.CreateCredential(instA.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	credB, secretB, err := customer.CreateCredential(instB.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(credA.SecretHash, "secret") || strings.Contains(credB.SecretHash, "secret") {
		t.Fatalf("credentials should be hashed only; got secret material in DB: %s / %s", credA.SecretHash, credB.SecretHash)
	}
	server := httptest.NewServer(gateway)
	defer server.Close()

	validHeader := "ApiKey " + credA.PublicID + ":" + secretA
	body := `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"USD","destination_rail":"sim_lp_1","urgency":"normal"}`

	// valid auth
	resp := doJSON(t, server, http.MethodPost, "/v1/quotes", validHeader, "idem-valid-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid auth expected 200, got %d: %s", resp.StatusCode, resp.Body)
	}

	// same key same payload replays
	resp2 := doJSON(t, server, http.MethodPost, "/v1/quotes", validHeader, "idem-valid-1", body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("replay expected 200, got %d: %s", resp2.StatusCode, resp2.Body)
	}
	if resp2.Body["quote_id"] != resp.Body["quote_id"] {
		t.Fatalf("same key should return same logical response: %v != %v", resp2.Body["quote_id"], resp.Body["quote_id"])
	}

	// same key changed payload 409
	conflict := `{"base_amount_minor":20000,"base_currency":"NGN","quote_currency":"USD","destination_rail":"sim_lp_1","urgency":"normal"}`
	resp3 := doJSON(t, server, http.MethodPost, "/v1/quotes", validHeader, "idem-valid-1", conflict)
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("same idem key changed payload expected 409, got %d: %s", resp3.StatusCode, resp3.Body)
	}

	// missing credential
	missing := doJSON(t, server, http.MethodPost, "/v1/quotes", "ApiKey ck_missing:secret", "idem-missing", body)
	if missing.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing credential should fail auth, got %d", missing.StatusCode)
	}

	// invalid credential
	invalid := doJSON(t, server, http.MethodPost, "/v1/quotes", "ApiKey "+credA.PublicID+":wrong-secret", "idem-invalid", body)
	if invalid.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid credential should fail auth, got %d", invalid.StatusCode)
	}

	// revoked credential
	customer.publicToCred[credA.PublicID].Status = "REVOKED"
	revoked := doJSON(t, server, http.MethodPost, "/v1/quotes", validHeader, "idem-revoked", body)
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked credential should fail auth, got %d", revoked.StatusCode)
	}
	customer.publicToCred[credA.PublicID].Status = "ACTIVE"

	// disabled institution
	customer.institutions[instA.ID].Status = "DISABLED"
	disabled := doJSON(t, server, http.MethodPost, "/v1/quotes", validHeader, "idem-disabled", body)
	if disabled.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled institution should fail, got %d: %s", disabled.StatusCode, disabled.Body)
	}
	customer.institutions[instA.ID].Status = "ACTIVE"

	// quote access control
	quoteID := resp.Body["quote_id"].(string)
	getOK := doJSONGET(t, server, "/v1/quotes/"+quoteID, validHeader)
	if getOK.StatusCode != http.StatusOK {
		t.Fatalf("quote lookup should succeed, got %d: %s", getOK.StatusCode, getOK.Body)
	}
	otherHeader := "ApiKey " + credB.PublicID + ":" + secretB
	getForbidden := doJSONGET(t, server, "/v1/quotes/"+quoteID, otherHeader)
	if getForbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("institution B should not read A quote, got %d: %s", getForbidden.StatusCode, getForbidden.Body)
	}

	// accept flow and replay safety
	acceptBody := `{"quote_id":"` + quoteID + `"}`
	acceptOK := doJSON(t, server, http.MethodPost, "/v1/quotes/"+quoteID+"/accept", validHeader, "idem-accept-1", acceptBody)
	if acceptOK.StatusCode != http.StatusOK {
		t.Fatalf("accept should succeed, got %d: %s", acceptOK.StatusCode, acceptOK.Body)
	}
	acceptReplay := doJSON(t, server, http.MethodPost, "/v1/quotes/"+quoteID+"/accept", validHeader, "idem-accept-1", acceptBody)
	if acceptReplay.StatusCode != http.StatusOK {
		t.Fatalf("accept replay should succeed, got %d: %s", acceptReplay.StatusCode, acceptReplay.Body)
	}
	conflictAccept := doJSON(t, server, http.MethodPost, "/v1/quotes/"+quoteID+"/accept", validHeader, "idem-accept-1", `{"quote_id":"`+quoteID+`","force":"different"}`)
	if conflictAccept.StatusCode != http.StatusConflict {
		t.Fatalf("accept conflict should 409, got %d: %s", conflictAccept.StatusCode, conflictAccept.Body)
	}

	// no duplicates after replay in in-memory store at least for quote
	if store != nil {
		if got, ok := store.Get(quoteID); !ok || got.Status != quotecore.QuoteStatusAccepted {
			t.Fatalf("accepted quote did not persist status: ok=%v status=%v", ok, func() string {
				if !ok {
					return "missing"
				}
				return string(got.Status)
			}())
		}
	}
}

type responseEnvelope struct {
	StatusCode int
	Body       map[string]any
}

func doJSON(t *testing.T, server *httptest.Server, method, path, auth, key, body string) responseEnvelope {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		out = map[string]any{"raw": "unable to decode json"}
	}
	return responseEnvelope{StatusCode: resp.StatusCode, Body: out}
}

func doJSONGET(t *testing.T, server *httptest.Server, path, auth string) responseEnvelope {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		out = map[string]any{"raw": "unable to decode json"}
	}
	return responseEnvelope{StatusCode: resp.StatusCode, Body: out}
}
