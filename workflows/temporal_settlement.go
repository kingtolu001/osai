package workflows

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

const CallbackSignal = "settlement.callback"

type TemporalSettlementInput struct {
	InstructionID string
	ClientRef     string
	Beneficiary   string
	AmountMinor   int64
	Currency      string
	Purpose       string
	MaxPolls      int
}

type TemporalSettlementResult struct {
	State      settlementcore.State
	Polls      int
	LedgerPost string
}

type SettlementActivities interface {
	CreateTransfer(ctx context.Context, input TemporalSettlementInput) (provider.TransferAck, error)
	GetTransfer(ctx context.Context, input TemporalSettlementInput) (provider.TransferResult, error)
	ConfirmLedger(ctx context.Context, input TemporalSettlementInput) error
	FailLedger(ctx context.Context, input TemporalSettlementInput) error
}

func TemporalSettlementWorkflow(ctx workflow.Context, input TemporalSettlementInput) (TemporalSettlementResult, error) {
	pollLimit := input.MaxPolls
	options := workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Second, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}
	ctx = workflow.WithActivityOptions(ctx, options)
	var ack provider.TransferAck
	createErr := workflow.ExecuteActivity(ctx, "SettlementActivities.CreateTransfer", input).Get(ctx, &ack)
	state := settlementcore.Submitted
	if createErr == nil {
		if ack.Status == provider.TransferProcessing {
			state = settlementcore.Processing
		} else {
			state = settlementcore.ProviderAccepted
		}
	}

	callbackChannel := workflow.GetSignalChannel(ctx, CallbackSignal)
	for poll := 0; pollLimit == 0 || poll < pollLimit; poll++ {
		var callback provider.WebhookEvent
		selector := workflow.NewSelector(ctx)
		timer := workflow.NewTimer(ctx, time.Second)
		receivedCallback := false
		selector.AddReceive(callbackChannel, func(channel workflow.ReceiveChannel, more bool) {
			channel.Receive(ctx, &callback)
			receivedCallback = true
		})
		selector.AddFuture(timer, func(workflow.Future) {})
		selector.Select(ctx)

		var result provider.TransferResult
		if receivedCallback {
			result = provider.TransferResult{Status: callback.Status, AmountMinor: callback.AmountMinor, Currency: callback.Currency, Beneficiary: callback.Beneficiary}
		} else if err := workflow.ExecuteActivity(ctx, "SettlementActivities.GetTransfer", input).Get(ctx, &result); err != nil {
			state = settlementcore.Unknown
			continue
		}
		if result.Status == provider.TransferConfirmed {
			if result.AmountMinor != input.AmountMinor || result.Currency != input.Currency || (result.Beneficiary != "" && result.Beneficiary != input.Beneficiary) {
				return TemporalSettlementResult{State: settlementcore.ManualReview, Polls: poll + 1}, nil
			}
			if err := workflow.ExecuteActivity(ctx, "SettlementActivities.ConfirmLedger", input).Get(ctx, nil); err != nil {
				return TemporalSettlementResult{}, err
			}
			return TemporalSettlementResult{State: settlementcore.Confirmed, Polls: poll + 1, LedgerPost: "confirmed"}, nil
		}
		if result.Status == provider.TransferFailed {
			if err := workflow.ExecuteActivity(ctx, "SettlementActivities.FailLedger", input).Get(ctx, nil); err != nil {
				return TemporalSettlementResult{}, err
			}
			return TemporalSettlementResult{State: settlementcore.Failed, Polls: poll + 1, LedgerPost: "failed"}, nil
		}
		state = settlementcore.Processing
	}
	return TemporalSettlementResult{State: state, Polls: pollLimit}, fmt.Errorf("settlement remained unresolved after %d polls", pollLimit)
}
