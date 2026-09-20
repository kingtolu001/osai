package webhookcore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osai/osai/adapters/simulator/simcore"
	"github.com/osai/osai/pkg/provider"
)

type signaler struct {
	calls int
	ref   string
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
