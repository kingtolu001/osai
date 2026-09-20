package webhookcore

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCustomerWebhookSigningAndDelivery(t *testing.T) {
	secret := "customer-secret"
	body := []byte(`{"trade_id":"trd_123"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature, err := SignCustomerPayload(secret, timestamp, body)
	if err != nil {
		t.Fatalf("sign outbound request: %v", err)
	}
	ok, err := VerifyCustomerPayload(secret, timestamp, body, signature)
	if err != nil || !ok {
		t.Fatalf("verification should pass: ok=%v err=%v", ok, err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Osai-Signature") == "" {
			t.Fatalf("missing signature header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := CustomerWebhookConfig{InstitutionID: "inst_1", EndpointURL: server.URL, Secret: secret}
	event := OutboundEvent{EventID: "evt_123", InstitutionID: "inst_1", EventType: "trade.status_changed", EventVersion: "v1", CorrelationID: "corr_123", Payload: `{"trade_id":"trd_123"}`, OccurredAt: time.Now().UTC()}
	attempt, err := DeliverCustomerEvent(server.Client(), cfg, event)
	if err != nil {
		t.Fatalf("delivery should succeed: %v", err)
	}
	if attempt == nil || attempt.EventID != event.EventID {
		t.Fatal("attempt should reference the logical event")
	}
	if attempt.HTTPStatus != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", attempt.HTTPStatus)
	}
	if attempt.Outcome != "success" {
		t.Fatalf("expected success outcome, got %s", attempt.Outcome)
	}

	store := NewDeliveryStore()
	store.RegisterConfig(cfg)
	store.EnqueueEvent(event)
	store.RecordAttempt(*attempt)
	if got := store.Event(event.EventID); got == nil || got.EventType != event.EventType {
		t.Fatal("delivery store should persist the logical outbound event")
	}
	if attempts := store.Attempts(event.EventID); len(attempts) != 1 {
		t.Fatalf("expected one delivery attempt, got %d", len(attempts))
	}
	fmt.Println("customer webhook delivery verification ok")
}
