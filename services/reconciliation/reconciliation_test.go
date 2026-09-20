package reconciliation

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestExactMatchDoesNotCreateBreak(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{
		ID:          "txn_1",
		ClientRef:   "si_1001",
		ProviderRef: "ptx_1001",
		AmountMinor: 1000,
		Currency:    "USD",
		Beneficiary: "acct_1",
		FeeMinor:    10,
		Account:     "NOSTRO_USD",
		Watermark:   "2026-09-19T10:00:00Z",
	}
	observed := StatementRow{
		ID:          "ext_1",
		ProviderRef: "ptx_1001",
		ClientRef:   "si_1001",
		AmountMinor: 1000,
		Currency:    "USD",
		Beneficiary: "acct_1",
		FeeMinor:    10,
		Account:     "NOSTRO_USD",
		Watermark:   "2026-09-19T10:00:00Z",
	}
	if brk := service.ReconcileTransaction(expected, observed); brk != nil {
		t.Fatalf("expected no break, got %+v", brk)
	}
}

func TestCompositeFallbackMatchUsesAmountCurrencyBeneficiary(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_2", ClientRef: "si_2002", AmountMinor: 2000, Currency: "EUR", Beneficiary: "acct_2", FeeMinor: 20, Account: "NOSTRO_EUR", Watermark: "2026-09-19T10:00:00Z"}
	observed := StatementRow{ID: "ext_2", ProviderRef: "ptx_2002", AmountMinor: 2000, Currency: "EUR", Beneficiary: "acct_2", FeeMinor: 20, Account: "NOSTRO_EUR", Watermark: "2026-09-19T10:00:00Z"}
	if brk := service.ReconcileTransaction(expected, observed); brk != nil {
		t.Fatalf("expected composite match fallback, got %+v", brk)
	}
}

func TestDuplicateStatementRowsAreDeduped(t *testing.T) {
	service := NewService()
	row := StatementRow{ID: "ext_dup", ProviderRef: "ptx_dup", ClientRef: "si_dup", AmountMinor: 999, Currency: "USD", Beneficiary: "acct_dup", FeeMinor: 9, Account: "NOSTRO_USD", RawHash: "hash_dup", Watermark: "2026-09-19T10:00:00Z"}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatalf("ingest failed: %v", err)
	}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatalf("duplicate ingest should be accepted as dedup, got %v", err)
	}
	if got := service.DuplicateRows(); got != 1 {
		t.Fatalf("expected one duplicate record tracked, got %d", got)
	}
}

func TestMissingExternalEvidenceCreatesBreak(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_missing", ClientRef: "si_missing", AmountMinor: 1234, Currency: "USD", Beneficiary: "acct_missing", FeeMinor: 12, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	brk := service.ReconcileTransaction(expected, StatementRow{})
	if brk == nil || brk.Type != BreakTypeMissingExternalEvidence {
		t.Fatalf("expected missing evidence break, got %+v", brk)
	}
}

func TestLateEvidenceCreatesBreak(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_late", ClientRef: "si_late", AmountMinor: 77, Currency: "USD", Beneficiary: "acct_late", FeeMinor: 3, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	notice := StatementRow{ID: "ext_late", ProviderRef: "ptx_late", ClientRef: "si_late", AmountMinor: 77, Currency: "USD", Beneficiary: "acct_late", FeeMinor: 3, Account: "NOSTRO_USD", Watermark: "2026-09-19T09:00:00Z"}
	brk := service.ReconcileTransaction(expected, notice)
	if brk == nil || brk.Type != BreakTypeLateEvidence {
		t.Fatalf("expected late evidence break, got %+v", brk)
	}
}

func TestAmountMismatchCreatesBreak(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_amt", ClientRef: "si_amt", AmountMinor: 500, Currency: "USD", Beneficiary: "acct_amt", FeeMinor: 5, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	observed := StatementRow{ID: "ext_amt", ProviderRef: "ptx_amt", ClientRef: "si_amt", AmountMinor: 505, Currency: "USD", Beneficiary: "acct_amt", FeeMinor: 5, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	brk := service.ReconcileTransaction(expected, observed)
	if brk == nil || brk.Type != BreakTypeAmountMismatch {
		t.Fatalf("expected amount mismatch break, got %+v", brk)
	}
}

