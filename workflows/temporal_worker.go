package workflows

import (
	"context"
	"errors"
	"os"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const TaskQueue = "osai-settlement-v1"

// Proof queues isolate independent sandbox runs from earlier unresolved
// workflows. Normal runtime always retains the established production queue.
func ConfiguredTaskQueue() (string, error) {
	selected := os.Getenv("OSAI_TEMPORAL_TASK_QUEUE")
	if selected == "" {
		return TaskQueue, nil
	}
	if os.Getenv("OSAI_LOCAL_PROOF_MODE") != "true" || !strings.HasPrefix(selected, "osai-proof-") || len(selected) <= len("osai-proof-") || len(selected) > 128 || strings.Trim(selected, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return "", errors.New("Temporal queue override requires an explicit local proof queue")
	}
	return selected, nil
}

func StartSettlementWorker(ctx context.Context, temporalClient client.Client, activities SettlementActivities) (worker.Worker, error) {
	if temporalClient == nil || activities == nil {
		return nil, context.Canceled
	}
	queue, err := ConfiguredTaskQueue()
	if err != nil {
		return nil, err
	}
	w := worker.New(temporalClient, queue, worker.Options{})
	w.RegisterWorkflow(TemporalSettlementWorkflow)
	w.RegisterActivityWithOptions(activities.CreateTransfer, activity.RegisterOptions{Name: "SettlementActivities.CreateTransfer"})
	w.RegisterActivityWithOptions(activities.GetTransfer, activity.RegisterOptions{Name: "SettlementActivities.GetTransfer"})
	w.RegisterActivityWithOptions(activities.ConfirmLedger, activity.RegisterOptions{Name: "SettlementActivities.ConfirmLedger"})
	w.RegisterActivityWithOptions(activities.FailLedger, activity.RegisterOptions{Name: "SettlementActivities.FailLedger"})
	w.RegisterActivityWithOptions(activities.ManualReview, activity.RegisterOptions{Name: "SettlementActivities.ManualReview"})
	if err := w.Start(); err != nil {
		return nil, err
	}
	return w, nil
}
