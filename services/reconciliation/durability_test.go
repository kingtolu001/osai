package reconciliation

import (
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
)

var errTestStore = errors.New("injected persistence failure")

type failingStore struct {
	*MemoryStore
	approvalError, breakError, evidenceError, loadError bool
}

func (s *failingStore) SaveApproval(a *Approval) error {
	if s.approvalError {
		return errTestStore
	}
	return s.MemoryStore.SaveApproval(a)
}
func (s *failingStore) SaveBreak(b *ReconciliationBreak) error {
	if s.breakError {
		return errTestStore
	}
	return s.MemoryStore.SaveBreak(b)
}
func (s *failingStore) SaveEvidence(e Evidence) error {
	if s.evidenceError {
		return errTestStore
	}
	return s.MemoryStore.SaveEvidence(e)
}
func (s *failingStore) LoadApprovals() ([]*Approval, error) {
	if s.loadError {
		return nil, errTestStore
	}
	return s.MemoryStore.LoadApprovals()
}

func TestApprovalPersistenceFailureDoesNotPublishState(t *testing.T) {
	store := &failingStore{MemoryStore: NewMemoryStore(), approvalError: true}
	service := mustServiceWithStore(t, store)
	if proposal, err := service.ProposeAdjustment("break", "maker", "checker", 100, "NGN", "reason"); !errors.Is(err, errTestStore) || proposal != nil {
		t.Fatalf("failed proposal acknowledged: %v %v", proposal, err)
	}
	if err := service.ApproveAdjustment("break", "maker", "checker"); err == nil {
		t.Fatal("unpersisted proposal approved")
	}
	store.approvalError = false
	proposal, err := service.ProposeAdjustment("break", "maker", "checker", 100, "NGN", "reason")
	if err != nil {
		t.Fatal(err)
	}
	proposal.Approved = true // returned values cannot alter authoritative state
	if service.HasApprovedAdjustment("break") {
		t.Fatal("returned proposal aliases service state")
	}
	store.approvalError = true
	if err := service.ApproveAdjustment("break", "maker", "checker"); !errors.Is(err, errTestStore) {
		t.Fatal(err)
	}
	if service.HasApprovedAdjustment("break") {
		t.Fatal("failed approval published")
	}
	if err := service.RejectAdjustment("break", "checker", "rejected"); !errors.Is(err, errTestStore) {
		t.Fatal(err)
	}
	store.approvalError = false
	if err := service.ApproveAdjustment("break", "maker", "checker"); err != nil {
		t.Fatal(err)
	}
	if !mustServiceWithStore(t, store).HasApprovedAdjustment("break") {
		t.Fatal("successful retry not durable")
	}
}

func TestApprovalReloadRejectsCorruptEconomicsAndLoadFailure(t *testing.T) {
	if service, err := NewServiceWithStore(&PostgresStore{}); err == nil || service != nil {
		t.Fatal("unconfigured durable store accepted")
	}
	store := &failingStore{MemoryStore: NewMemoryStore(), loadError: true}
	if service, err := NewServiceWithStore(store); !errors.Is(err, errTestStore) || service != nil {
		t.Fatalf("load failure hidden: %v", err)
	}
	for _, amount := range []any{float64(100), float64(1.5), json.Number("1.5"), json.Number("9223372036854775808"), "NaN", int64(-1), nil} {
		if _, err := approvalAmountMinor(amount); err == nil {
			t.Fatalf("unsafe amount accepted: %v", amount)
		}
	}
	for _, amount := range []any{json.Number("9007199254740993"), "9007199254740993", int64(9007199254740993)} {
		value, err := approvalAmountMinor(amount)
		if err != nil || value != 9007199254740993 {
			t.Fatalf("exact amount lost: %d %v", value, err)
		}
	}
	if amount, err := approvalAmountMinor(json.Number("9223372036854775807")); err != nil || amount != math.MaxInt64 {
		t.Fatalf("max amount lost: %d %v", amount, err)
	}
	store = &failingStore{MemoryStore: NewMemoryStore()}
	store.approvals["bad"] = &Approval{BreakID: "break", Proposer: "maker", Approver: "checker", Status: "APPROVED", ProposedEffect: map[string]any{"amount_minor": float64(100), "currency": "NGN"}}
	if service, err := NewServiceWithStore(store); err == nil || service != nil {
		t.Fatal("corrupt persisted approval accepted")
	}
}

