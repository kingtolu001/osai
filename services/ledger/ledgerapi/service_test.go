package ledgerapi

import (
	"testing"
)

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
