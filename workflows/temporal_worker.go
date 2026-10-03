package workflows

import (
	"context"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const TaskQueue = "osai-settlement-v1"

func StartSettlementWorker(ctx context.Context, temporalClient client.Client, activities SettlementActivities) (worker.Worker, error) {
	if temporalClient == nil || activities == nil {
		return nil, context.Canceled
	}
	w := worker.New(temporalClient, TaskQueue, worker.Options{})
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
