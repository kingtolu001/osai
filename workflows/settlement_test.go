package workflows

import (
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/settlement/settlementcore"
)

type workflowRail struct {
	creates, gets int
	clientRef     string
	result        provider.TransferResult
}

func (r *workflowRail) CreateTransfer(in provider.TransferInstruction) (provider.TransferAck, error) {
	r.creates++
	r.clientRef = in.ClientRef
	return provider.TransferAck{Status: provider.TransferProcessing}, nil
}
func (r *workflowRail) GetTransfer(string) (provider.TransferResult, error) {
	r.gets++
	if r.result.Status != "" {
		result := r.result
		if !result.Evidence.IsCompleteFor(r.clientRef) {
			result.Evidence = provider.Evidence{ID: "workflow-evidence", Provider: "sim_lp_1", Method: "get_transfer", RequestRef: r.clientRef, RawPayloadHash: "sha256:test", ParsedAt: time.Now().UTC()}
		}
		return result, nil
	}
	return provider.TransferResult{
		Status:      provider.TransferConfirmed,
		AmountMinor: 100,
		Currency:    "USD",
		Beneficiary: "acct_1",
		Evidence:    provider.Evidence{ID: "workflow-evidence", Provider: "sim_lp_1", Method: "get_transfer", RequestRef: r.clientRef, RawPayloadHash: "sha256:test", ParsedAt: time.Now().UTC()},
	}, nil
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

func TestWorkflowPollsProviderRatherThanTrustingWebhookStatus(t *testing.T) {
	rail := &workflowRail{result: provider.TransferResult{
		Status:      provider.TransferConfirmed,
		AmountMinor: 100,
		Currency:    "USD",
		Beneficiary: "acct_1",
	}}
	workflow := &SettlementWorkflow{Store: settlementcore.NewStore(nil), Rail: rail, Snapshots: NewMemorySnapshots()}
	instruction, err := workflow.Start("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = workflow.Submit(instruction.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := workflow.HandleWebhook(instruction.ID, provider.WebhookEvent{
		ProviderEventID: "evt_failed_claim",
		ClientRef:       instruction.ClientRef,
		Status:          provider.TransferFailed,
		AmountMinor:     100,
		Currency:        "USD",
		Beneficiary:     "acct_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != settlementcore.Confirmed || rail.gets != 1 {
		t.Fatalf("workflow did not use authoritative provider query: state=%s queries=%d", resolved.State, rail.gets)
	}
}