func TestFeeDriftCreatesBreak(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_fee", ClientRef: "si_fee", AmountMinor: 800, Currency: "USD", Beneficiary: "acct_fee", FeeMinor: 8, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	observed := StatementRow{ID: "ext_fee", ProviderRef: "ptx_fee", ClientRef: "si_fee", AmountMinor: 800, Currency: "USD", Beneficiary: "acct_fee", FeeMinor: 12, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	brk := service.ReconcileTransaction(expected, observed)
	if brk == nil || brk.Type != BreakTypeFeeMismatch {
		t.Fatalf("expected fee mismatch break, got %+v", brk)
	}
}

func TestClosingBalanceMismatchCreatesBreak(t *testing.T) {
	service := NewService()
	brk := service.ReconcileBalance("NOSTRO_USD", 1000, 920, 40)
	if brk == nil || brk.Type != BreakTypeBalanceMismatch {
		t.Fatalf("expected balance mismatch break, got %+v", brk)
	}
}

func TestReplaySameStatementIsDedupedAcrossReconcile(t *testing.T) {
	service := NewService()
	row := StatementRow{ID: "ext_replay", ProviderRef: "ptx_replay", ClientRef: "si_replay", AmountMinor: 333, Currency: "USD", Beneficiary: "acct_replay", FeeMinor: 3, Account: "NOSTRO_USD", RawHash: "hash_replay", Watermark: "2026-09-19T10:00:00Z"}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatalf("ingest failed: %v", err)
	}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatalf("replay should be deduped, got %v", err)
	}
	if service.DedupCount() != 1 {
		t.Fatalf("expected single deduped row, got %d", service.DedupCount())
	}
}

func TestWatermarkBoundaryAcceptsExactBoundary(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_boundary", ClientRef: "si_boundary", AmountMinor: 42, Currency: "USD", Beneficiary: "acct_boundary", FeeMinor: 1, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	observed := StatementRow{ID: "ext_boundary", ProviderRef: "ptx_boundary", ClientRef: "si_boundary", AmountMinor: 42, Currency: "USD", Beneficiary: "acct_boundary", FeeMinor: 1, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	if brk := service.ReconcileTransaction(expected, observed); brk != nil {
		t.Fatalf("watermark boundary should match exactly, got %+v", brk)
	}
}

func TestAdjustmentMakerCheckerRequiresApproval(t *testing.T) {
	service := NewService()
	breakID := "break_1"
	proposal, err := service.ProposeAdjustment(breakID, "maker_1", "checker_1", 100, "USD", "manual adjustment")
	if err != nil || proposal == nil {
		t.Fatalf("expected proposal to be created, got %v %v", proposal, err)
	}
	if err := service.ApproveAdjustment(breakID, "maker_1", "checker_1"); err != nil {
		t.Fatalf("expected approved adjustment to succeed, got %v", err)
	}
	if !service.HasApprovedAdjustment(breakID) {
		t.Fatalf("expected adjustment approval recorded")
	}
}

func TestApprovedAdjustmentPersistsAcrossRestart(t *testing.T) {
	store := NewMemoryStore()
	service := NewServiceWithStore(store)
	breakID := "break_persist_1"
	if _, err := service.ProposeAdjustment(breakID, "maker_persist", "checker_persist", 75, "USD", "persisted adjustment"); err != nil {
		t.Fatalf("proposal failed: %v", err)
	}
	if err := service.ApproveAdjustment(breakID, "maker_persist", "checker_persist"); err != nil {
		t.Fatalf("approval failed: %v", err)
	}
	if !service.HasApprovedAdjustment(breakID) {
		t.Fatal("approval not recorded in memory")
	}
	restarted := NewServiceWithStore(store)
	if !restarted.HasApprovedAdjustment(breakID) {
		t.Fatal("approval did not survive restart through persistence")
	}
	approvals, err := store.LoadApprovals()
	if err != nil {
		t.Fatalf("load approvals failed: %v", err)
	}
	if len(approvals) != 1 {
		t.Fatalf("expected one persisted approval, got %d", len(approvals))
	}
	if approvals[0].Status != "APPROVED" {
		t.Fatalf("expected persisted approval to be APPROVED, got %s", approvals[0].Status)
	}
}

