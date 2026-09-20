//go:build ignore

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
  event := webhookcore.OutboundEvent{EventID: "evt_retry_proof_002", InstitutionID: "inst_sandbox_local", EventType: "settlement.confirmed", EventVersion: "v1", CorrelationID: "corr_retry_002", Payload: `{"trade_id":"trd_123"}`, OccurredAt: time.Now().UTC()}
  store := webhookcore.NewDeliveryStore(); store.RegisterConfig(cfg); store.EnqueueEvent(event)
  first, err1 := webhookcore.DeliverCustomerEvent(srv.Client(), cfg, event)
  fmt.Println("ATTEMPT1_ERR:", err1)
  fmt.Println("ATTEMPT1_HTTP:", first.HTTPStatus, "OUTCOME:", first.Outcome, "ATTEMPT_NO:", first.AttemptNumber)
  store.RecordAttempt(*first)
  second, err2 := webhookcore.DeliverCustomerEvent(srv.Client(), cfg, event)
  fmt.Println("ATTEMPT2_ERR:", err2)
  fmt.Println("ATTEMPT2_HTTP:", second.HTTPStatus, "OUTCOME:", second.Outcome, "ATTEMPT_NO:", second.AttemptNumber)
  store.RecordAttempt(*second)
  out, _ := json.MarshalIndent(map[string]any{"event_id": event.EventID, "attempts": store.Attempts(event.EventID), "calls": calls}, "", "  ")
  fmt.Println(string(out))
}
