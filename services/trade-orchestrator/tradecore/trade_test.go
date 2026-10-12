package tradecore

import (
	"testing"
	"time"
)

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

func TestListTradesFiltersByInstitutionAndCustomerReference(t *testing.T) {
	store := NewStore()
	createdAt := time.Date(2026, time.October, 11, 12, 0, 0, 0, time.UTC)
	first, err := store.CreateTrade("inst_1", "quo_1", "key_1", "corr_1", 10000, "NGN", "USD", "", "", "partner-42")
	if err != nil {
		t.Fatal(err)
	}
	first.CreatedAt = createdAt
	first.UpdatedAt = createdAt
	second, err := store.CreateTrade("inst_1", "quo_2", "key_2", "corr_2", 20000, "USD", "NGN", "", "", "partner-99")
	if err != nil {
		t.Fatal(err)
	}
	second.CreatedAt = createdAt.Add(time.Minute)
	_, err = store.CreateTrade("inst_2", "quo_3", "key_3", "corr_3", 30000, "NGN", "USD", "", "", "partner-42")
	if err != nil {
		t.Fatal(err)
	}

	filter := TradeListFilter{
		Page: 1, PageSize: 10, From: createdAt, To: createdAt.Add(time.Hour),
		Currency: "nGn", Status: string(StateAccepted), CustomerReference: "partner-42",
	}
	trades, total, err := store.ListTrades("inst_1", filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(trades) != 1 || trades[0].ID != first.ID {
		t.Fatalf("expected only matching institution trade, total=%d trades=%+v", total, trades)
	}

	filter.CustomerReference = ""
	filter.Currency = ""
	filter.PageSize = 1
	pageOne, total, err := store.ListTrades("inst_1", filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(pageOne) != 1 || pageOne[0].ID != second.ID {
		t.Fatalf("expected newest trade on first page, total=%d trades=%+v", total, pageOne)
	}
}