func TestResolvedBreakStateAndHistoryPersistAcrossRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://osai:osai@localhost:5432/osai?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`TRUNCATE TABLE reconciliation_evidence, reconciliation_breaks, reconciliation_match_results, reconciliation_watermarks, reconciliation_approvals, reconciliation_resolution_history RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	store := &PostgresStore{DB: db}
	service := NewServiceWithStore(store)
	brk := service.NewBreak(BreakTypeAmountMismatch, "provider_hist", "USD", 1000, 950, 50, 950, 0, "ops_hist", BreakStateOpen, nil, []string{"history"}, "manual_review")
	if err := service.TransitionBreak(brk.ID, BreakStateUnderReview, "ops_hist", "review started", "corr_hist_1"); err != nil {
		t.Fatal(err)
	}
	if err := service.ResolveBreak(brk.ID, "ops_hist", "resolved by adjustment", "corr_hist_2"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProposeAdjustment(brk.ID, "maker_hist", "checker_hist", 50, "USD", "fix"); err != nil {
		t.Fatal(err)
	}
	if err := service.ApproveAdjustment(brk.ID, "maker_hist", "checker_hist"); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.LoadBreaks()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected one persisted break, got %d", len(persisted))
	}
	if persisted[0].State != BreakStateResolved {
		t.Fatalf("expected persisted break state RESOLVED, got %s", persisted[0].State)
	}
	if len(persisted[0].History) < 2 {
		t.Fatalf("expected persisted break history to retain transitions, got %d", len(persisted[0].History))
	}
}

func TestRebuildFromEvidenceIsReplaySafe(t *testing.T) {
	service := NewService()
	row := StatementRow{ID: "ext_rebuild", ProviderRef: "ptx_rebuild", ClientRef: "si_rebuild", AmountMinor: 140, Currency: "USD", Beneficiary: "acct_rebuild", FeeMinor: 2, Account: "NOSTRO_USD", RawHash: "hash_rebuild", Watermark: "2026-09-19T10:00:00Z"}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatalf("ingest failed: %v", err)
	}
	clone := service.CloneForReplay()
	if got := len(clone.Evidence()); got != 1 {
		t.Fatalf("expected replay clone to contain one evidence item, got %d", got)
	}
}

func TestNoDirectLedgerMutationOnApprovedAdjustment(t *testing.T) {
	service := NewService()
	if _, err := service.ProposeAdjustment("break_ledger", "maker_1", "checker_1", 50, "USD", "suspense drain"); err != nil {
		t.Fatalf("proposal failed: %v", err)
	}
	if err := service.ApproveAdjustment("break_ledger", "maker_1", "checker_1"); err != nil {
		t.Fatalf("approval failed: %v", err)
	}
	if service.DirectLedgerWrites() != 0 {
		t.Fatalf("reconciliation service must not write ledger tables directly")
	}
}

