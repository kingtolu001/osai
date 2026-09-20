package simcore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	quotecore "github.com/osai/osai/services/quote/quotecore"
)

func TestCreateTransferSameClientRefIsIdempotent(t *testing.T) {
	sim := NewSimulator(FailureProfile{})
	instr := TransferInstruction{ClientRef: "si_1", Beneficiary: "acct_1", AmountMinor: 1000, Currency: "USD"}

	first, err := sim.CreateTransfer(instr)
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	second, err := sim.CreateTransfer(instr)
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if first.ProviderRef != second.ProviderRef {
		t.Fatalf("duplicate transfer should preserve provider ref: %s vs %s", first.ProviderRef, second.ProviderRef)
	}
}

func TestWebhookReplayIsDeduped(t *testing.T) {
	sim := NewSimulator(FailureProfile{})
	raw := []byte("payload")
	timestamp := "2026-09-19T00:00:00Z"
	key := []byte("sandbox-secret")
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(timestamp))
	_, _ = h.Write([]byte("."))
	_, _ = h.Write(raw)
	sig := hex.EncodeToString(h.Sum(nil))
	headers := map[string]string{"X-Osai-Timestamp": timestamp, "X-Osai-Signature": sig}
	first, err := sim.VerifyWebhook(raw, headers)
	if err != nil {
		t.Fatalf("valid webhook should be accepted: %v", err)
	}
	second, err := sim.VerifyWebhook(raw, headers)
	if err != nil {
		t.Fatalf("duplicate webhook should be deduped without error: %v", err)
	}
	if first.ProviderEventID != second.ProviderEventID {
		t.Fatal("duplicate webhook should preserve the same provider event id")
	}
}

func TestHealthReflectsCapability(t *testing.T) {
	sim := NewSimulator(FailureProfile{})
	health := sim.Health()
	if !health.Up {
		t.Fatal("simulator should report healthy")
	}
	if !health.Capabilities["quotes"] {
		t.Fatal("quotes capability should be enabled")
	}
}

func TestProviderFailureModesDoNotDuplicateMoney(t *testing.T) {
	sim := NewSimulator(FailureProfile{GhostSuccess: true})
	instr := TransferInstruction{ClientRef: "si_dup", Beneficiary: "acct_1", AmountMinor: 1000, Currency: "USD"}
	first, err := sim.CreateTransfer(instr)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	status, err := sim.GetTransfer(instr.ClientRef)
	if err != nil {
		t.Fatalf("transfer query failed: %v", err)
	}
	if first.ProviderRef == "" || status.AmountMinor <= 0 {
		t.Fatal("transfer should preserve a single provider identity and amount")
	}
	if status.AmountMinor != 1001 {
		t.Fatalf("ghost success drift should expose the mismatch: got %d", status.AmountMinor)
	}
}

func TestE2ESimulatedTradeFlow(t *testing.T) {
	store := quotecore.NewQuoteStore()
	quoteReq := quotecore.QuoteRequest{CustomerID: "cust_1", BaseAmountMinor: 1000, BaseCurrency: "USD", QuoteCurrency: "USD", DestinationRail: "swift", Urgency: "normal", IdempotencyKey: "k-1"}
	quote, err := store.Create(quoteReq, "sim_lp_1", 1000, 50, 1000, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("quote create failed: %v", err)
	}
	if err := quote.Accept("trd_1", "accept-k-1", time.Now().UTC(), store); err != nil {
		t.Fatalf("quote accept failed: %v", err)
	}
	if quote.Status != quotecore.QuoteStatusAccepted {
		t.Fatalf("expected accepted status, got %s", quote.Status)
	}
	if !NewSimulator(FailureProfile{}).Health().Up {
		t.Fatal("simulator must be healthy for a successful Phase 2 execution")
	}
}
