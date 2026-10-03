package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type temporalTestActivities struct {
	ledger      *ledgerapi.Service
	polled      provider.TransferResult
	createErr   error
	createCalls int
	getCalls    int
}

type temporalLedgerAdapter struct{ service *ledgerapi.Service }

func (a temporalLedgerAdapter) ConfirmSettlement(instruction settlementcore.Instruction) error {
	return a.service.ConfirmSettlement(ledgerapi.SettlementInstruction{ID: instruction.ID, ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency})
}

func (a temporalLedgerAdapter) FailSettlement(instruction settlementcore.Instruction) error {
	return a.service.FailSettlement(ledgerapi.SettlementInstruction{ID: instruction.ID, ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency})
}

func (a *temporalTestActivities) CreateTransfer(context.Context, TemporalSettlementInput) (provider.TransferAck, error) {
	a.createCalls++
	if a.createErr != nil {
		return provider.TransferAck{}, a.createErr
	}
	return provider.TransferAck{Status: provider.TransferProcessing}, nil
}
func (a *temporalTestActivities) GetTransfer(context.Context, TemporalSettlementInput) (provider.TransferResult, error) {
	a.getCalls++
	if a.polled.Status != "" {
		return a.polled, nil
	}
	return provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}, nil
}
func (a *temporalTestActivities) ConfirmLedger(_ context.Context, input TemporalSettlementInput) error {
	return temporalLedgerAdapter{service: a.ledger}.ConfirmSettlement(settlementcore.Instruction{ID: input.InstructionID, ClientRef: input.ClientRef, Beneficiary: input.Beneficiary, AmountMinor: input.AmountMinor, Currency: input.Currency})
}

func TestCallbackCannotConfirmWithoutRailPoll(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	environment := suite.NewTestWorkflowEnvironment()
	activities := &temporalTestActivities{ledger: ledgerapi.NewService(), polled: provider.TransferResult{Status: provider.TransferProcessing}}
	environment.RegisterWorkflow(TemporalSettlementWorkflow)
	environment.RegisterActivityWithOptions(activities.CreateTransfer, activity.RegisterOptions{Name: "SettlementActivities.CreateTransfer"})
	environment.RegisterActivityWithOptions(activities.GetTransfer, activity.RegisterOptions{Name: "SettlementActivities.GetTransfer"})
	environment.RegisterActivityWithOptions(activities.ConfirmLedger, activity.RegisterOptions{Name: "SettlementActivities.ConfirmLedger"})
	environment.RegisterActivityWithOptions(activities.FailLedger, activity.RegisterOptions{Name: "SettlementActivities.FailLedger"})
	environment.RegisterDelayedCallback(func() {
		environment.SignalWorkflow(CallbackSignal, provider.WebhookEvent{ClientRef: "si_temporal", Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD"})
	}, time.Millisecond)
	environment.ExecuteWorkflow(TemporalSettlementWorkflow, TemporalSettlementInput{InstructionID: "si_temporal", ClientRef: "si_temporal", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD", MaxPolls: 1})
	if !environment.IsWorkflowCompleted() || environment.GetWorkflowError() == nil {
		t.Fatal("workflow should remain unresolved after nonfinal rail poll")
	}
	if got := len(activities.ledger.Journals()); got != 0 {
		t.Fatalf("callback caused %d ledger effects", got)
	}
}
func (a *temporalTestActivities) FailLedger(context.Context, TemporalSettlementInput) error {
	return nil
}
func (a *temporalTestActivities) ManualReview(context.Context, TemporalSettlementInput) error {
	return nil
}

func registerTemporalActivities(environment *testsuite.TestWorkflowEnvironment, activities *temporalTestActivities) {
	environment.RegisterWorkflow(TemporalSettlementWorkflow)
	environment.RegisterActivityWithOptions(activities.CreateTransfer, activity.RegisterOptions{Name: "SettlementActivities.CreateTransfer"})
	environment.RegisterActivityWithOptions(activities.GetTransfer, activity.RegisterOptions{Name: "SettlementActivities.GetTransfer"})
	environment.RegisterActivityWithOptions(activities.ConfirmLedger, activity.RegisterOptions{Name: "SettlementActivities.ConfirmLedger"})
	environment.RegisterActivityWithOptions(activities.FailLedger, activity.RegisterOptions{Name: "SettlementActivities.FailLedger"})
	environment.RegisterActivityWithOptions(activities.ManualReview, activity.RegisterOptions{Name: "SettlementActivities.ManualReview"})
}

func TestTemporalAmbiguousCreateQueriesBeforeAnyNewCreate(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	environment := suite.NewTestWorkflowEnvironment()
	activities := &temporalTestActivities{ledger: ledgerapi.NewService(), createErr: errors.New("timeout after provider accepted")}
	registerTemporalActivities(environment, activities)
	environment.ExecuteWorkflow(TemporalSettlementWorkflow, TemporalSettlementInput{InstructionID: "si_temporal", ClientRef: "si_temporal", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD", MaxPolls: 1})
	if err := environment.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if activities.createCalls != 1 || activities.getCalls != 1 || len(activities.ledger.Journals()) != 1 {
		t.Fatalf("ambiguous create path: creates=%d queries=%d journals=%d", activities.createCalls, activities.getCalls, len(activities.ledger.Journals()))
	}
}

func TestTemporalProviderMismatchNeverPostsLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result provider.TransferResult
	}{
		{"amount", provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 101, Currency: "USD", Beneficiary: "acct_1"}},
		{"currency", provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "NGN", Beneficiary: "acct_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			environment := suite.NewTestWorkflowEnvironment()
			activities := &temporalTestActivities{ledger: ledgerapi.NewService(), polled: tc.result}
			registerTemporalActivities(environment, activities)
			environment.ExecuteWorkflow(TemporalSettlementWorkflow, TemporalSettlementInput{InstructionID: "si_temporal", ClientRef: "si_temporal", Beneficiary: "acct_1", AmountMinor: 100, Currency: "USD", MaxPolls: 1})
			if err := environment.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result TemporalSettlementResult
			if err := environment.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if result.State != settlementcore.ManualReview || len(activities.ledger.Journals()) != 0 {
				t.Fatalf("mismatch finalized: %+v", result)
			}
		})
	}
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
