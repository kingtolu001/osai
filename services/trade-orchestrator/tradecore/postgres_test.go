package tradecore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/services/ledger/ledgercore"
)

func TestPostgresFXExecutionUnknownQueryRecoveryAndRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	verifier := testFXVerifier{}
	snapshot := crossCurrencyFXSnapshot(t)
	institutionID := "inst_fx_lifecycle_" + uuid.NewString()
	quoteID := "quo_fx_lifecycle_" + uuid.NewString()
	trade, err := store.CreateTradeWithPricing(institutionID, quoteID, "accept-fx", "corr-fx",
		snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot,
		"ben_fx", "sim_fx_test", "fx-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_fx_executions WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
	})
	if err = store.SetStatus(trade.ID, StateFunded); err != nil {
		t.Fatal(err)
	}

	type createResult struct {
		execution *FXExecution
		err       error
	}
	results := make(chan createResult, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			execution, createErr := store.CreateFXExecution(trade.ID, "fx-key", "sim_fx_test")
			results <- createResult{execution: execution, err: createErr}
		}()
	}
	wait.Wait()
	close(results)
	var execution *FXExecution
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if execution != nil && result.execution.ID != execution.ID {
			t.Fatalf("concurrent create produced different FX execution IDs: %s / %s", execution.ID, result.execution.ID)
		}
		execution = result.execution
	}
	if execution == nil || execution.Status != FXExecutionReady {
		t.Fatalf("FX execution was not durably prepared: %+v", execution)
	}
	if _, err = store.CreateFXExecution(trade.ID, "fx-key", "different-executor"); err == nil {
		t.Fatal("executor change reused an existing FX idempotency key")
	}

	uncertain := &testFXExecutor{submitErr: errors.New("connection reset after submit"), queryErr: errors.New("query unavailable")}
	unknown, err := store.SubmitFXExecution(context.Background(), trade.ID, uncertain, verifier)
	if err == nil || unknown == nil || unknown.Status != FXExecutionUnknown || uncertain.submitCalls != 1 {
		t.Fatalf("ambiguous submission did not persist UNKNOWN: execution=%+v calls=%d err=%v", unknown, uncertain.submitCalls, err)
	}
	if tradeView, found := store.GetTrade(trade.ID); !found ||
		tradeView.State != StateFXUnknown || tradeView.FXExecutionID != execution.ID ||
		tradeView.FXExecutorID != "sim_fx_test" || tradeView.FXExecutionReference != execution.RequestReference {
		t.Fatalf("customer trade view did not expose durable FX state: %+v found=%v", tradeView, found)
	}
	if _, err = store.SubmitFXExecution(context.Background(), trade.ID, uncertain, verifier); err == nil || uncertain.submitCalls != 1 {
		t.Fatalf("unknown FX execution was blindly resubmitted: calls=%d err=%v", uncertain.submitCalls, err)
	}
	restartedUnknown, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	unknownAfterRestart, found, err := restartedUnknown.GetFXExecution(trade.ID)
	if err != nil || !found || unknownAfterRestart.Status != FXExecutionUnknown {
		t.Fatalf("UNKNOWN FX state did not survive restart: execution=%+v found=%v err=%v", unknownAfterRestart, found, err)
	}
	recoveryQueue, err := restartedUnknown.ListFXExecutionsForRecovery(100)
	queueContainsTrade := false
	for _, queued := range recoveryQueue {
		if queued.ID == execution.ID && queued.TradeID == trade.ID && queued.InstitutionID == institutionID {
			queueContainsTrade = true
		}
	}
	if err != nil || !queueContainsTrade {
		t.Fatalf("persisted UNKNOWN execution missing from restart recovery queue: items=%+v err=%v", recoveryQueue, err)
	}
	if _, err = restartedUnknown.SubmitFXExecution(context.Background(), trade.ID, uncertain, verifier); err == nil || uncertain.submitCalls != 1 {
		t.Fatalf("restarted UNKNOWN execution was blindly submitted: calls=%d err=%v", uncertain.submitCalls, err)
	}

	nonExecution := FXQueryResult{Status: FXQueryNotExecuted, QueryEvidence: testFXQueryEvidence(unknown, unknown.RequestReference)}
	uncertain.queryResult = nonExecution
	uncertain.queryErr = nil
	badEvidence := nonExecution
	badEvidence.QueryEvidence.ExecutorID = "other-executor"
	if _, err = restartedUnknown.applyFXQueryResult(trade.ID, badEvidence); err == nil {
		t.Fatal("query evidence from a different executor was accepted")
	}
	tamperedHash := nonExecution
	tamperedHash.QueryEvidence.PayloadHash = strings.Repeat("0", 64)
	if _, err = restartedUnknown.applyFXQueryResult(trade.ID, tamperedHash); err == nil {
		t.Fatal("query evidence hash that does not match response bytes was accepted")
	}
	if _, err = restartedUnknown.RecoverFXExecution(context.Background(), trade.ID, uncertain,
		testFXVerifier{err: errors.New("provider signature invalid")}); err == nil {
		t.Fatal("query result without successful evidence verification was accepted")
	}
	ready, err := restartedUnknown.RecoverFXExecution(context.Background(), trade.ID, uncertain, verifier)
	if err != nil || ready.Status != FXExecutionReady || ready.RequestReference != execution.RequestReference {
		t.Fatalf("query-first recovery did not safely restore the same request: execution=%+v err=%v", ready, err)
	}

	expectedEvidence := ledgercore.FXExecutionEvidence{
		TradeID: trade.ID, InstitutionID: institutionID, QuoteID: quoteID, Executor: "sim_fx_test",
		SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9_500,
		DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95,
		AppliedRate: "0.01", RateDirection: ledgercore.DestinationPerSource,
		ExecutionReference: "provider-fx-001", SourceEvidenceReference: "source-evidence-001",
		DestinationEvidenceRef: "destination-evidence-001", OccurredAt: time.Now().UTC(),
	}
	executor := &testFXExecutor{queryResult: FXQueryResult{
		Status: FXQueryExecuted, QueryEvidence: testFXQueryEvidence(ready, expectedEvidence.ExecutionReference),
		Execution: &expectedEvidence,
	}}
	completed, err := restartedUnknown.SubmitFXExecution(context.Background(), trade.ID, executor, verifier)
	if err != nil || completed.Status != FXExecutionComplete || executor.submitCalls != 1 ||
		executor.lastSubmission.RequestReference != execution.RequestReference {
		t.Fatalf("FX submit/query finalization failed: execution=%+v submitted=%+v err=%v",
			completed, executor.lastSubmission, err)
	}

	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	persisted, found, err := restarted.GetFXExecution(trade.ID)
	if err != nil || !found || persisted.Status != FXExecutionComplete ||
		persisted.ExecutionEvidence == nil || persisted.ExecutionEvidence.ExecutionReference != "provider-fx-001" {
		t.Fatalf("completed FX evidence did not survive service restart: execution=%+v found=%v err=%v", persisted, found, err)
	}
	persistedTrade, found := restarted.GetTrade(trade.ID)
	if !found || persistedTrade.State != StateFXExecuted || persistedTrade.FXExecutionStatus != string(FXExecutionComplete) {
		t.Fatalf("restarted customer trade view lost FX status: %+v found=%v", persistedTrade, found)
	}
	if _, err = restarted.applyFXQueryResult(trade.ID, executor.queryResult); err == nil {
		t.Fatal("completed FX execution accepted a second finality mutation")
	}
	var executedEvents int
	if err = db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.fx_executed'`, trade.ID).Scan(&executedEvents); err != nil {
		t.Fatal(err)
	}
	if executedEvents != 1 {
		t.Fatalf("FX completion event count = %d, want one", executedEvents)
	}
	var unknownEvents int
	if err = db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.fx_execution_unknown'`, trade.ID).Scan(&unknownEvents); err != nil {
		t.Fatal(err)
	}
	if unknownEvents == 0 {
		t.Fatal("UNKNOWN lifecycle transition was not durably published")
	}
}

