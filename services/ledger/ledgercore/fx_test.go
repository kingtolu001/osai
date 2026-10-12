package ledgercore

import (
	"strings"
	"testing"
	"time"
)

func validFXEvidence() FXExecutionEvidence {
	return FXExecutionEvidence{
		TradeID: "trd_1", InstitutionID: "inst_1", QuoteID: "quo_1", Executor: "provider",
		SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 150000, DestinationCurrency: "USD", DestinationExponent: 2,
		DestinationProceedsMinor: 100, AppliedRate: "1500.00", RateDirection: SourcePerDestination,
		ExecutionReference: "exec_1", SourceEvidenceReference: "src_evidence_1",
		DestinationEvidenceRef: "dst_evidence_1", OccurredAt: time.Now().UTC(),
		Fees: []FXFeeEvidence{{Kind: "provider", AmountMinor: 12, Currency: "NGN", EvidenceReference: "fee_1"}},
	}
}

func validFXMapping() FXJournalMapping {
	return FXJournalMapping{
		SourceDebit:       FXAccount{Code: "finance-source-debit", Name: "Finance source debit", Owner: "inst_1", Currency: "NGN"},
		SourceCredit:      FXAccount{Code: "finance-source-credit", Name: "Finance source credit", Owner: "provider", Currency: "NGN"},
		DestinationDebit:  FXAccount{Code: "finance-destination-debit", Name: "Finance destination debit", Owner: "custodian", Currency: "USD"},
		DestinationCredit: FXAccount{Code: "finance-destination-credit", Name: "Finance destination credit", Owner: "provider", Currency: "USD"},
	}
}

func TestBuildFXExecutionJournalsBalancesEachCurrencyIndependently(t *testing.T) {
	journals, err := BuildFXExecutionJournals(validFXEvidence(), validFXMapping())
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 2 || journals[0].Currency != "NGN" || journals[1].Currency != "USD" {
		t.Fatalf("expected independent source and destination journals, got %+v", journals)
	}
	if err := journals[0].Validate(); err != nil {
		t.Fatalf("source journal is not balanced: %v", err)
	}
	if err := journals[1].Validate(); err != nil {
		t.Fatalf("destination journal is not balanced: %v", err)
	}
}

func TestBuildFXExecutionJournalsFailsClosedWithoutFinanceMapping(t *testing.T) {
	_, err := BuildFXExecutionJournals(validFXEvidence(), FXJournalMapping{})
	if err == nil || !strings.Contains(err.Error(), "explicit FX account mapping") {
		t.Fatalf("expected missing account mapping to block posting, got %v", err)
	}
}

func TestBuildFXExecutionJournalsRejectsAccountCurrencyMismatch(t *testing.T) {
	mapping := validFXMapping()
	mapping.DestinationDebit.Currency = "NGN"
	if _, err := BuildFXExecutionJournals(validFXEvidence(), mapping); err == nil ||
		!strings.Contains(err.Error(), `currency "NGN" does not match journal currency "USD"`) {
		t.Fatalf("expected destination currency mismatch to fail closed, got %v", err)
	}
}

func TestValidateFXExecutionEvidenceRequiresIndependentEvidenceAndExactRate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*FXExecutionEvidence)
	}{
		{"missing source evidence", func(e *FXExecutionEvidence) { e.SourceEvidenceReference = "" }},
		{"missing destination evidence", func(e *FXExecutionEvidence) { e.DestinationEvidenceRef = "" }},
		{"floating point notation", func(e *FXExecutionEvidence) { e.AppliedRate = "1.5e3" }},
		{"unknown rate direction", func(e *FXExecutionEvidence) { e.RateDirection = "" }},
		{"missing fee evidence", func(e *FXExecutionEvidence) { e.Fees[0].EvidenceReference = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := validFXEvidence()
			test.change(&evidence)
			if err := ValidateFXExecutionEvidence(evidence); err == nil {
				t.Fatal("expected invalid execution evidence to be rejected")
			}
		})
	}
}

func TestValidateFXExecutionEvidenceChecksAppliedRateAndRounding(t *testing.T) {
	tests := []struct {
		name     string
		evidence FXExecutionEvidence
		wantErr  bool
	}{
		{name: "source per destination", evidence: validFXEvidence()},
		{
			name: "destination per source",
			evidence: func() FXExecutionEvidence {
				evidence := validFXEvidence()
				evidence.AppliedRate = "0.0006666667"
				evidence.RateDirection = DestinationPerSource
				return evidence
			}(),
		},
		{
			name: "floor conversion",
			evidence: func() FXExecutionEvidence {
				evidence := validFXEvidence()
				evidence.SourcePrincipalMinor = 149999
				evidence.DestinationProceedsMinor = 99
				return evidence
			}(),
		},
		{
			name: "different currency exponents",
			evidence: func() FXExecutionEvidence {
				evidence := validFXEvidence()
				evidence.SourceCurrency = "JPY"
				evidence.SourceExponent = 0
				evidence.SourcePrincipalMinor = 15000
				evidence.AppliedRate = "150"
				evidence.DestinationProceedsMinor = 10000
				evidence.Fees[0].Currency = "JPY"
				return evidence
			}(),
		},
		{
			name: "provider proceeds disagree with applied rate",
			evidence: func() FXExecutionEvidence {
				evidence := validFXEvidence()
				evidence.DestinationProceedsMinor = 101
				return evidence
			}(),
			wantErr: true,
		},
		{
			name: "unsupported currency exponent",
			evidence: func() FXExecutionEvidence {
				evidence := validFXEvidence()
				evidence.SourceExponent = 10
				return evidence
			}(),
			wantErr: true,
		},
		{
			name: "fee in unsupported third currency",
			evidence: func() FXExecutionEvidence {
				evidence := validFXEvidence()
				evidence.Fees[0].Currency = "EUR"
				return evidence
			}(),
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateFXExecutionEvidence(test.evidence)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateFXExecutionEvidence() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestJournalRejectsNegativeValuesAndOverflowingUnbalancedTotals(t *testing.T) {
	negative := NewJournal("negative", "trd_1", "NGN")
	negative.AddEntry("acct-1", "Account 1", "inst_1", "NGN", -1, 0, "negative")
	negative.AddEntry("acct-2", "Account 2", "inst_1", "NGN", 0, -1, "negative-credit")
	if err := negative.Validate(); err == nil {
		t.Fatal("negative journal amounts must be rejected")
	}

	overflow := NewJournal("overflow", "trd_1", "NGN")
	overflow.AddEntry("acct-1", "Account 1", "inst_1", "NGN", int64(^uint64(0)>>1), 0, "debit-1")
	overflow.AddEntry("acct-2", "Account 2", "inst_1", "NGN", int64(^uint64(0)>>1), 0, "debit-2")
	overflow.AddEntry("acct-3", "Account 3", "inst_1", "NGN", 0, int64(^uint64(0)>>1), "credit")
	if err := overflow.Validate(); err == nil {
		t.Fatal("overflowing unbalanced journal totals must be rejected")
	}
}
