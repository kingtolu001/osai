package ledgerstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/services/ledger/ledgercore"
)

func readySandboxPayout(t *testing.T, s *SandboxAccounting) SandboxTerms {
	t.Helper()
	terms := sandboxTerms()
	terms.Plan = &sandboxaccounting.FXPlan{QuoteID: "sbx:quote", Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 100, RateDirection: ledgercore.DestinationPerSource, BeneficiaryID: "sbx:beneficiary", ProviderID: "sbx:provider"}
	sandboxApply(t, s, "fund", terms)
	sandboxApply(t, s, "reserve", terms)
	c := sandboxCommand("fx_complete", terms)
	c.ExpectedRevision = 2
	c.Execution = &ledgercore.FXExecutionEvidence{TradeID: terms.TradeID, InstitutionID: terms.InstitutionID, QuoteID: terms.Plan.QuoteID, Executor: terms.Plan.Executor, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: terms.PrincipalMinor, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 100, AppliedRate: "0.01", RateDirection: ledgercore.DestinationPerSource, ExecutionReference: "sbx:execution", SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
	raw, _ := json.Marshal(c.Execution)
	hash := sha256.Sum256(raw)
	c.EvidencePayload, c.EvidenceHash = string(raw), hex.EncodeToString(hash[:])
	if _, _, err := s.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return terms
}

func payoutCommand(event string, terms SandboxTerms, revision int64, id string) SandboxAccountingCommand {
	c := sandboxCommand(event, terms)
	c.ExpectedRevision = revision
	c.Payout = &sandboxaccounting.PayoutBinding{SettlementID: id, ClientReference: "sbx:ref:" + id, ProviderID: "sbx:provider", BeneficiaryID: "sbx:beneficiary", Currency: "USD", AmountMinor: 100}
	return c
}

func TestSandboxPayoutBindingSerializesProceedsAndSurvivesRestart(t *testing.T) {
	db, s := sandboxAccountingDB(t)
	terms := readySandboxPayout(t, s)
	ctx := context.Background()
	wrongBeneficiary := payoutCommand("payout_bind", terms, 3, "sbx:substituted")
	wrongBeneficiary.Payout.BeneficiaryID = "sbx:another-approved-beneficiary"
	if _, _, err := s.Apply(ctx, wrongBeneficiary); err == nil {
		t.Fatal("another approved beneficiary substituted the accepted selection")
	}
	if _, err := s.Position(ctx, "sbx:other", terms.TradeID); err == nil {
		t.Fatal("cross-institution position exposed")
	}
	var wg sync.WaitGroup
	results := make(chan SandboxAccountingCommand, 2)
	errors := make(chan error, 2)
	for _, id := range []string{"sbx:payout-a", "sbx:payout-b"} {
		c := payoutCommand("payout_bind", terms, 3, id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Apply(ctx, c)
			errors <- err
			if err == nil {
				results <- c
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	wins := 0
	for err := range errors {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent payout grants=%d", wins)
	}
	winner := <-results
	restarted, err := NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, replay, err := restarted.Apply(ctx, winner); err != nil || !replay {
		t.Fatalf("lost bind receipt replay: %t %v", replay, err)
	}
	position, err := restarted.Position(ctx, terms.InstitutionID, terms.TradeID)
	if err != nil || position.Revision != 4 || position.Payout == nil || position.Payout.SettlementID != winner.Payout.SettlementID || position.PayoutState != "BOUND" {
		t.Fatalf("recovered binding: %+v %v", position, err)
	}
	if sandboxBalance(t, db, "USD", "held") != -100 || sandboxBalance(t, db, "NGN", "fee_revenue") != 0 {
		t.Fatal("binding spent proceeds or recognized a fee")
	}
	bad := payoutCommand("payout_complete", terms, 4, winner.Payout.SettlementID)
	if _, _, err := s.Apply(ctx, bad); err == nil {
		t.Fatal("unsubmitted payout completed")
	}
	submit := payoutCommand("payout_submitted", terms, 4, winner.Payout.SettlementID)
	if _, _, err := s.Apply(ctx, submit); err != nil {
		t.Fatal(err)
	}
	bad = payoutCommand("payout_complete", terms, 5, "sbx:wrong-attempt")
	if _, _, err := s.Apply(ctx, bad); err == nil {
		t.Fatal("different payout spent bound funds")
	}
	unknown := payoutCommand("unknown", terms, 5, winner.Payout.SettlementID)
	if _, _, err := s.Apply(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if sandboxBalance(t, db, "USD", "held") != -100 || sandboxBalance(t, db, "NGN", "fee_revenue") != 0 {
		t.Fatal("UNKNOWN released funds or earned a fee")
	}
	complete := payoutCommand("payout_complete", terms, 6, winner.Payout.SettlementID)
	if _, err := db.Exec(`CREATE FUNCTION reject_payout_fee() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_name='fee_revenue' THEN RAISE EXCEPTION 'injected fee failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_payout_fee BEFORE INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_payout_fee()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(ctx, complete); err == nil {
		t.Fatal("partial payout finality accepted")
	}
	position, err = s.Position(ctx, terms.InstitutionID, terms.TradeID)
	if err != nil || position.PayoutState != "UNKNOWN" || position.Revision != 6 || sandboxBalance(t, db, "USD", "held") != -100 {
		t.Fatal("payout binding escaped journal rollback")
	}
	if _, err := db.Exec(`DROP TRIGGER reject_payout_fee ON ledger_entries; DROP FUNCTION reject_payout_fee()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(ctx, complete); err != nil {
		t.Fatal(err)
	}
	if _, replay, err := s.Apply(ctx, complete); err != nil || !replay {
		t.Fatalf("duplicate finality: %t %v", replay, err)
	}
	if sandboxBalance(t, db, "USD", "held") != 0 || sandboxBalance(t, db, "NGN", "fee_revenue") != -200 {
		t.Fatal("payout and fee economics incorrect")
	}
	returned := payoutCommand("return", terms, 7, winner.Payout.SettlementID)
	if _, _, err := s.Apply(ctx, returned); err != nil {
		t.Fatal(err)
	}
	if sandboxBalance(t, db, "USD", "restricted") != -100 || sandboxBalance(t, db, "NGN", "available") != 0 {
		t.Fatal("return fabricated source-currency refund")
	}
}

func TestSandboxPayoutFailureRequiresControlledRetry(t *testing.T) {
	db, s := sandboxAccountingDB(t)
	terms := readySandboxPayout(t, s)
	ctx := context.Background()
	for i, event := range []string{"payout_bind", "payout_submitted", "payout_failed"} {
		if _, _, err := s.Apply(ctx, payoutCommand(event, terms, int64(3+i), "sbx:first")); err != nil {
			t.Fatal(err)
		}
	}
	if sandboxBalance(t, db, "USD", "restricted") != -100 || sandboxBalance(t, db, "NGN", "fee_revenue") != 0 {
		t.Fatal("failed payout lost proceeds or earned a fee")
	}
	if _, _, err := s.Apply(ctx, payoutCommand("payout_bind", terms, 6, "sbx:retry")); err == nil {
		t.Fatal("new payout authorized before controlled retry")
	}
	retry := sandboxCommand("retry_payout", terms)
	retry.ExpectedRevision = 6
	bad := retry
	bad.Checker = bad.Maker
	bad.Actor = bad.Maker
	if _, _, err := s.Apply(ctx, bad); err == nil {
		t.Fatal("single-actor retry approved")
	}
	if _, _, err := s.Apply(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(ctx, payoutCommand("payout_bind", terms, 7, "sbx:first")); err == nil {
		t.Fatal("reused provider attempt reference")
	}
	if _, _, err := s.Apply(ctx, payoutCommand("payout_bind", terms, 7, "sbx:retry")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(ctx, payoutCommand("payout_complete", terms, 8, "sbx:first")); err == nil {
		t.Fatal("old failed attempt consumed retry funds")
	}
}
