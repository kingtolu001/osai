package ledgergrpc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/fxevidence"
	"github.com/osai/osai/pkg/sandboxaccounting"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerstore"
)

func TestSandboxCustomerReadsPreserveRestrictedCurrencyAndActualCashFlows(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof PostgreSQL required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	accounting, err := ledgerstore.NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	terms := sandboxaccounting.Terms{TradeID: "sbx:read:" + id, InstitutionID: "sbx:read:" + id, PrincipalMinor: 10000, FeeMinor: 200, Plan: &sandboxaccounting.FXPlan{QuoteID: "sbx:quote", Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 100, RateDirection: fxevidence.DestinationPerSource}}
	for revision, event := range []string{"fund", "reserve", "fx_complete", "payout_failed"} {
		c := sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, ExpectedRevision: int64(revision), EventID: "sbx:" + id + ":" + event, Event: event, Actor: "sbx:fixture", CorrelationID: "sbx:correlation", Reason: "explicit customer read fixture", Terms: terms}
		payload := []byte(`{"fixture":true}`)
		if event == "fx_complete" {
			c.Execution = &fxevidence.FXExecutionEvidence{TradeID: terms.TradeID, InstitutionID: terms.InstitutionID, QuoteID: terms.Plan.QuoteID, Executor: terms.Plan.Executor, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 10000, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 100, AppliedRate: "0.01", RateDirection: fxevidence.DestinationPerSource, ExecutionReference: "sbx:execution", SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
			payload, _ = json.Marshal(c.Execution)
		}
		hash := sha256.Sum256(payload)
		c.EvidencePayload, c.EvidenceHash = string(payload), hex.EncodeToString(hash[:])
		if _, _, err = accounting.Apply(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{DB: db, SandboxToken: "fixture-read-token", SandboxAccounting: accounting}
	balances, err := server.GetCustomerBalances(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: terms.InstitutionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(balances.Balances) != 2 {
		t.Fatalf("missing currency claims: %v", balances)
	}
	for _, b := range balances.Balances {
		if b.AvailableMinor != 0 {
			t.Fatal("FX failure fabricated spendable funds")
		}
		if b.Currency == "NGN" && (b.HeldMinor != 200 || b.RestrictedMinor != 0) {
			t.Fatal("source fee hold omitted")
		}
		if b.Currency == "USD" && (b.HeldMinor != 0 || b.RestrictedMinor != 100) {
			t.Fatal("restricted destination claim omitted or mislabeled")
		}
	}
	transactions, err := server.ListCustomerTransactions(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: terms.InstitutionID, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if transactions.Total != 3 || len(transactions.Items) != 3 {
		t.Fatalf("cash-flow journal count=%d rows=%d; want funding and two FX legs", transactions.Total, len(transactions.Items))
	}
	seen := map[string]int{}
	for _, item := range transactions.Items {
		if item.AmountMinor == 0 {
			t.Fatal("internal claim transfer exposed as zero cash flow")
		}
		seen[item.Currency]++
	}
	if seen["NGN"] != 2 || seen["USD"] != 1 {
		t.Fatal("cash flows lost currency separation")
	}
	disabled := &Server{DB: db}
	hidden, err := disabled.GetCustomerBalances(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: terms.InstitutionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden.Balances) != 1 || hidden.Balances[0].AvailableMinor != 0 || hidden.Balances[0].HeldMinor != 0 || hidden.Balances[0].RestrictedMinor != 0 {
		t.Fatal("fixture roles enabled outside proof server")
	}
	other, err := server.ListCustomerTransactions(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: "sbx:unrelated", Page: 1, PageSize: 100})
	if err != nil || other.Total != 0 {
		t.Fatal("cross-institution journal exposure")
	}
}
