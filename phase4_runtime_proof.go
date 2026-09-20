//go:build ignore

package main

import (
    "database/sql"
    "fmt"
    "log"
    "os"

    _ "github.com/jackc/pgx/v5/stdlib"
    ledgerapi "github.com/osai/osai/services/ledger/ledgerapi"
    reconciliation "github.com/osai/osai/services/reconciliation"
)

func main() {
    dsn := os.Getenv("OSAI_POSTGRES_DSN")
    if dsn == "" {
        dsn = "postgres://osai:osai@localhost:5432/osai?sslmode=disable"
    }
    db, err := sql.Open("pgx", dsn)
    if err != nil { log.Fatal(err) }
    defer db.Close()
    if err := db.Ping(); err != nil { log.Fatal(err) }
    if _, err := db.Exec(`TRUNCATE TABLE reconciliation_evidence, reconciliation_breaks, reconciliation_match_results, reconciliation_watermarks, reconciliation_approvals, reconciliation_resolution_history RESTART IDENTITY`); err != nil { log.Fatal(err) }
    if err := reconciliation.EnsureSchema(db); err != nil { log.Fatal(err) }

    store := &reconciliation.PostgresStore{DB: db}
    svc := reconciliation.NewServiceWithStore(store)

    row := reconciliation.StatementRow{
        ID:          "phase4_evidence_20260920_001",
        Provider:    "prime",
        ProviderRef: "ptx_phase4_001",
        ClientRef:   "si_phase4_001",
        AmountMinor: 1250,
        Currency:    "USD",
        Beneficiary: "acct_phase4_001",
        FeeMinor:    12,
        Account:     "NOSTRO_USD",
        Watermark:   "2026-09-20T10:00:00Z",
    }
    if _, err := svc.IngestStatement(row); err != nil { log.Fatal(err) }

    expected := reconciliation.ExpectedTransaction{
        ID:          "txn_phase4_001",
        ClientRef:   "si_phase4_001",
        ProviderRef: "ptx_phase4_001",
        AmountMinor: 1000,
        Currency:    "USD",
        Beneficiary: "acct_phase4_001",
        FeeMinor:    12,
        Account:     "NOSTRO_USD",
        Watermark:   "2026-09-20T10:00:00Z",
    }
    brk := svc.ReconcileTransaction(expected, row)
    if brk == nil { log.Fatal("expected a break for amount mismatch") }
    if err := svc.TransitionBreak(brk.ID, reconciliation.BreakStateUnderReview, "ops_phase4", "review started", "corr_phase4_1"); err != nil { log.Fatal(err) }
    if _, err := svc.ProposeAdjustment(brk.ID, "maker_phase4", "checker_phase4", 50, "USD", "phase4 runtime adjustment"); err != nil { log.Fatal(err) }
    if err := svc.ApproveAdjustment(brk.ID, "maker_phase4", "checker_phase4"); err != nil { log.Fatal(err) }

    restarted := reconciliation.NewServiceWithStore(store)
    if got := len(restarted.Evidence()); got != 1 { log.Fatalf("restarted evidence count mismatch: got %d", got) }
    if !restarted.HasApprovedAdjustment(brk.ID) { log.Fatal("approval did not survive restart") }

    var evidenceRows, breakRows, approvalRows int
    if err := db.QueryRow("SELECT COUNT(*) FROM reconciliation_evidence").Scan(&evidenceRows); err != nil { log.Fatal(err) }
    if err := db.QueryRow("SELECT COUNT(*) FROM reconciliation_breaks").Scan(&breakRows); err != nil { log.Fatal(err) }
    if err := db.QueryRow("SELECT COUNT(*) FROM reconciliation_approvals").Scan(&approvalRows); err != nil { log.Fatal(err) }

    ledgerSvc := ledgerapi.NewService()
    firstID, firstReplay, err := ledgerSvc.PostReconciliationAdjustment(ledgerapi.ReconciliationAdjustmentCommand{
        IdempotencyKey: "phase4-proof-key-001",
        CorrelationID:  "corr_phase4_ledger_1",
        BreakID:        brk.ID,
        Maker:          "maker_phase4",
        Checker:        "checker_phase4",
        Currency:       "USD",
        AmountMinor:    50,
        Reason:         "phase4 runtime adjustment",
        JournalTag:     "proof",
    })
    if err != nil { log.Fatal(err) }
    secondID, secondReplay, err := ledgerSvc.PostReconciliationAdjustment(ledgerapi.ReconciliationAdjustmentCommand{
        IdempotencyKey: "phase4-proof-key-001",
        CorrelationID:  "corr_phase4_ledger_2",
        BreakID:        brk.ID,
        Maker:          "maker_phase4",
        Checker:        "checker_phase4",
        Currency:       "USD",
        AmountMinor:    50,
        Reason:         "phase4 runtime adjustment",
        JournalTag:     "proof",
    })
    if err != nil { log.Fatal(err) }

    journals := ledgerSvc.Journals()
    if len(journals) != 1 { log.Fatalf("expected exactly one reconciliation journal, got %d", len(journals)) }
    var debitTotal, creditTotal int64
    for _, entry := range journals[0].Entries {
        debitTotal += entry.Debit
        creditTotal += entry.Credit
    }
    if debitTotal != creditTotal { log.Fatalf("journal imbalance: debit=%d credit=%d", debitTotal, creditTotal) }

    fmt.Printf("evidence_rows=%d break_rows=%d approval_rows=%d\n", evidenceRows, breakRows, approvalRows)
    fmt.Printf("first_call_journal_id=%s already_applied=%v\n", firstID, firstReplay)
    fmt.Printf("second_call_journal_id=%s already_applied=%v same_journal=%v\n", secondID, secondReplay, firstID == secondID)
    fmt.Printf("journal_balance_ok=%v debit_total=%d credit_total=%d\n", debitTotal == creditTotal, debitTotal, creditTotal)
    fmt.Printf("restarted_evidence_count=%d restarted_approval=%v\n", len(restarted.Evidence()), restarted.HasApprovedAdjustment(brk.ID))
}
