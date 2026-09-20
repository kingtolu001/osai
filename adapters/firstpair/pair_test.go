package firstpair

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
)

func TestCommonAdapterContractAgainstFirstPair(t *testing.T) {
	pair := New()
	instruction := provider.TransferInstruction{ClientRef: "si_contract_1", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD"}
	first, err := pair.Settlement.CreateTransfer(instruction)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pair.Settlement.CreateTransfer(instruction)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderRef == "" || first.ProviderRef != second.ProviderRef {
		t.Fatalf("client_ref must be idempotent: %+v %+v", first, second)
	}

	if health := pair.Settlement.Health(); !health.Up || !health.Capabilities["payout"] {
		t.Fatalf("unexpected health: %+v", health)
	}

	raw := []byte(`{"type":"transfer.success","client_ref":"si_contract_1","status":"SUCCESS","amount_minor":"100","currency":"USD","beneficiary":"acct_1"}`)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	mac := hmac.New(sha256.New, []byte("sandbox-secret"))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(raw)
	headers := map[string]string{"X-Osai-Timestamp": timestamp, "X-Osai-Signature": hex.EncodeToString(mac.Sum(nil)), "X-Provider-Event-Id": "evt_contract_1"}
	event, err := pair.Settlement.VerifyWebhook(raw, headers)
	if err != nil || event.Status != provider.TransferConfirmed {
		t.Fatalf("webhook normalization failed: %+v %v", event, err)
	}
	duplicate, err := pair.Settlement.VerifyWebhook(raw, headers)
	if err != nil || duplicate.ProviderEventID != event.ProviderEventID {
		t.Fatalf("webhook replay was not deduped: %+v %v", duplicate, err)
	}
}
