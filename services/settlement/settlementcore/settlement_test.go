package settlementcore

import (
	"errors"
	"sync"
	"testing"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/ledger/ledgerapi"
)

type fakeRail struct {
	creates, gets int
	createErr     error
	result        provider.TransferResult
}

func (f *fakeRail) CreateTransfer(provider.TransferInstruction) (provider.TransferAck, error) {
	f.creates++
	if f.createErr != nil {
		return provider.TransferAck{}, f.createErr
	}
	return provider.TransferAck{ProviderRef: "ptx_1", Status: provider.TransferAccepted}, nil
}
func (f *fakeRail) GetTransfer(string) (provider.TransferResult, error) {
	f.gets++
	return f.result, nil
}
func (f *fakeRail) VerifyWebhook([]byte, map[string]string) (provider.WebhookEvent, error) {
	return provider.WebhookEvent{}, nil
}
func (f *fakeRail) ListTransactions(any) ([]provider.ExternalTransaction, error) { return nil, nil }
func (f *fakeRail) Health() provider.Health                                      { return provider.Health{Up: true} }

func TestSubmitQueriesAfterAmbiguousCreateWithoutSecondCreate(t *testing.T) {
	rail := &fakeRail{createErr: &provider.Error{Class: provider.ErrorProviderTimeout, Message: "timeout"}, result: provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}}
	store := NewStore(nil)
	si, err := store.Create("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Submit(si.ID, rail)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != Confirmed || rail.creates != 1 || rail.gets != 1 {
		t.Fatalf("expected query-first confirmation, state=%s creates=%d gets=%d", resolved.State, rail.creates, rail.gets)
	}
}

func TestTerminalTransitionIsAbsorbingAndLedgerEffectRunsOnce(t *testing.T) {
	rail := &fakeRail{result: provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}}
	ledger := &fakeLedger{}
	store := NewStore(ledger)
	si, err := store.Create("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Submit(si.ID, rail); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ResolveWebhook(si.ID, provider.WebhookEvent{ClientRef: si.ClientRef, Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	if ledger.confirmed != 1 {
		t.Fatalf("expected one ledger confirmation, got %d", ledger.confirmed)
	}
}

func TestMismatchedConfirmationRequiresManualReview(t *testing.T) {
	rail := &fakeRail{result: provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 101, Currency: "USD", Beneficiary: "acct_1"}}
	store := NewStore(nil)
	si, _ := store.Create("acct_1", "USD", 100, "payout")
	if _, err := store.Submit(si.ID, rail); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Poll(si.ID, rail)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != ManualReview {
		t.Fatalf("expected manual review, got %s", resolved.State)
	}
}

func TestCallbackPollRaceProducesOneAuthoritativeLedgerEffect(t *testing.T) {
	rail := &fakeRail{result: provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"}}
	ledger := ledgerapi.NewService()
	store := NewStore(ledgerAdapter{service: ledger})
	si, err := store.Create("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Submit(si.ID, rail); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); _, _ = store.Poll(si.ID, rail) }()
	go func() {
		defer wait.Done()
		_, _ = store.ResolveWebhook(si.ID, provider.WebhookEvent{ProviderEventID: "evt_race", ClientRef: si.ClientRef, Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"})
	}()
	wait.Wait()
	resolved, ok := store.Get(si.ID)
	if !ok || resolved.State != Confirmed {
		t.Fatalf("expected confirmed race result: %+v", resolved)
	}
	if got := len(ledger.Journals()); got != 1 {
		t.Fatalf("expected one economic effect, got %d journals", got)
	}
}

type ledgerAdapter struct{ service *ledgerapi.Service }

func (a ledgerAdapter) ConfirmSettlement(instruction Instruction) error {
	return a.service.ConfirmSettlement(ledgerapi.SettlementInstruction{ID: instruction.ID, ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency, Purpose: instruction.Purpose})
}

func (a ledgerAdapter) FailSettlement(instruction Instruction) error {
	return a.service.FailSettlement(ledgerapi.SettlementInstruction{ID: instruction.ID, ClientRef: instruction.ClientRef, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency, Purpose: instruction.Purpose})
}

func TestFailedInstructionCanBeSupersededWithNewClientRef(t *testing.T) {
	rail := &fakeRail{result: provider.TransferResult{Status: provider.TransferFailed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1", FailureReason: "rejected"}}
	store := NewStore(nil)
	si, _ := store.Create("acct_1", "USD", 100, "payout")
	if _, err := store.Submit(si.ID, rail); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Poll(si.ID, rail)
	if err != nil || failed.State != Failed {
		t.Fatalf("expected authoritative failure: %+v %v", failed, err)
	}
	next, err := store.CreateSuperseding(si.ID, "acct_1", "USD", 100, "payout-retry")
	if err != nil {
		t.Fatal(err)
	}
	if next.Supersedes != si.ID || next.ClientRef == si.ClientRef {
		t.Fatalf("invalid superseding instruction: %+v", next)
	}
}

type fakeLedger struct{ confirmed, failed int }

func (f *fakeLedger) ConfirmSettlement(Instruction) error { f.confirmed++; return nil }
func (f *fakeLedger) FailSettlement(Instruction) error    { f.failed++; return errors.New("unused") }
