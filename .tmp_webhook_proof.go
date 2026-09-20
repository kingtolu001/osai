package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "net/http/httptest"
    "time"

    webhookcore "github.com/osai/osai/services/notification-webhook/webhookcore"
)

func main() {
    var calls int
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        calls++
        if calls == 1 {
            w.WriteHeader(http.StatusInternalServerError)
            _, _ = w.Write([]byte(`{"error":"transient"}`))
            return
        }
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte(`{"ok":true}`))
    }))
    defer srv.Close()

    cfg := webhookcore.CustomerWebhookConfig{InstitutionID: "inst_sandbox_local", EndpointURL: srv.URL, Secret: "customer-secret"}
    event := webhookcore.OutboundEvent{EventID: "evt_retry_proof", InstitutionID: "inst_sandbox_local", EventType: "settlement.confirmed", EventVersion: "v1", CorrelationID: "corr_retry_001", Payload: `{"trade_id":"trd_123"}`, OccurredAt: time.Now().UTC()}
    store := webhookcore.NewDeliveryStore()
    store.RegisterConfig(cfg)
    store.EnqueueEvent(event)

    a1, err := webhookcore.DeliverCustomerEvent(srv.Client(), cfg, event)
    fmt.Println("ATTEMPT_1_ERR=", err)
    fmt.Println("ATTEMPT_1_STATUS=", a1.HTTPStatus, "OUTCOME=", a1.Outcome, "EVENT_ID=", a1.EventID, "ATTEMPT_NO=", a1.AttemptNumber)
    store.RecordAttempt(*a1)

    a2, err := webhookcore.DeliverCustomerEvent(srv.Client(), cfg, event)
    fmt.Println("ATTEMPT_2_ERR=", err)
    fmt.Println("ATTEMPT_2_STATUS=", a2.HTTPStatus, "OUTCOME=", a2.Outcome, "EVENT_ID=", a2.EventID, "ATTEMPT_NO=", a2.AttemptNumber)
    store.RecordAttempt(*a2)

    attempts := store.Attempts(event.EventID)
    out, _ := json.MarshalIndent(map[string]any{"event_id": event.EventID, "attempt_count": len(attempts), "attempts": attempts}, "", "  ")
    fmt.Println(string(out))
    fmt.Println("CALLS=", calls)
}