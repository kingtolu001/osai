package settlementstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/sandboxaccounting"
)

func TestSandboxPayoutDispatchFenceAndFrozenOutcomeSurviveRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof PostgreSQL required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	p := Postgres{DB: db}
	ctx := context.Background()
	if err = p.EnsureSandboxPayoutSchema(ctx); err != nil {
		t.Fatal(err)
	}
	i, err := p.CreateForTrade(ctx, "sbx:payout-store:"+uuid.NewString(), "trd_"+uuid.NewString(), "sbx:quote", "sbx:correlation", "sbx:beneficiary", "USD", 95, "fixture payout", "sbx:provider", "", "")
	if err != nil {
		t.Fatal(err)
	}
	terms := sandboxaccounting.Terms{TradeID: i.TradeID, InstitutionID: i.InstitutionID, PrincipalMinor: 9500, FeeMinor: 500, Plan: &sandboxaccounting.FXPlan{QuoteID: i.QuoteID, Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 95, BeneficiaryID: i.Beneficiary, ProviderID: i.ProviderID}}
	position := sandboxaccounting.Position{Terms: terms, State: "FX_HELD", Revision: 2}
	for _, bad := range []sandboxaccounting.Position{{Terms: terms, State: "HELD", Revision: 1}, {Terms: terms, State: "UNKNOWN_FX_HELD", Revision: 2}} {
		if _, err = p.PrepareSandboxPayout(ctx, i, bad); err == nil {
			t.Fatal("unverified destination funds accepted")
		}
	}
	wrong := i
	wrong.Beneficiary = "sbx:another-approved-beneficiary"
	if _, err = p.PrepareSandboxPayout(ctx, wrong, position); err == nil {
		t.Fatal("accepted beneficiary replaced")
	}
	if _, err = p.PrepareSandboxPayout(ctx, i, position); err != nil {
		t.Fatal(err)
	}
	if won, err := p.AdvanceSandboxPayout(ctx, i.ID, "dispatch"); err != nil || won {
		t.Fatal("dispatch granted before Ledger acknowledgements")
	}
	for _, step := range []string{"bind", "submit"} {
		if _, err = p.AdvanceSandboxPayout(ctx, i.ID, step); err != nil {
			t.Fatal(err)
		}
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := p.AdvanceSandboxPayout(ctx, i.ID, "dispatch")
			if err != nil {
				failures <- err
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if wins.Load() != 1 {
		t.Fatalf("provider POST grants=%d", wins.Load())
	}
	// A separate connection models another worker after the first process died
	// immediately after the dispatch fence, before any provider reply existed.
	reopened, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := Postgres{DB: reopened}
	job, err := restarted.SandboxPayoutJob(ctx, i.ID)
	if err != nil || !job.DispatchStarted || !job.SubmitAck {
		t.Fatalf("dispatch recovery: %+v %v", job, err)
	}
	if won, err := restarted.AdvanceSandboxPayout(ctx, i.ID, "dispatch"); err != nil || won {
		t.Fatal("restart authorized a second POST")
	}
	first := SandboxPayoutCommand(i, terms, "payout_complete", 4, `{"fixture":"original signed observation"}`)
	frozen, err := restarted.FreezeSandboxPayoutFinal(ctx, i.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	second := SandboxPayoutCommand(i, terms, "payout_complete", 4, `{"fixture":"new query nonce"}`)
	replayed, err := p.FreezeSandboxPayoutFinal(ctx, i.ID, second)
	if err != nil || replayed.EvidenceHash != frozen.EvidenceHash || replayed.EvidencePayload != first.EvidencePayload {
		t.Fatalf("frozen finality was overwritten: %v", err)
	}
	conflict := SandboxPayoutCommand(i, terms, "payout_failed", 4, `{"fixture":"contradictory outcome"}`)
	if _, err = p.FreezeSandboxPayoutFinal(ctx, i.ID, conflict); err == nil {
		t.Fatal("contradictory finality replaced the frozen command")
	}
	job, err = restarted.SandboxPayoutJob(ctx, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted sandboxaccounting.Command
	if json.Unmarshal([]byte(job.FinalCommand), &persisted) != nil || persisted.EvidenceHash != first.EvidenceHash {
		t.Fatal("durable final evidence changed")
	}
	if _, err = p.CreateForTrade(ctx, i.InstitutionID, i.TradeID, i.QuoteID, i.CorrelationID, "sbx:another-approved-beneficiary", i.Currency, i.AmountMinor, i.Purpose, i.ProviderID, "", ""); err == nil {
		t.Fatal("idempotent settlement replay changed beneficiary")
	}
}
