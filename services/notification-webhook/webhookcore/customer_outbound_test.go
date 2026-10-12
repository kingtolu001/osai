package webhookcore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCustomerWebhookSigningAndDelivery(t *testing.T) {
	t.Setenv("OSAI_LOCAL_PROOF_MODE", "true")
	secret := "customer-secret"
	eventID := "evt_123"
	body := []byte(`{"trade_id":"trd_123"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature, err := SignCustomerPayload(secret, eventID, timestamp, body)
	if err != nil {
		t.Fatalf("sign outbound request: %v", err)
	}
	ok, err := VerifyCustomerPayload(secret, eventID, timestamp, body, signature)
	if err != nil || !ok {
		t.Fatalf("verification should pass: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyCustomerPayload(secret, "evt_other", timestamp, body, signature); ok {
		t.Fatal("modified event ID should not verify")
	}
	if ok, _ := VerifyCustomerPayload(secret, eventID, time.Now().Add(-6*time.Minute).UTC().Format(time.RFC3339), body, signature); ok {
		t.Fatal("stale timestamp should not verify")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Osai-Signature") == "" || r.Header.Get("X-Osai-Event-Id") == "" {
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

func TestCustomerWebhookRejectsPrivateTargetsOutsideLocalProof(t *testing.T) {
	t.Setenv("OSAI_LOCAL_PROOF_MODE", "")
	t.Setenv("OSAI_ENVIRONMENT", "production")
	for _, target := range []string{"http://127.0.0.1:8080/hook", "https://127.0.0.1/hook"} {
		if err := validateCustomerWebhookURL(context.Background(), target); err == nil {
			t.Fatalf("expected private webhook target to be rejected: %s", target)
		}
	}
}

func TestCustomerWebhookRedirectIsNotFollowed(t *testing.T) {
	t.Setenv("OSAI_LOCAL_PROOF_MODE", "true")
	redirectTargetHits := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetHits <- struct{}{}
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	attempt, err := DeliverCustomerEvent(redirect.Client(), CustomerWebhookConfig{
		InstitutionID: "inst_1",
		EndpointURL:   redirect.URL,
		Secret:        "customer-secret",
	}, OutboundEvent{
		EventID:       "evt_redirect",
		InstitutionID: "inst_1",
		EventType:     "trade.accepted",
		EventVersion:  "v1",
		CorrelationID: "corr_redirect",
		Payload:       `{"trade_id":"trd_1"}`,
		OccurredAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempt.HTTPStatus != http.StatusFound || attempt.Outcome != "failed" {
		t.Fatalf("redirect should be recorded as a failed delivery, got %+v", attempt)
	}
	select {
	case <-redirectTargetHits:
		t.Fatal("redirect target received a request")
	default:
	}
}
