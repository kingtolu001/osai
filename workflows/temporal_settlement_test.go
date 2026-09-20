package workflows

import (
	"context"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type temporalTestActivities struct{ ledger *ledgerapi.Service }

type temporalLedgerAdapter struct{ service *ledgerapi.Service }

func (a temporalLedgerAdapter) ConfirmSettlement(instruction settlementcore.Instruction) error {
	return a.service.ConfirmSettlement(ledgerapi.SettlementInstruction{ID: instruction.ID, ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency})
}

func (a temporalLedgerAdapter) FailSettlement(instruction settlementcore.Instruction) error {
	return a.service.FailSettlement(ledgerapi.SettlementInstruction{ID: instruction.ID, ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency})
}

func (a *temporalTestActivities) CreateTransfer(context.Context, TemporalSettlementInput) (provider.TransferAck, error) {
	return provider.TransferAck{Status: provider.TransferProcessing}, nil
}
func (a *temporalTestActivities) GetTransfer(context.Context, TemporalSettlementInput) (provider.TransferResult, error) {
	return provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}, nil
}
func (a *temporalTestActivities) ConfirmLedger(_ context.Context, input TemporalSettlementInput) error {
	return temporalLedgerAdapter{service: a.ledger}.ConfirmSettlement(settlementcore.Instruction{ID: input.InstructionID, ClientRef: input.ClientRef, Beneficiary: input.Beneficiary, AmountMinor: input.AmountMinor, Currency: input.Currency})
}
func (a *temporalTestActivities) FailLedger(context.Context, TemporalSettlementInput) error {
	return nil
}

func TestTemporalSettlementWorkflowUsesDurableHistoryAndOneLedgerEffect(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	environment := suite.NewTestWorkflowEnvironment()
	activities := &temporalTestActivities{ledger: ledgerapi.NewService()}
	environment.RegisterWorkflow(TemporalSettlementWorkflow)
	environment.RegisterActivityWithOptions(activities.CreateTransfer, activity.RegisterOptions{Name: "SettlementActivities.CreateTransfer"})
	environment.RegisterActivityWithOptions(activities.GetTransfer, activity.RegisterOptions{Name: "SettlementActivities.GetTransfer"})
	environment.RegisterActivityWithOptions(activities.ConfirmLedger, activity.RegisterOptions{Name: "SettlementActivities.ConfirmLedger"})
	environment.RegisterActivityWithOptions(activities.FailLedger, activity.RegisterOptions{Name: "SettlementActivities.FailLedger"})
	environment.ExecuteWorkflow(TemporalSettlementWorkflow, TemporalSettlementInput{InstructionID: "si_temporal", ClientRef: "si_temporal", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD", MaxPolls: 2})
	if !environment.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := environment.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result TemporalSettlementResult
	if err := environment.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != settlementcore.Confirmed || result.LedgerPost != "confirmed" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if got := len(activities.ledger.Journals()); got != 1 {
		t.Fatalf("expected one authoritative ledger journal, got %d", got)
	}
}
