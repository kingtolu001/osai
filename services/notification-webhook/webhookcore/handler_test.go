package webhookcore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/adapters/simulator/simcore"
	"github.com/osai/osai/pkg/provider"
)

type signaler struct {
	calls int
	ref   string
}

func TestFlutterwaveDuplicateAfterHandlerRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL DSN not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skip("PostgreSQL unavailable")
	}
	inbox := PostgresProviderInbox{DB: db}
	if err := inbox.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	id := time.Now().UTC().UnixNano()
	body := fmt.Sprintf(`{"event":"transfer.completed","data":{"id":%d,"created_at":"2021-04-28T17:01:41.000Z","currency":"NGN","amount":100,"status":"SUCCESSFUL","reference":"si_restart"}}`, id)
	receiver := &signaler{}
	for index := 0; index < 2; index++ {
		handler := NewHandler(map[string]provider.SettlementRail{"flutterwave": flutterwave.New(flutterwave.Config{SecretHash: "local-test-hash"})}, receiver)
		handler.SetProviderInbox(inbox)
		request := httptest.NewRequest(http.MethodPost, "/v1/webhooks/providers/flutterwave", strings.NewReader(body))
		request.Header.Set("Verif-Hash", "local-test-hash")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("delivery %d returned %d", index, response.Code)
		}
	}
	if receiver.calls != 1 {
		t.Fatalf("restart replay produced %d signals", receiver.calls)
	}
}

func TestFlutterwaveWebhookRoute(t *testing.T) {
	const body = `{"event":"transfer.completed","data":{"id":8416497,"created_at":"2021-04-28T17:01:41.000Z","currency":"NGN","amount":100,"status":"SUCCESSFUL","reference":"si_1"}}`
	receiver := &signaler{}
	handler := NewHandler(map[string]provider.SettlementRail{"flutterwave": flutterwave.New(flutterwave.Config{SecretHash: "local-test-hash"})}, receiver)
	request := func(body, hash string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/providers/flutterwave", strings.NewReader(body))
		if hash != "" {
			r.Header.Set("Verif-Hash", hash)
		}
		return r
	}
	for _, tc := range []struct {
		name, body, hash string
		status           int
	}{
		{"missing hash", body, "", http.StatusUnauthorized},
		{"wrong hash", body, "wrong", http.StatusUnauthorized},
		{"malformed", `{`, "local-test-hash", http.StatusBadRequest},
		{"unsupported", `{"event":"charge.completed","data":{}}`, "local-test-hash", http.StatusOK},
		{"valid", body, "local-test-hash", http.StatusOK},
		{"duplicate", body, "local-test-hash", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request(tc.body, tc.hash))
			if response.Code != tc.status {
				t.Fatalf("got %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
		})
	}
	if receiver.calls != 1 || receiver.ref != "si_1" {
		t.Fatalf("duplicate signal: calls=%d ref=%s", receiver.calls, receiver.ref)
	}
	missingConfig := NewHandler(map[string]provider.SettlementRail{"flutterwave": flutterwave.New(flutterwave.Config{})}, receiver)
	response := httptest.NewRecorder()
	missingConfig.ServeHTTP(response, request(body, "local-test-hash"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured provider returned %d", response.Code)
	}
}

func (s *signaler) SignalSettlement(_ context.Context, ref string, _ provider.WebhookEvent) error {
	s.calls++
	s.ref = ref
	return nil
}

func TestProviderWebhookRouteVerifiesAndSignalsOnce(t *testing.T) {
	adapter := simcore.NewNormalizedAdapter(simcore.FailureProfile{})
	receiver := &signaler{}
	handler := NewHandler(map[string]provider.SettlementRail{"sim_lp_1": adapter}, receiver)
	raw := []byte(`{"type":"transfer.success","client_ref":"si_1","status":"SUCCESS","amount_minor":"100","currency":"USD","beneficiary":"acct_1"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	mac := hmac.New(sha256.New, []byte("sandbox-secret"))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(raw)
	request := httptest.NewRequest("POST", "/v1/webhooks/providers/sim_lp_1", bytes.NewReader(raw))
	request.Header.Set("X-Osai-Timestamp", timestamp)
	request.Header.Set("X-Osai-Signature", hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-Provider-Event-Id", "evt_1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || receiver.calls != 1 || receiver.ref != "si_1" {
		t.Fatalf("unexpected valid webhook: code=%d calls=%d ref=%s", response.Code, receiver.calls, receiver.ref)
	}
	request = httptest.NewRequest("POST", "/v1/webhooks/providers/sim_lp_1", bytes.NewReader(raw))
	request.Header.Set("X-Osai-Timestamp", timestamp)
	request.Header.Set("X-Osai-Signature", hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-Provider-Event-Id", "evt_1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || receiver.calls != 1 {
		t.Fatalf("unexpected duplicate webhook: code=%d calls=%d", response.Code, receiver.calls)
	}
}

func TestProviderWebhookRouteRejectsInvalidAndUnknownRequests(t *testing.T) {
	adapter := simcore.NewNormalizedAdapter(simcore.FailureProfile{})
	handler := NewHandler(map[string]provider.SettlementRail{"sim_lp_1": adapter}, &signaler{})
	request := httptest.NewRequest("POST", "/v1/webhooks/providers/nope", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected unknown provider 404, got %d", response.Code)
	}
	request = httptest.NewRequest("POST", "/v1/webhooks/providers/sim_lp_1", strings.NewReader("{}"))
	request.Header.Set("X-Osai-Timestamp", time.Now().UTC().Format(time.RFC3339))
	request.Header.Set("X-Osai-Signature", "bad")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid signature 401, got %d", response.Code)
	}
	request = httptest.NewRequest("POST", "/v1/webhooks/providers/sim_lp_1", strings.NewReader("{}"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected malformed verification 401, got %d", response.Code)
	}
}
