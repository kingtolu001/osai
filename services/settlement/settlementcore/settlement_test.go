package settlementcore

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/services/ledger/ledgerapi"
)

type fakeRail struct {
	creates, gets int
	createErr     error
	result        provider.TransferResult
	noEvidence    bool
}

func (f *fakeRail) CreateTransfer(provider.TransferInstruction) (provider.TransferAck, error) {
	f.creates++
	if f.createErr != nil {
		return provider.TransferAck{}, f.createErr
	}
	return provider.TransferAck{ProviderRef: "ptx_1", Status: provider.TransferAccepted}, nil
}
func (f *fakeRail) GetTransfer(clientRef string) (provider.TransferResult, error) {
	f.gets++
	result := f.result
	if !f.noEvidence && !result.Evidence.IsCompleteFor(clientRef) {
		result.Evidence = provider.Evidence{ID: "evidence_" + clientRef, Provider: "sim_lp_1", Method: "get_transfer", RequestRef: clientRef, RawPayloadHash: "sha256:test", ParsedAt: time.Now().UTC()}
	}
	return result, nil
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
	if resolved, _ := store.Get(si.ID); resolved.State == Confirmed || ledger.confirmed != 0 {
		t.Fatalf("callback finalized settlement without provider query: state=%s confirmations=%d", resolved.State, ledger.confirmed)
	}
	if _, err = store.Poll(si.ID, rail); err != nil {
		t.Fatal(err)
	}
	if ledger.confirmed != 1 {
		t.Fatalf("expected one ledger confirmation, got %d", ledger.confirmed)
	}
}

func TestDuplicateWebhookIsEvidenceOnly(t *testing.T) {
	store := NewStore(&fakeLedger{})
	si, err := store.Create("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	event := provider.WebhookEvent{
		ProviderEventID: "evt_duplicate",
		ClientRef:       si.ClientRef,
		Status:          provider.TransferConfirmed,
		AmountMinor:     100,
		Currency:        "USD",
		Beneficiary:     "acct_1",
		Evidence:        provider.Evidence{ID: "evidence_duplicate"},
	}
	for range 2 {
		if _, err := store.ResolveWebhook(si.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	resolved, _ := store.Get(si.ID)
	if resolved.State != Created || len(resolved.Evidence) != 1 {
		t.Fatalf("duplicate callback changed finality/evidence: state=%s evidence=%d", resolved.State, len(resolved.Evidence))
	}
}

func TestEvidencePersistenceFailureBlocksFinality(t *testing.T) {
	rail := &fakeRail{result: provider.TransferResult{
		Status:      provider.TransferConfirmed,
		AmountMinor: 100,
		Currency:    "USD",
		Beneficiary: "acct_1",
	}}
	ledger := &fakeLedger{}
	store := NewStore(ledger)
	si, err := store.Create("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	store.persistence = evidenceFailurePersistence{}
	resolved, err := store.Poll(si.ID, rail)
	if err == nil {
		t.Fatal("expected provider evidence persistence error")
	}
	if resolved.State != Created || ledger.confirmed != 0 || ledger.failed != 0 {
		t.Fatalf("failed evidence persistence authorized finality: state=%s confirmed=%d failed=%d", resolved.State, ledger.confirmed, ledger.failed)
	}
}

func TestMissingFinalityEvidenceRequiresManualReview(t *testing.T) {
	rail := &fakeRail{
		result:     provider.TransferResult{Status: provider.TransferConfirmed, AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1"},
		noEvidence: true,
	}
	ledger := &fakeLedger{}
	store := NewStore(ledger)
	si, err := store.Create("acct_1", "USD", 100, "payout")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Submit(si.ID, rail); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Poll(si.ID, rail)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != ManualReview || ledger.confirmed != 0 || ledger.failed != 0 {
		t.Fatalf("incomplete provider evidence finalized: state=%s confirmed=%d failed=%d", resolved.State, ledger.confirmed, ledger.failed)
	}
}

type evidenceFailurePersistence struct{ Persistence }

func (evidenceFailurePersistence) SaveEvidence(string, provider.Evidence) error {
	return errors.New("database unavailable")
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
