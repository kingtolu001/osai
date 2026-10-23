package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Secrets stay in process memory and request headers. Failure messages never
// include credential response bodies, keys or authorization headers.
func verifyPublicCredentialLifecycle(t *testing.T, api, authority, correlation string, restartCustomer func()) {
	t.Helper()
	call := func(method, path, auth, body string) (int, map[string]any) {
		return proofHTTP(t, method, api+path, auth, "", correlation, body)
	}
	code, created := call(http.MethodPost, "/v1/credentials", authority, `{"label":"integration child","environment":"sandbox"}`)
	id, _ := created["public_id"].(string)
	secret, _ := created["secret"].(string)
	if code != http.StatusCreated || id == "" || secret == "" {
		t.Fatalf("public sandbox credential creation failed: code=%d", code)
	}
	oldAuth := "ApiKey " + id + ":" + secret
	if code, _ := call(http.MethodGet, "/v1/providers/status", oldAuth, ""); code != http.StatusOK {
		t.Fatalf("new credential authentication failed: code=%d", code)
	}
	code, rotated := call(http.MethodPost, "/v1/credentials/"+id+"/rotate", authority, "{}")
	nextSecret, _ := rotated["secret"].(string)
	if code != http.StatusOK || nextSecret == "" || nextSecret == secret {
		t.Fatalf("public credential rotation failed: code=%d", code)
	}
	newAuth := "ApiKey " + id + ":" + nextSecret
	if code, _ := call(http.MethodGet, "/v1/providers/status", oldAuth, ""); code != http.StatusUnauthorized {
		t.Fatalf("old secret survived rotation: code=%d", code)
	}
	restartCustomer()
	if code, _ := call(http.MethodGet, "/v1/providers/status", newAuth, ""); code != http.StatusOK {
		t.Fatalf("rotated credential did not survive restart: code=%d", code)
	}
	if code, _ := call(http.MethodGet, "/v1/providers/status", oldAuth, ""); code != http.StatusUnauthorized {
		t.Fatalf("restart restored the old secret: code=%d", code)
	}
	code, listed := call(http.MethodGet, "/v1/credentials", authority, "")
	raw, err := json.Marshal(listed)
	if err != nil || code != http.StatusOK || strings.Contains(string(raw), secret) || strings.Contains(string(raw), nextSecret) {
		t.Fatal("credential metadata listing failed or exposed a secret")
	}
	if code, _ := call(http.MethodDelete, "/v1/credentials/"+id, authority, ""); code != http.StatusOK {
		t.Fatalf("public credential revocation failed: code=%d", code)
	}
	restartCustomer()
	if code, _ := call(http.MethodGet, "/v1/providers/status", newAuth, ""); code != http.StatusUnauthorized {
		t.Fatalf("revoked credential survived restart: code=%d", code)
	}
	t.Log("CREDENTIAL_LIFECYCLE public create/rotate/revoke and restart rejection verified; secrets withheld")
}
