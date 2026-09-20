package tradecore

import "testing"

func TestTradeAcceptsQuoteWhenFunded(t *testing.T) {
	trade := &Trade{State: StateCreated, HoldAmount: 200, Available: 200}

	if err := trade.Accept(); err != nil {
		t.Fatalf("trade should accept when sufficient funds are available: %v", err)
	}

	if trade.State != StateAccepted {
		t.Fatalf("expected accepted state, got %s", trade.State)
	}
}

func TestTradeRejectsWhenHoldExceedsAvailable(t *testing.T) {
	trade := &Trade{State: StateCreated, HoldAmount: 250, Available: 200}

	if err := trade.Accept(); err == nil {
		t.Fatal("expected accept to fail when hold exceeds available funds")
	}
}
