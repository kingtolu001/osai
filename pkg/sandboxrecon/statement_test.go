package sandboxrecon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatementClientRejectsUnverifiedOrMismatchedEvidence(t *testing.T) {
	key := "local-" + strings.Repeat("s", 40)
	for _, mode := range []string{"valid", "bad_signature", "wrong_nonce", "wrong_institution", "wrong_trade", "stale", "future", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request Request
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					w.WriteHeader(400)
					return
				}
				statement := Statement{Snapshot: Snapshot{InstitutionID: request.InstitutionID, TradeID: request.TradeID}, Nonce: request.Nonce, ObservedAt: time.Now().UTC()}
				switch mode {
				case "wrong_nonce":
					statement.Nonce = "other"
				case "wrong_institution":
					statement.Snapshot.InstitutionID = "sbx:foreign"
				case "wrong_trade":
					statement.Snapshot.TradeID = "trd_other"
				case "stale":
					statement.ObservedAt = time.Now().Add(-10 * time.Minute)
				case "future":
					statement.ObservedAt = time.Now().Add(time.Minute)
				case "oversize":
					w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
					return
				}
				payload, _ := json.Marshal(statement)
				signature := MAC(payload, key)
				if mode == "bad_signature" {
					signature = strings.Repeat("0", 64)
				}
				json.NewEncoder(w).Encode(Envelope{Payload: payload, Signature: signature})
			}))
			defer server.Close()
			client, err := New(server.URL, key)
			if err != nil {
				t.Fatal(err)
			}
			_, hash, err := client.Read(context.Background(), "sbx:institution", "trd_fixture")
			if (mode == "valid") != (err == nil) {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
			if mode == "valid" && len(hash) != 64 {
				t.Fatal("raw statement hash missing")
			}
		})
	}
	for _, target := range []string{"https://example.com", "http://127.0.0.1@public.example", "http://127.0.0.1/?secret=x"} {
		if _, err := New(target, key); err == nil {
			t.Fatal("unsafe fixture endpoint admitted")
		}
	}
}
