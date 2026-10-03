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
	ProviderRef   string
	ProviderID    string
	BankCode      string
	AccountNumber string
	Beneficiary   string
	AmountMinor   int64
	Currency      string
	Purpose       string
	MaxPolls      int
	PollInterval  time.Duration
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
	ManualReview(ctx context.Context, input TemporalSettlementInput) error
}

func TemporalSettlementWorkflow(ctx workflow.Context, input TemporalSettlementInput) (TemporalSettlementResult, error) {
	pollLimit := input.MaxPolls
	activityTimeout := 10 * time.Second
	if input.ProviderID == "flutterwave" {
		// A reference lookup may require both list and detail requests, each
		// with its own network timeout. Keep the activity alive for both.
		activityTimeout = 30 * time.Second
	}
	options := workflow.ActivityOptions{StartToCloseTimeout: activityTimeout, RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1}}
	ctx = workflow.WithActivityOptions(ctx, options)
	var ack provider.TransferAck
	createErr := workflow.ExecuteActivity(ctx, "SettlementActivities.CreateTransfer", input).Get(ctx, &ack)
	state := settlementcore.Submitted
	if createErr == nil {
		input.ProviderRef = ack.ProviderRef
		if ack.Status == provider.TransferProcessing {
			state = settlementcore.Processing
		} else {
			state = settlementcore.ProviderAccepted
		}
	}

	callbackChannel := workflow.GetSignalChannel(ctx, CallbackSignal)
	seenCallbacks := map[string]struct{}{}
	for poll := 0; pollLimit == 0 || poll < pollLimit; poll++ {
		var callback provider.WebhookEvent
		selector := workflow.NewSelector(ctx)
		interval := time.Second
		if input.ProviderID == "flutterwave" {
			interval = 30 * time.Second
		}
		receivedCallback := false
		if input.PollInterval > 0 {
			interval = input.PollInterval
		}
		if createErr == nil || poll > 0 {
			timer := workflow.NewTimer(ctx, interval)
			selector.AddReceive(callbackChannel, func(channel workflow.ReceiveChannel, more bool) {
				channel.Receive(ctx, &callback)
				receivedCallback = true
			})
			selector.AddFuture(timer, func(workflow.Future) {})
			selector.Select(ctx)
		}

		var result provider.TransferResult
		// A signed callback is a hint to poll the configured rail. It is not
		// authoritative evidence for ledger finality by itself.
		if receivedCallback && callback.ClientRef != input.ClientRef {
			if err := workflow.ExecuteActivity(ctx, "SettlementActivities.ManualReview", input).Get(ctx, nil); err != nil {
				return TemporalSettlementResult{}, err
			}
			return TemporalSettlementResult{State: settlementcore.ManualReview, Polls: poll + 1}, nil
		}
		if receivedCallback && callback.ProviderEventID != "" {
			if _, ok := seenCallbacks[callback.ProviderEventID]; ok {
				continue
			}
			seenCallbacks[callback.ProviderEventID] = struct{}{}
		}
		if err := workflow.ExecuteActivity(ctx, "SettlementActivities.GetTransfer", input).Get(ctx, &result); err != nil {
			state = settlementcore.Unknown
			continue
		}
		if result.ClientRef != "" && result.ClientRef != input.ClientRef || input.ProviderRef != "" && result.ProviderRef != "" && result.ProviderRef != input.ProviderRef || result.AmountMinor != input.AmountMinor || result.Currency != input.Currency || (result.Beneficiary != "" && result.Beneficiary != input.Beneficiary) {
			if err := workflow.ExecuteActivity(ctx, "SettlementActivities.ManualReview", input).Get(ctx, nil); err != nil {
				return TemporalSettlementResult{}, err
			}
			return TemporalSettlementResult{State: settlementcore.ManualReview, Polls: poll + 1}, nil
		}
		if result.ProviderRef != "" {
			input.ProviderRef = result.ProviderRef
		}
		if result.Status == provider.TransferConfirmed {
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
