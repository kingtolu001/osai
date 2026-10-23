package ledgerstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/ledger/ledgercore"
)

func TestSandboxPlannedFXReservesBeforeExecutionAndRecovers(t *testing.T) {
	db, store := sandboxAccountingDB(t)
	terms := sandboxTerms()
	terms.Plan = &sandboxaccounting.FXPlan{QuoteID: "sbx:quote", Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 100, RateDirection: ledgercore.DestinationPerSource}
	sandboxApply(t, store, "fund", terms)
	sandboxApply(t, store, "reserve", terms)
	if sandboxBalance(t, db, "USD", "held") != 0 {
		t.Fatal("reservation invented destination funds before FX execution")
	}
	command := sandboxCommand("fx_complete", terms)
	command.ExpectedRevision = 2
	ctx := context.Background()
	if _, _, err := store.Apply(ctx, command); err == nil {
		t.Fatal("FX completed without execution evidence")
	}
	execution := &ledgercore.FXExecutionEvidence{TradeID: terms.TradeID, InstitutionID: terms.InstitutionID, QuoteID: terms.Plan.QuoteID, Executor: terms.Plan.Executor, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: terms.PrincipalMinor, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 100, AppliedRate: "0.01", RateDirection: ledgercore.DestinationPerSource, ExecutionReference: "sbx:execution", SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
	seal := func(c *SandboxAccountingCommand) {
		raw, err := json.Marshal(c.Execution)
		if err != nil {
			t.Fatal(err)
		}
		c.EvidencePayload = string(raw)
		hash := sha256.Sum256(raw)
		c.EvidenceHash = hex.EncodeToString(hash[:])
	}
	command.Execution = execution
	seal(&command)
	for _, tc := range []struct {
		name   string
		change func(*SandboxAccountingCommand)
	}{
		{"different proceeds", func(c *SandboxAccountingCommand) { c.Execution.DestinationProceedsMinor++ }},
		{"different executor", func(c *SandboxAccountingCommand) { c.Execution.Executor = "sbx:other" }},
		{"different institution", func(c *SandboxAccountingCommand) { c.Execution.InstitutionID = "sbx:other" }},
		{"nonfixture custody", func(c *SandboxAccountingCommand) { c.Execution.DestinationEvidenceRef = "real-custody" }},
		{"unapproved provider fee", func(c *SandboxAccountingCommand) {
			c.Execution.Fees = []ledgercore.FXFeeEvidence{{Kind: "charge", AmountMinor: 1, Currency: "USD", EvidenceReference: "sbx:fee"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := command
			copy := *execution
			bad.Execution = &copy
			tc.change(&bad)
			seal(&bad)
			if _, _, err := store.Apply(ctx, bad); err == nil {
				t.Fatal("conflicting execution accepted")
			}
		})
	}
	if sandboxBalance(t, db, "NGN", "held") != -10200 || sandboxBalance(t, db, "USD", "held") != 0 {
		t.Fatal("rejected evidence changed funds")
	}
	// A destination-leg write failure must not consume the source principal.
	if _, err := db.Exec(`CREATE FUNCTION reject_planned_usd() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.currency='USD' THEN RAISE EXCEPTION 'injected destination failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_usd BEFORE INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_planned_usd()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Apply(ctx, command); err == nil {
		t.Fatal("destination failure accepted")
	}
	if sandboxBalance(t, db, "NGN", "held") != -10200 || sandboxBalance(t, db, "USD", "held") != 0 {
		t.Fatal("destination failure consumed source funds")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_usd ON ledger_entries; DROP FUNCTION reject_planned_usd()`); err != nil {
		t.Fatal(err)
	}
	if state, replay, err := store.Apply(ctx, command); err != nil || replay || state != "FX_HELD" {
		t.Fatalf("post FX: %s %t %v", state, replay, err)
	}
	restarted, err := NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if state, replay, err := restarted.Apply(ctx, command); err != nil || !replay || state != "FX_HELD" {
		t.Fatalf("replay FX: %s %t %v", state, replay, err)
	}
	for _, event := range []string{"payout_failed", "retry_payout", "unknown", "payout_complete", "return", "release_return", "refund_fee"} {
		sandboxApply(t, restarted, event, terms)
	}
	if sandboxBalance(t, db, "USD", "available") != -100 || sandboxBalance(t, db, "NGN", "available") != -200 || sandboxBalance(t, db, "NGN", "held") != 0 || sandboxBalance(t, db, "USD", "held") != 0 {
		t.Fatal("recovery lost funds or fabricated NGN principal on a USD return")
	}
}

func TestSandboxFXReservationsSharePrefundWithoutCreatingMoney(t *testing.T) {
	db, store := sandboxAccountingDB(t)
	ctx := context.Background()
	deposit := sandboxTerms()
	sandboxApply(t, store, "fund", deposit)
	commands := make([]SandboxAccountingCommand, 2)
	for i := range commands {
		terms := sandboxTerms()
		terms.Plan = &sandboxaccounting.FXPlan{QuoteID: "sbx:quote", Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 100, RateDirection: ledgercore.DestinationPerSource}
		commands[i] = sandboxCommand("reserve", terms)
	}
	type outcome struct {
		command SandboxAccountingCommand
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, command := range commands {
		wg.Add(1)
		go func(c SandboxAccountingCommand) {
			defer wg.Done()
			_, _, err := store.Apply(ctx, c)
			results <- outcome{c, err}
		}(command)
	}
	wg.Wait()
	close(results)
	winners, refused := 0, 0
	for result := range results {
		if result.err == nil {
			winners++
			if state, replay, err := store.Apply(ctx, result.command); err != nil || !replay || state != "HELD" {
				t.Fatalf("reservation replay: %s %t %v", state, replay, err)
			}
		} else if errors.Is(result.err, ledgerapi.ErrInsufficientFunding) {
			refused++
		} else {
			t.Fatal(result.err)
		}
	}
	if winners != 1 || refused != 1 || sandboxBalance(t, db, "NGN", "held") != -10200 || sandboxBalance(t, db, "NGN", "available") != 0 || sandboxBalance(t, db, "USD", "held") != 0 {
		t.Fatal("concurrent reservations overdrew or created funds")
	}
	var positions, funds int
	if err := db.QueryRow(`SELECT count(*) FROM ledger_sandbox_positions`).Scan(&positions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ledger_sandbox_events WHERE command_json::jsonb->>'Event'='fund'`).Scan(&funds); err != nil {
		t.Fatal(err)
	}
	if positions != 2 || funds != 1 {
		t.Fatal("refused reservation left a position or manufactured funding")
	}
}
