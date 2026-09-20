package ledgercore

import "testing"

func TestJournalBalancesPerCurrency(t *testing.T) {
	journal := NewJournal("JT-001", "trd_1", "NGN")
	journal.AddEntry("1300", "CLIENT_FUNDS_BANK", "BankA", "NGN", 1505000, 0, "bank:ngn:001")
	journal.AddEntry("2000", "CLIENT_PREFUND", "ClientX", "NGN", 0, 1505000, "bank:ngn:002")

	if err := journal.Validate(); err != nil {
		t.Fatalf("expected balanced journal: %v", err)
	}
}

func TestHoldCannotExceedAvailable(t *testing.T) {
	acct := &LedgerAccount{Code: "2000", Owner: "ClientX", Currency: "NGN", Available: 1000}

	if err := PlaceHold(acct, 1001); err == nil {
		t.Fatal("expected hold beyond available to fail")
	}
}

func TestReleaseHoldRestoresAvailable(t *testing.T) {
	acct := &LedgerAccount{Code: "2000", Owner: "ClientX", Currency: "NGN", Available: 1000, Held: 200}

	if err := ReleaseHold(acct, 200); err != nil {
		t.Fatalf("release should succeed: %v", err)
	}

	if acct.Held != 0 {
		t.Fatalf("held should be zero after release, got %d", acct.Held)
	}
	if acct.Available != 1200 {
		t.Fatalf("available should recover to 1200, got %d", acct.Available)
	}
}

func TestDuplicateExternalReferenceIsRejected(t *testing.T) {
	journal := NewJournal("JT-101", "trd_1", "NGN")
	journal.AddEntry("2000", "CLIENT_PREFUND", "ClientX", "NGN", 1000, 0, "ext:ref-1")
	journal.AddEntry("2100", "CLIENT_PREFUND_HELD", "ClientX", "NGN", 0, 1000, "ext:ref-1")

	if err := journal.Validate(); err == nil {
		t.Fatal("expected duplicate external reference to fail")
	}
}
