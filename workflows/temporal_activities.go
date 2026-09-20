package workflows

import (
	"context"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type LedgerExecutor interface {
	ConfirmSettlement(settlementcore.Instruction) error
	FailSettlement(settlementcore.Instruction) error
}

type ProviderSettlementActivities struct {
	Rail   provider.SettlementRail
	Ledger LedgerExecutor
}

func (a *ProviderSettlementActivities) CreateTransfer(_ context.Context, input TemporalSettlementInput) (provider.TransferAck, error) {
	return a.Rail.CreateTransfer(provider.TransferInstruction{ClientRef: input.ClientRef, Beneficiary: input.Beneficiary, AmountMinor: input.AmountMinor, Currency: input.Currency, Purpose: input.Purpose})
}

func (a *ProviderSettlementActivities) GetTransfer(_ context.Context, input TemporalSettlementInput) (provider.TransferResult, error) {
	return a.Rail.GetTransfer(input.ClientRef)
}

func (a *ProviderSettlementActivities) ConfirmLedger(_ context.Context, input TemporalSettlementInput) error {
	return a.Ledger.ConfirmSettlement(toInstruction(input))
}

func (a *ProviderSettlementActivities) FailLedger(_ context.Context, input TemporalSettlementInput) error {
	return a.Ledger.FailSettlement(toInstruction(input))
}

func toInstruction(input TemporalSettlementInput) settlementcore.Instruction {
	return settlementcore.Instruction{ID: input.InstructionID, ClientRef: input.ClientRef, Beneficiary: input.Beneficiary, AmountMinor: input.AmountMinor, Currency: input.Currency, Purpose: input.Purpose}
}