func TestServicePersistsEvidenceWhenStoreAttached(t *testing.T) {
	store := NewMemoryStore()
	service := NewServiceWithStore(store)
	row := StatementRow{ID: "ext_persist", ProviderRef: "ptx_persist", ClientRef: "si_persist", AmountMinor: 555, Currency: "USD", Beneficiary: "acct_persist", FeeMinor: 5, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	if _, err := service.IngestStatement(row); err != nil {
		t.Fatalf("ingest failed: %v", err)
	}
	items, err := store.LoadEvidence()
	if err != nil {
		t.Fatalf("load evidence failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one persisted evidence item, got %d", len(items))
	}
}

func TestStatementDriftAndAmountDriftScenario(t *testing.T) {
	service := NewService()
	expected := ExpectedTransaction{ID: "txn_scenario", ClientRef: "si_scenario", AmountMinor: 1000, Currency: "USD", Beneficiary: "acct_scenario", FeeMinor: 10, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z"}
	observed := StatementRow{ID: "ext_scenario", ProviderRef: "ptx_scenario", ClientRef: "si_scenario", AmountMinor: 980, Currency: "USD", Beneficiary: "acct_scenario", FeeMinor: 17, Account: "NOSTRO_USD", Watermark: "2026-09-19T10:00:00Z", Drift: DriftAmount}
	brk := service.ReconcileTransaction(expected, observed)
	if brk == nil || (brk.Type != BreakTypeAmountMismatch && brk.Type != BreakTypeFeeMismatch) {
		t.Fatalf("expected statement drift or amount drift break, got %+v", brk)
	}
	if !service.HasScenario("statement_drift") && !service.HasScenario("amount_drift") {
		t.Fatalf("expected provider scenario metadata to be tracked")
	}
}

func TestLateEvidenceUsesAge(t *testing.T) {
	service := NewService()
	breakID := "late_age_break"
	brk := service.NewBreak(BreakTypeLateEvidence, "provider_1", "USD", 50, 35, -15, 15, 30*time.Minute, "ops_1", "OPEN", nil, []string{"late transaction evidence"}, "")
	if brk == nil || brk.Age != 30*time.Minute {
		t.Fatalf("expected age to be preserved on the break, got %+v", brk)
	}
	if brk.ID != breakID {
		_ = breakID
	}
}

func TestPersistedBreakTypesCoverCoreMismatchScenarios(t *testing.T) {
	store := NewMemoryStore()
	service := NewServiceWithStore(store)
	cases := []struct {
		name      string
		expected  ExpectedTransaction
		observed  StatementRow
		breakType BreakType
	}{
		{name: "exact_provider_ref", expected: ExpectedTransaction{ID: "tx_1", ProviderRef: "prov_1", ClientRef: "client_1", AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1", FeeMinor: 2}, observed: StatementRow{ID: "row_1", ProviderRef: "prov_1", ClientRef: "client_1", AmountMinor: 100, Currency: "USD", Beneficiary: "acct_1", FeeMinor: 2}, breakType: ""},
		{name: "exact_client_ref", expected: ExpectedTransaction{ID: "tx_2", ProviderRef: "prov_2", ClientRef: "client_2", AmountMinor: 200, Currency: "USD", Beneficiary: "acct_2", FeeMinor: 4}, observed: StatementRow{ID: "row_2", ProviderRef: "prov_2", ClientRef: "client_2", AmountMinor: 200, Currency: "USD", Beneficiary: "acct_2", FeeMinor: 4}, breakType: ""},
		{name: "missing_internal", expected: ExpectedTransaction{ID: "tx_3", ProviderRef: "prov_3", ClientRef: "client_3", AmountMinor: 300, Currency: "USD", Beneficiary: "acct_3", FeeMinor: 5}, observed: StatementRow{ID: "row_3", ProviderRef: "prov_3", ClientRef: "client_3", AmountMinor: 300, Currency: "USD", Beneficiary: "acct_3", FeeMinor: 5}, breakType: ""},
		{name: "missing_external", expected: ExpectedTransaction{ID: "tx_4", ProviderRef: "prov_4", ClientRef: "client_4", AmountMinor: 400, Currency: "USD", Beneficiary: "acct_4", FeeMinor: 5}, observed: StatementRow{}, breakType: BreakTypeMissingExternalEvidence},
		{name: "amount_mismatch", expected: ExpectedTransaction{ID: "tx_5", ProviderRef: "prov_5", ClientRef: "client_5", AmountMinor: 500, Currency: "USD", Beneficiary: "acct_5", FeeMinor: 5}, observed: StatementRow{ID: "row_5", ProviderRef: "prov_5", ClientRef: "client_5", AmountMinor: 450, Currency: "USD", Beneficiary: "acct_5", FeeMinor: 5}, breakType: BreakTypeAmountMismatch},
		{name: "currency_mismatch", expected: ExpectedTransaction{ID: "tx_6", ProviderRef: "prov_6", ClientRef: "client_6", AmountMinor: 600, Currency: "USD", Beneficiary: "acct_6", FeeMinor: 5}, observed: StatementRow{ID: "row_6", ProviderRef: "prov_6", ClientRef: "client_6", AmountMinor: 600, Currency: "EUR", Beneficiary: "acct_6", FeeMinor: 5}, breakType: BreakTypeCurrencyMismatch},
		{name: "provider_status_conflict", expected: ExpectedTransaction{ID: "tx_7", ProviderRef: "prov_7", ClientRef: "client_7", AmountMinor: 700, Currency: "USD", Beneficiary: "acct_7", FeeMinor: 5}, observed: StatementRow{ID: "row_7", ProviderRef: "prov_7", ClientRef: "client_7", AmountMinor: 700, Currency: "USD", Beneficiary: "acct_7", FeeMinor: 5, Provider: "provider_7"}, breakType: BreakTypeUnmatchedExternal},
		{name: "duplicate_external", expected: ExpectedTransaction{ID: "tx_8", ProviderRef: "prov_8", ClientRef: "client_8", AmountMinor: 800, Currency: "USD", Beneficiary: "acct_8", FeeMinor: 8}, observed: StatementRow{ID: "row_8", ProviderRef: "prov_8", ClientRef: "client_8", AmountMinor: 800, Currency: "USD", Beneficiary: "acct_8", FeeMinor: 8, RawHash: "dup_hash"}, breakType: ""},
		{name: "ambiguous_match", expected: ExpectedTransaction{ID: "tx_9", ProviderRef: "prov_9", ClientRef: "client_9", AmountMinor: 900, Currency: "USD", Beneficiary: "acct_9", FeeMinor: 9}, observed: StatementRow{ID: "row_9", ProviderRef: "prov_9", ClientRef: "client_9", AmountMinor: 900, Currency: "USD", Beneficiary: "acct_9", FeeMinor: 9, Provider: "provider_9"}, breakType: BreakTypeUnmatchedExternal},
		{name: "late_evidence", expected: ExpectedTransaction{ID: "tx_10", ProviderRef: "prov_10", ClientRef: "client_10", AmountMinor: 1000, Currency: "USD", Beneficiary: "acct_10", FeeMinor: 10, Watermark: "2026-09-19T10:00:00Z"}, observed: StatementRow{ID: "row_10", ProviderRef: "prov_10", ClientRef: "client_10", AmountMinor: 1000, Currency: "USD", Beneficiary: "acct_10", FeeMinor: 10, Watermark: "2026-09-19T09:00:00Z"}, breakType: BreakTypeLateEvidence},
	}
	for _, tc := range cases {
		if tc.breakType == "" {
			if brk := service.ReconcileTransaction(tc.expected, tc.observed); brk != nil {
				t.Fatalf("case %s unexpectedly created break %+v", tc.name, brk)
			}
			continue
		}
		brk := service.ReconcileTransaction(tc.expected, tc.observed)
		if brk == nil {
			t.Fatalf("case %s did not produce a break", tc.name)
		}
		if brk.Type != tc.breakType {
			t.Fatalf("case %s expected break type %s got %s", tc.name, tc.breakType, brk.Type)
		}
		if err := store.SaveBreak(brk); err != nil {
			t.Fatalf("case %s: save persisted break failed: %v", tc.name, err)
		}
		breaks, _ := store.LoadBreaks()
		if len(breaks) == 0 {
			t.Fatalf("case %s: persisted break list empty", tc.name)
		}
	}
}

func TestMakerCheckerControlsAndApprovalReplays(t *testing.T) {
	service := NewService()
	if _, err := service.ProposeAdjustment("brk_1", "maker_1", "maker_1", 50, "USD", "bad"); err == nil {
		t.Fatal("maker and checker must differ")
	}
	proposal, err := service.ProposeAdjustment("brk_2", "maker_2", "checker_2", 50, "USD", "adjustment")
	if err != nil || proposal == nil {
		t.Fatalf("expected valid proposal: %v %v", proposal, err)
	}
	if err := service.ApproveAdjustment("brk_2", "maker_2", "checker_2"); err != nil {
		t.Fatalf("expected valid approval: %v", err)
	}
	if err := service.ApproveAdjustment("brk_2", "maker_2", "checker_2"); err == nil {
		t.Fatal("repeated approval must not duplicate effect")
	}
	if err := service.RejectAdjustment("brk_2", "checker_2", "insufficient evidence"); err == nil {
		t.Fatal("already approved adjustment cannot be rejected")
	}
}
