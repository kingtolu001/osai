package workflows

import (
	"context"
	"errors"

	"github.com/osai/osai/pkg/correlation"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type LedgerExecutor interface {
	ConfirmSettlement(settlementcore.Instruction) error
	FailSettlement(settlementcore.Instruction) error
}

type FundingVerifier interface {
	VerifyFunding(context.Context, string, string, int64, string) error
}

type ProviderSettlementActivities struct {
	Rail     provider.SettlementRail
	Rails    map[string]provider.SettlementRail
	Ledger   LedgerExecutor
	Funding  FundingVerifier
	Recorder interface {
		RecordRuntimeState(context.Context, string, settlementcore.State, string, provider.Evidence) error
	}
}

func (a *ProviderSettlementActivities) CreateTransfer(ctx context.Context, input TemporalSettlementInput) (provider.TransferAck, error) {
	ctx, span := observability.Start(correlation.WithContext(ctx, input.CorrelationID), "temporal-worker", "provider.create_transfer")
	defer span.End()
	if input.ProviderID == "flutterwave" {
		if a.Funding == nil {
			return provider.TransferAck{}, errors.New("ledger funding verification unavailable")
		}
		if err := a.Funding.VerifyFunding(ctx, input.InstitutionID, input.TradeID, input.AmountMinor, input.Currency); err != nil {
			return provider.TransferAck{}, err
		}
	}
	rail, err := a.rail(input.ProviderID)
	if err != nil {
		return provider.TransferAck{}, err
	}
	if err := a.record(ctx, input, settlementcore.Submitted, "", provider.Evidence{}); err != nil {
		return provider.TransferAck{}, err
	}
	ack, err := rail.CreateTransfer(provider.TransferInstruction{ClientRef: input.ClientRef, Beneficiary: input.Beneficiary, AmountMinor: input.AmountMinor, Currency: input.Currency, Purpose: input.Purpose, Metadata: map[string]string{"account_bank": input.BankCode, "account_number": input.AccountNumber}})
	if err != nil {
		_ = a.record(ctx, input, settlementcore.Unknown, "", provider.Evidence{})
		return provider.TransferAck{}, err
	}
	state := settlementcore.ProviderAccepted
	if ack.Status == provider.TransferProcessing {
		state = settlementcore.Processing
	}
	if err := a.record(ctx, input, state, ack.ProviderRef, ack.Evidence); err != nil {
		return provider.TransferAck{}, err
	}
	return ack, nil
}

func (a *ProviderSettlementActivities) GetTransfer(ctx context.Context, input TemporalSettlementInput) (provider.TransferResult, error) {
	ctx, span := observability.Start(correlation.WithContext(ctx, input.CorrelationID), "temporal-worker", "provider.get_transfer")
	defer span.End()
	rail, err := a.rail(input.ProviderID)
	if err != nil {
		return provider.TransferResult{}, err
	}
	result, err := rail.GetTransfer(input.ClientRef)
	if err != nil {
		_ = a.record(ctx, input, settlementcore.Unknown, "", provider.Evidence{})
		return provider.TransferResult{}, err
	}
	state := settlementcore.Processing
	if result.Status == provider.TransferAccepted {
		state = settlementcore.ProviderAccepted
	}
	if err := a.record(ctx, input, state, result.ProviderRef, result.Evidence); err != nil {
		return provider.TransferResult{}, err
	}
	return result, nil
}

func (a *ProviderSettlementActivities) rail(id string) (provider.SettlementRail, error) {
	if id == "" || id == "sim_lp_1" {
		if a.Rail != nil {
			return a.Rail, nil
		}
	}
	if rail, ok := a.Rails[id]; ok && rail != nil {
		return rail, nil
	}
	return nil, errors.New("settlement provider unavailable")
}

func (a *ProviderSettlementActivities) ConfirmLedger(ctx context.Context, input TemporalSettlementInput) error {
	ctx, span := observability.Start(correlation.WithContext(ctx, input.CorrelationID), "temporal-worker", "ledger.confirm_activity")
	defer span.End()
	if err := a.Ledger.ConfirmSettlement(toInstruction(input)); err != nil {
		return err
	}
	return a.record(ctx, input, settlementcore.Confirmed, input.ProviderRef, provider.Evidence{})
}

func (a *ProviderSettlementActivities) FailLedger(ctx context.Context, input TemporalSettlementInput) error {
	ctx, span := observability.Start(correlation.WithContext(ctx, input.CorrelationID), "temporal-worker", "ledger.fail_activity")
	defer span.End()
	if err := a.Ledger.FailSettlement(toInstruction(input)); err != nil {
		return err
	}
	return a.record(ctx, input, settlementcore.Failed, input.ProviderRef, provider.Evidence{})
}

func (a *ProviderSettlementActivities) ManualReview(ctx context.Context, input TemporalSettlementInput) error {
	return a.record(ctx, input, settlementcore.ManualReview, input.ProviderRef, provider.Evidence{})
}

func (a *ProviderSettlementActivities) record(ctx context.Context, input TemporalSettlementInput, state settlementcore.State, ref string, evidence provider.Evidence) error {
	if a.Recorder == nil {
		return nil
	}
	return a.Recorder.RecordRuntimeState(ctx, input.InstructionID, state, ref, evidence)
}

func toInstruction(input TemporalSettlementInput) settlementcore.Instruction {
	return settlementcore.Instruction{ID: input.InstructionID, InstitutionID: input.InstitutionID, TradeID: input.TradeID, CorrelationID: input.CorrelationID, ClientRef: input.ClientRef, ProviderRef: input.ProviderRef, Beneficiary: input.Beneficiary, AmountMinor: input.AmountMinor, Currency: input.Currency, Purpose: input.Purpose}
}