func TestBreakFailureAndResolutionAreRetryable(t *testing.T) {
	store := &failingStore{MemoryStore: NewMemoryStore(), breakError: true}
	service := mustServiceWithStore(t, store)
	expected := ExpectedTransaction{ID: "tx", Provider: "fixture", Currency: "NGN", AmountMinor: 100}
	observed := StatementRow{ID: "row", Provider: "fixture", Currency: "NGN", AmountMinor: 90}
	if brk, err := service.ReconcileTransaction(expected, observed); !errors.Is(err, errTestStore) || brk != nil {
		t.Fatalf("break failure hidden: %v", err)
	}
	store.breakError = false
	brk, err := service.ReconcileTransaction(expected, observed)
	if err != nil {
		t.Fatal(err)
	}
	store.breakError = true
	if err := service.ResolveBreak(brk.ID, "operator", "reason", "correlation"); !errors.Is(err, errTestStore) {
		t.Fatal(err)
	}
	if service.breaks[brk.ID].State != BreakStateOpen {
		t.Fatal("unpersisted resolution published")
	}
	observed.AmountMinor = 100
	if _, err := service.ReconcileTransaction(expected, observed); !errors.Is(err, errTestStore) {
		t.Fatal("automatic resolution hid failure")
	}
	store.breakError = false
	if _, err := service.ReconcileTransaction(expected, observed); err != nil {
		t.Fatal(err)
	}
	if mustServiceWithStore(t, store).breaks[brk.ID].State != BreakStateResolved {
		t.Fatal("retry did not persist resolution")
	}
}

func TestFailedEvidenceWriteDoesNotPoisonDeduplication(t *testing.T) {
	store := &failingStore{MemoryStore: NewMemoryStore(), evidenceError: true}
	service := mustServiceWithStore(t, store)
	row := StatementRow{ID: "row", Provider: "fixture", Currency: "NGN", AmountMinor: 100}
	if _, err := service.IngestStatement(row); !errors.Is(err, errTestStore) {
		t.Fatal(err)
	}
	store.evidenceError = false
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatal(err)
	}
	items, _ := store.LoadEvidence()
	if len(items) != 1 || service.DuplicateRows() != 0 {
		t.Fatal("failed write consumed dedup key")
	}
}

func TestBalanceBreakUsesExplicitCurrency(t *testing.T) {
	service := NewService()
	brk, err := service.ReconcileBalance("fixture-NGN-account", "NGN", 1000, 920, 40)
	if err != nil || brk.Currency != "NGN" || brk.Expected != 960 {
		t.Fatalf("incorrect balance break: %+v %v", brk, err)
	}
	if _, err := service.ReconcileBalance("fixture-account", "", 1000, 920, 40); err == nil {
		t.Fatal("currency inferred")
	}
}

func TestConcurrentMismatchReplayRetainsOneBreak(t *testing.T) {
	store := NewMemoryStore()
	service := mustServiceWithStore(t, store)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			_, err := service.ReconcileTransaction(ExpectedTransaction{ID: "tx", Provider: "fixture", Currency: "NGN", AmountMinor: 100}, StatementRow{ID: "row", Provider: "fixture", Currency: "NGN", AmountMinor: 90})
			if err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	breaks, err := store.LoadBreaks()
	if err != nil || len(breaks) != 1 {
		t.Fatalf("duplicate breaks: %d %v", len(breaks), err)
	}
}

func TestGeneratedEvidenceIDsDoNotCollideAcrossRestart(t *testing.T) {
	store := NewMemoryStore()
	first := mustServiceWithStore(t, store)
	row1, err := first.IngestStatement(StatementRow{Provider: "fixture", ProviderRef: "one", Currency: "NGN", AmountMinor: 100})
	if err != nil {
		t.Fatal(err)
	}
	row2, err := mustServiceWithStore(t, store).IngestStatement(StatementRow{Provider: "fixture", ProviderRef: "two", Currency: "NGN", AmountMinor: 200})
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.LoadEvidence()
	if err != nil || len(items) != 2 || row1.ID == row2.ID {
		t.Fatalf("restart overwrote evidence: rows=%d err=%v", len(items), err)
	}
}