func TestPostgresFXRecoveryGenerationRejectsStaleNotExecutedResult(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	verifier := testFXVerifier{}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := crossCurrencyFXSnapshot(t)
	institutionID := "inst_fx_fence_" + uuid.NewString()
	quoteID := "quo_fx_fence_" + uuid.NewString()
	trade, err := store.CreateTradeWithPricing(institutionID, quoteID, "accept-fx-fence", "corr-fx-fence",
		snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot,
		"ben_fx", "sim_fx_test", "fx-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_fx_executions WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
	})
	if err = store.SetStatus(trade.ID, StateFunded); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateFXExecution(trade.ID, "fx-fence-key", "sim_fx_test"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.MarkFXExecutionSubmitted(trade.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.MarkFXExecutionUnknown(trade.ID); err != nil {
		t.Fatal(err)
	}

	staleQueryStarted := make(chan struct{})
	releaseStaleQuery := make(chan struct{})
	staleExecutor := &controlledFXExecutor{query: func(execution FXExecution) (FXQueryResult, error) {
		close(staleQueryStarted)
		<-releaseStaleQuery
		return FXQueryResult{Status: FXQueryNotExecuted,
			QueryEvidence: testFXQueryEvidence(&execution, execution.RequestReference)}, nil
	}}
	staleResult := make(chan error, 1)
	go func() {
		_, recoverErr := store.RecoverFXExecution(context.Background(), trade.ID, staleExecutor, verifier)
		staleResult <- recoverErr
	}()
	<-staleQueryStarted // The first query has claimed generation 1 and is now delayed.

	executedEvidence := ledgercore.FXExecutionEvidence{
		TradeID: trade.ID, InstitutionID: institutionID, QuoteID: quoteID, Executor: "sim_fx_test",
		SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9_500,
		DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95,
		AppliedRate: "0.01", RateDirection: ledgercore.DestinationPerSource,
		ExecutionReference: "provider-fx-fenced", SourceEvidenceReference: "source-fenced",
		DestinationEvidenceRef: "destination-fenced", OccurredAt: time.Now().UTC(),
	}
	currentExecutor := &controlledFXExecutor{query: func(execution FXExecution) (FXQueryResult, error) {
		return FXQueryResult{Status: FXQueryExecuted,
			QueryEvidence: testFXQueryEvidence(&execution, executedEvidence.ExecutionReference),
			Execution:     &executedEvidence}, nil
	}}
	completed, err := store.RecoverFXExecution(context.Background(), trade.ID, currentExecutor, verifier)
	if err != nil || completed.Status != FXExecutionComplete || completed.RecoveryGeneration != 2 {
		t.Fatalf("newest verified query did not win: execution=%+v err=%v", completed, err)
	}
	close(releaseStaleQuery)
	if err = <-staleResult; err == nil || !strings.Contains(err.Error(), "stale FX query result") {
		t.Fatalf("delayed non-execution result was not fenced: err=%v", err)
	}
	final, found, err := store.GetFXExecution(trade.ID)
	if err != nil || !found || final.Status != FXExecutionComplete {
		t.Fatalf("stale query re-enabled submission: execution=%+v found=%v err=%v", final, found, err)
	}
}

type controlledFXExecutor struct {
	query func(FXExecution) (FXQueryResult, error)
}

func (executor *controlledFXExecutor) SubmitFX(context.Context, FXExecution) error { return nil }

func (executor *controlledFXExecutor) QueryFX(_ context.Context, execution FXExecution) (FXQueryResult, error) {
	return executor.query(execution)
}

type testFXVerifier struct{ err error }

func (verifier testFXVerifier) VerifyFXQuery(_ context.Context, execution FXExecution, result FXQueryResult) error {
	if verifier.err != nil {
		return verifier.err
	}
	return validateFXQueryEvidence(&execution, result.QueryEvidence)
}

type testFXExecutor struct {
	submitErr      error
	queryErr       error
	queryResult    FXQueryResult
	submitCalls    int
	lastSubmission FXExecution
}

func (executor *testFXExecutor) SubmitFX(_ context.Context, execution FXExecution) error {
	executor.submitCalls++
	executor.lastSubmission = execution
	return executor.submitErr
}

func (executor *testFXExecutor) QueryFX(_ context.Context, execution FXExecution) (FXQueryResult, error) {
	result := executor.queryResult
	result.QueryEvidence.RecoveryGeneration = execution.RecoveryGeneration
	return result, executor.queryErr
}

func testFXQueryEvidence(execution *FXExecution, providerRef string) FXQueryEvidence {
	rawPayload := []byte("verified-test-response:" + execution.RequestReference + ":" + providerRef)
	payloadHash := sha256.Sum256(rawPayload)
	return FXQueryEvidence{
		ExecutorID: execution.ExecutorID, RequestReference: execution.RequestReference,
		RecoveryGeneration: execution.RecoveryGeneration,
		ProviderRef:        providerRef, QueryReference: "query-" + uuid.NewString(),
		PayloadHash: hex.EncodeToString(payloadHash[:]), ObservedAt: time.Now().UTC(), RawPayload: rawPayload,
	}
}

func crossCurrencyFXSnapshot(t *testing.T) pricing.Snapshot {
	t.Helper()
	snapshot := pricing.Snapshot{
		Mode: "FIXED_SOURCE", RequestedAmountMinor: 10_000, SourcePrincipalMinor: 9_500,
		FeeMinor: 500, TotalDebitMinor: 10_000, DestinationAmountMinor: 95,
		SourceCurrency: "NGN", DestinationCurrency: "USD", RateScaled: 1_000_000,
		RateDirection: "USD_PER_NGN", RateScale: pricing.RateScale,
		SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "fx_fee_test", FeeScheduleVersion: 1,
		FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500,
		MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1_000_000,
		MinimumDebitMinor: 1, MaximumDebitMinor: 2_000_000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 2_000_000,
		ExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}
	hash, err := pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Hash = hash
	if err = pricing.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPostgresListTradesFiltersAndPaginates(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}

	institutionID := "inst_trade_list_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE institution_id=$1`, institutionID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id IN (SELECT id FROM trades WHERE institution_id=$1)`, institutionID)
		_, _ = db.Exec(`DELETE FROM trades WHERE institution_id=$1`, institutionID)
	})
	first, err := store.CreateTrade(institutionID, "quo_"+uuid.NewString(), "key-1", "corr-1", 10000, "NGN", "USD", "", "", "order-42")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTrade(institutionID, "quo_"+uuid.NewString(), "key-2", "corr-2", 20000, "USD", "NGN", "", "", "order-99")
	if err != nil {
		t.Fatal(err)
	}
	otherInstitutionID := institutionID + "_other"
	other, err := store.CreateTrade(otherInstitutionID, "quo_"+uuid.NewString(), "key-3", "corr-3", 30000, "NGN", "USD", "", "", "order-42")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, other.ID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, other.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, other.ID)
	})
	if err := store.SetStatus(second.ID, StateFunded); err != nil {
		t.Fatal(err)
	}
	var acceptedEvents, fundedEvents int
	if err := db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.accepted'`, first.ID).Scan(&acceptedEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.funded'`, second.ID).Scan(&fundedEvents); err != nil {
		t.Fatal(err)
	}
	if acceptedEvents != 1 || fundedEvents != 1 {
		t.Fatalf("trade state outbox events missing or duplicated: accepted=%d funded=%d", acceptedEvents, fundedEvents)
	}

	filter := TradeListFilter{
		Page: 1, PageSize: 10, From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC().Add(time.Hour),
		Currency: "nGn", Status: string(StateAccepted), CustomerReference: "order-42",
	}
	trades, total, err := store.ListTrades(institutionID, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(trades) != 1 || trades[0].ID != first.ID {
		t.Fatalf("unexpected filtered trades: total=%d trades=%+v", total, trades)
	}

	filter = TradeListFilter{Page: 1, PageSize: 1}
	pageOne, total, err := store.ListTrades(institutionID, filter)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(pageOne) != 1 || pageOne[0].ID != second.ID {
		t.Fatalf("unexpected first page: total=%d trades=%+v", total, pageOne)
	}
}

func TestPostgresPricingSnapshotPersistsAndRejectsAlteredReplay(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OSAI_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	institutionID := "inst_pricing_snapshot_" + uuid.NewString()
	quoteID := "quo_pricing_snapshot_" + uuid.NewString()
	snapshot := persistedFeeSnapshot(t)
	trade, err := store.CreateTradeWithPricing(institutionID, quoteID, "accept-first", "corr-first",
		snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot,
		"", "sim_lp_1", "pricing-order")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
		_, _ = db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
	})
	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := restarted.GetByQuoteID(quoteID)
	if !ok || loaded.PricingSnapshot == nil || *loaded.PricingSnapshot != snapshot {
		t.Fatalf("pricing snapshot did not survive Trade store restart: trade=%+v found=%v", loaded, ok)
	}

	type result struct {
		trade *Trade
		err   error
	}
	results := make(chan result, 2)
	for _, key := range []string{"accept-retry-a", "accept-retry-b"} {
		go func(key string) {
			replayed, replayErr := restarted.CreateTradeWithPricing(institutionID, quoteID, key, "corr-retry",
				snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot,
				"", "sim_lp_1", "pricing-order")
			results <- result{trade: replayed, err: replayErr}
		}(key)
	}
	for i := 0; i < 2; i++ {
		replayed := <-results
		if replayed.err != nil || replayed.trade == nil || replayed.trade.ID != trade.ID {
			t.Fatalf("concurrent pricing replay created a second trade: trade=%+v err=%v", replayed.trade, replayed.err)
		}
	}

	altered := snapshot
	altered.ExpiresAt = time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	altered.Hash, err = pricing.Hash(altered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.CreateTradeWithPricing(institutionID, quoteID, "accept-altered", "corr-altered",
		altered.SourcePrincipalMinor, altered.SourceCurrency, altered.DestinationCurrency, altered,
		"", "sim_lp_1", "pricing-order"); err == nil {
		t.Fatal("altered immutable pricing snapshot replay accepted")
	}
}

func persistedFeeSnapshot(t *testing.T) pricing.Snapshot {
	t.Helper()
	snapshot := pricing.Snapshot{
		Mode: "FIXED_SOURCE", RequestedAmountMinor: 10_000, SourcePrincipalMinor: 9_500,
		FeeMinor: 500, TotalDebitMinor: 10_000, DestinationAmountMinor: 9_500,
		SourceCurrency: "NGN", DestinationCurrency: "NGN", RateScaled: pricing.RateScale,
		RateDirection: "NGN_PER_NGN", RateScale: pricing.RateScale,
		SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "fee_test", FeeScheduleVersion: 1,
		FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500,
		MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1_000_000,
		MinimumDebitMinor: 1, MaximumDebitMinor: 2_000_000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 2_000_000,
		ExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}
	hash, err := pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Hash = hash
	if err = pricing.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
