package workflows

import (
	"testing"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type workflowRail struct{ creates, gets int }

func (r *workflowRail) CreateTransfer(provider.TransferInstruction) (provider.TransferAck, error) {
	r.creates++
	return provider.TransferAck{Status: provider.TransferProcessing}, nil
}
func (r *workflowRail) GetTransfer(string) (provider.TransferResult, error) {
	r.gets++
	result := provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}
	return result, nil
}
func (r *workflowRail) VerifyWebhook([]byte, map[string]string) (provider.WebhookEvent, error) {
	return provider.WebhookEvent{}, nil
}
func (r *workflowRail) ListTransactions(any) ([]provider.ExternalTransaction, error) { return nil, nil }
func (r *workflowRail) Health() provider.Health                                      { return provider.Health{Up: true} }

func TestWorkflowResumesFromPersistedInstruction(t *testing.T) {
	rail := &workflowRail{}
	snapshots := NewMemorySnapshots()
	workflow := &SettlementWorkflow{Store: settlementcore.NewStore(nil), Rail: rail, Snapshots: snapshots}
	si, err := workflow.Start("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = workflow.Submit(si.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := workflow.Resume(si.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != settlementcore.Confirmed || rail.creates != 1 || rail.gets != 1 {
		t.Fatalf("unexpected recovery state=%s creates=%d gets=%d", resolved.State, rail.creates, rail.gets)
	}
	snapshot, ok := snapshots.Load(si.ID)
	if !ok || snapshot.Stage != StageTerminal {
		t.Fatalf("expected terminal durable snapshot, got %+v", snapshot)
	}
}
