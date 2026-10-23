package tradecore

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestSandboxInitialSubmissionCannotRetryVerifiedNonExecution(t *testing.T) {
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
	snapshot := crossCurrencyFXSnapshot(t)
	trade, err := store.CreateTradeWithPricing("sbx:institution", "sbx:"+uuid.NewString(), "sbx:accept", "sbx:correlation", 9500, "NGN", "USD", snapshot, "sbx:beneficiary", "sbx:provider", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM trade_fx_executions WHERE trade_id=$1`, trade.ID)
		db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
		db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
		db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
	})
	if err := store.SetStatus(trade.ID, StateFunded); err != nil {
		t.Fatal(err)
	}
	initial, err := store.CreateFXExecution(trade.ID, "sbx:intent", "sbx:executor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkFXExecutionSubmitted(trade.ID); err != nil {
		t.Fatal(err)
	}
	key := "local-" + strings.Repeat("x", 40)
	var mu sync.Mutex
	posts := 0
	server := signedFXFixture(t, key, func(p *SandboxFXPayload) { p.Status = FXQueryNotExecuted; p.Execution = nil; p.ProviderRef = "" }, &posts, &mu)
	defer server.Close()
	client, err := NewSandboxFXClient(server.URL, "sbx:executor", key)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := store.RecoverFXExecution(context.Background(), trade.ID, client, client); err != nil || recovered.Status != FXExecutionReady {
		t.Fatalf("recovery: %v %v", recovered, err)
	}
	if _, err := store.markFXExecutionSubmitted(trade.ID, initial); err == nil {
		t.Fatal("stale initial claim crossed recovery generation")
	}
	if _, err := store.submitFXExecution(context.Background(), trade.ID, client, client, true); err == nil {
		t.Fatal("automatic initial worker resubmitted a prior attempt")
	}
	final, found, err := store.GetFXExecution(trade.ID)
	if err != nil || !found || final.Status != FXExecutionReady || final.SubmissionCount != 1 || posts != 0 {
		t.Fatalf("stale worker mutated execution: %v %v posts=%d", final, err, posts)
	}
	if err := store.SetSandboxFundingRequired(trade.ID); err != nil {
		t.Fatal(err)
	}
	current, found := store.GetTrade(trade.ID)
	if !found || current.State != StateFXExecutionRequired {
		t.Fatal("delayed insufficient-funds result overwrote FX progress")
	}
}
