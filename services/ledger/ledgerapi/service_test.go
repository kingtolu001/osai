package ledgerapi

import (
	"errors"
	"testing"
)

func TestAdjustmentWithoutApprovedTargetCannotPost(t *testing.T) {
	service := NewService()
	command := ReconciliationAdjustmentCommand{IdempotencyKey: "fixture-adjustment", CorrelationID: "fixture-correlation", BreakID: "fixture-break", Maker: "maker", Checker: "checker", Currency: "NGN", AmountMinor: 100}
	for range 2 {
		id, replayed, err := service.PostReconciliationAdjustment(command)
		if !errors.Is(err, ErrAdjustmentPolicyRequired) || id != "" || replayed {
			t.Fatalf("unsafe adjustment accepted: id=%q replay=%v err=%v", id, replayed, err)
		}
	}
	if len(service.Journals()) != 0 {
		t.Fatal("disabled adjustment mutated journals")
	}
}

func TestTerminalSettlementPostingIsIdempotent(t *testing.T) {
	service := NewService()
	instruction := SettlementInstruction{ID: "si_1", ClientRef: "si_1", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD"}
	if err := service.ConfirmSettlement(instruction); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfirmSettlement(instruction); err != nil {
		t.Fatal(err)
	}
	if got := len(service.Journals()); got != 1 {
		t.Fatalf("expected one journal, got %d", got)
	}
}
