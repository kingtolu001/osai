package sandboxpayout

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osai/osai/pkg/sandboxaccounting"
)

func TestAuthenticatedPayoutQueryBindsInstructionAndRejectsConflictingEvidence(t *testing.T) {
	key := "local-payout-fixture-signing-key-123456789"
	i := Instruction{InstitutionID: "sbx:institution", TradeID: "trd_fixture", Binding: sandboxaccounting.PayoutBinding{SettlementID: "si_fixture", ClientReference: "si_reference", ProviderID: "sbx:provider", BeneficiaryID: "sbx:beneficiary", Currency: "USD", AmountMinor: 95}}
	for _, kind := range []string{"valid", "wrong_amount", "wrong_beneficiary", "wrong_nonce", "old", "future", "unsigned", "unknown_status", "trailing_json"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var request Request
				if json.Unmarshal(raw, &request) != nil || r.Header.Get("X-Osai-Sandbox-Payout-Signature") != hex.EncodeToString(mac([]byte(key), raw)) {
					t.Error("request authentication missing")
					w.WriteHeader(401)
					return
				}
				p := Payload{Instruction: request.Instruction, QueryReference: request.Nonce, Status: "SUCCEEDED", ProviderReference: "sbx:provider-reference", ObservedAt: time.Now().UTC()}
				switch kind {
				case "wrong_amount":
					p.Instruction.Binding.AmountMinor++
				case "wrong_beneficiary":
					p.Instruction.Binding.BeneficiaryID = "sbx:other"
				case "wrong_nonce":
					p.QueryReference = "old-query"
				case "old":
					p.ObservedAt = p.ObservedAt.Add(-6 * time.Minute)
				case "future":
					p.ObservedAt = p.ObservedAt.Add(time.Minute)
				case "unknown_status":
					p.Status = "made-up"
				}
				payload, _ := json.Marshal(p)
				signature := hex.EncodeToString(mac([]byte(key), payload))
				if kind == "unsigned" {
					signature = "00"
				}
				_ = json.NewEncoder(w).Encode(Envelope{Payload: payload, Signature: signature})
				if kind == "trailing_json" {
					_, _ = io.WriteString(w, `{}`)
				}
			}))
			defer server.Close()
			c, err := New(server.URL, key)
			if err != nil {
				t.Fatal(err)
			}
			observation, err := c.Query(context.Background(), i)
			if kind != "valid" {
				if err == nil {
					t.Fatal("conflicting evidence accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.VerifyStored(i, observation.Raw); err != nil {
				t.Fatal("durable signed evidence failed verification", err)
			}
			observation.Raw[len(observation.Raw)/2] ^= 1
			if _, err = c.VerifyStored(i, observation.Raw); err == nil {
				t.Fatal("corrupted durable evidence accepted")
			}
		})
	}
}

func TestPayoutClientRejectsNonlocalConfiguration(t *testing.T) {
	for _, base := range []string{"https://api.flutterwave.com/v3", "http://example.com", "http://127.0.0.1/other", "http://user@127.0.0.1", "http://127.0.0.1?redirect=1"} {
		if _, err := New(base, "local-payout-fixture-signing-key-123456789"); err == nil {
			t.Fatalf("unsafe fixture endpoint accepted: %s", base)
		}
	}
}
