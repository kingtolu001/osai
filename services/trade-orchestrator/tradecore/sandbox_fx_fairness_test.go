package tradecore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/fxevidence"
)

func TestSandboxFXBadEvidenceDoesNotStarveOtherCompletedTrades(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof database required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateSandboxFXRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	executorID := "sbx:" + uuid.NewString()
	for k, v := range map[string]string{"OSAI_SANDBOX_ACCOUNTING_POLICY": "osai-sandbox-accounting/v1", "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": "http://127.0.0.1:1/v3", "FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": strings.Repeat("x", 32), "OSAI_SANDBOX_FX_ACCEPTANCE": "true", "OSAI_SANDBOX_FX_LEDGER_POSTING": "true", "OSAI_SANDBOX_FX_BASE_URL": "http://127.0.0.1:1", "OSAI_SANDBOX_FX_EXECUTOR_ID": executorID, "OSAI_SANDBOX_FX_SIGNING_KEY": "local-" + strings.Repeat("x", 32)} {
		t.Setenv(k, v)
	}
	ctx := context.Background()
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM trade_sandbox_fx_postings WHERE execution_id IN (SELECT execution_id FROM trade_fx_executions WHERE trade_id=$1)`, id)
			db.Exec(`DELETE FROM trade_sandbox_fx_reservations WHERE trade_id=$1`, id)
			db.Exec(`DELETE FROM trade_fx_executions WHERE trade_id=$1`, id)
			db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, id)
			db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, id)
			db.Exec(`DELETE FROM trades WHERE id=$1`, id)
		}
	})
	for range 2 {
		snapshot := crossCurrencyFXSnapshot(t)
		trade, err := store.CreateTradeWithPricing("sbx:fairness", "sbx:"+uuid.NewString(), "accept", "sbx:correlation", 9500, "NGN", "USD", snapshot, "sbx:beneficiary", "sbx:provider", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, trade.ID)
		if err := store.SetStatus(trade.ID, StateFunded); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateFXExecution(trade.ID, "sbx:intent", executorID); err != nil {
			t.Fatal(err)
		}
		executor := &controlledFXExecutor{query: func(e FXExecution) (FXQueryResult, error) {
			ref := "sbx:execution:" + e.ID
			return FXQueryResult{Status: FXQueryExecuted, QueryEvidence: testFXQueryEvidence(&e, ref), Execution: &fxevidence.FXExecutionEvidence{TradeID: e.TradeID, InstitutionID: e.InstitutionID, QuoteID: e.QuoteID, Executor: e.ExecutorID, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9500, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95, AppliedRate: "0.01", RateDirection: fxevidence.DestinationPerSource, ExecutionReference: ref, SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}}, nil
		}}
		if _, err := store.SubmitFXExecution(ctx, trade.ID, executor, testFXVerifier{}); err != nil {
			t.Fatal(err)
		}
		// Queue scheduling fixture only: no claim that a real Ledger reserved
		// these trades. The independent-process test verifies actual receipts.
		if _, err := db.Exec(`INSERT INTO trade_sandbox_fx_reservations(trade_id,event_id,revision,command_json,receipt_json) VALUES($1,$2,1,'{}','{}')`, trade.ID, "sbx:reservation:"+trade.ID); err != nil {
			t.Fatal(err)
		}
	}
	bad, _, err := store.GetFXExecution(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	bad.VerifiedResponse = []byte("corrupted")
	raw, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE trade_fx_executions SET body=$2::jsonb,updated_at=now()-interval '1 hour' WHERE trade_id=$1`, ids[0], string(raw)); err != nil {
		t.Fatal(err)
	}
	client, err := NewSandboxFXClient("http://127.0.0.1:1", executorID, "local-"+strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.ProcessSandboxFXAcceptedBatch(ctx, client, 1); err == nil || n != 0 {
		t.Fatalf("bad evidence not rejected: n=%d err=%v", n, err)
	}
	if n, err := store.ProcessSandboxFXAcceptedBatch(ctx, client, 1); err != nil || n != 1 {
		t.Fatalf("bad evidence starved valid trade: n=%d err=%v", n, err)
	}
	var queued string
	if err := db.QueryRow(`SELECT e.trade_id FROM trade_sandbox_fx_postings p JOIN trade_fx_executions e ON e.execution_id=p.execution_id WHERE e.executor_id=$1`, executorID).Scan(&queued); err != nil || queued != ids[1] {
		t.Fatalf("incorrect trade queued: %s %v", queued, err)
	}
}
