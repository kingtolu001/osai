package tradecore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresStaleFXFailureCannotChangeNewSubmission(t *testing.T) {
	for _, failure := range []string{"query", "verification", "submission"} {
		t.Run(failure, func(t *testing.T) {
			dsn := os.Getenv("OSAI_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("OSAI_POSTGRES_DSN required")
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
			snapshot := crossCurrencyFXSnapshot(t)
			trade, err := store.CreateTradeWithPricing("fixture-"+uuid.NewString(), "quote-"+uuid.NewString(), "accept", "corr", snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot, "beneficiary", "sim_fx_test", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				db.Exec(`DELETE FROM trade_fx_executions WHERE trade_id=$1`, trade.ID)
				db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
				db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
				db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
			}()
			if err := store.SetStatus(trade.ID, StateFunded); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateFXExecution(trade.ID, "fx", "sim_fx_test"); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			delayed := &delayedFailureExecutor{started: started, release: release, submitFailure: failure == "submission", queryFailure: failure == "query"}
			verifier := testFXVerifier{}
			if failure == "verification" {
				verifier.err = errors.New("untrusted old response")
			}
			if failure != "submission" {
				if _, err := store.MarkFXExecutionSubmitted(trade.ID); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan error, 1)
			go func() {
				var err error
				if failure == "submission" {
					_, err = store.SubmitFXExecution(context.Background(), trade.ID, delayed, verifier)
				} else {
					_, err = store.RecoverFXExecution(context.Background(), trade.ID, delayed, verifier)
				}
				finished <- err
			}()
			<-started
			newer := &controlledFXExecutor{query: func(execution FXExecution) (FXQueryResult, error) {
				return FXQueryResult{Status: FXQueryNotExecuted, QueryEvidence: testFXQueryEvidence(&execution, execution.RequestReference)}, nil
			}}
			if _, err := store.RecoverFXExecution(context.Background(), trade.ID, newer, testFXVerifier{}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.MarkFXExecutionSubmitted(trade.ID); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-finished; err == nil {
				t.Fatal("old failed operation unexpectedly succeeded")
			}
			current, found, err := store.GetFXExecution(trade.ID)
			if err != nil || !found || current.Status != FXExecutionSubmitted || current.SubmissionCount != 2 {
				t.Fatalf("old failure changed newer submission: %+v %v", current, err)
			}
		})
	}
}

type delayedFailureExecutor struct {
	started, release            chan struct{}
	submitFailure, queryFailure bool
}

func (e *delayedFailureExecutor) SubmitFX(context.Context, FXExecution) error {
	if e.submitFailure {
		close(e.started)
		<-e.release
		return errors.New("delayed submission timeout")
	}
	return nil
}
func (e *delayedFailureExecutor) QueryFX(_ context.Context, execution FXExecution) (FXQueryResult, error) {
	close(e.started)
	<-e.release
	if e.queryFailure {
		return FXQueryResult{}, errors.New("delayed query timeout")
	}
	return FXQueryResult{Status: FXQueryPending, QueryEvidence: testFXQueryEvidence(&execution, execution.RequestReference)}, nil
}
