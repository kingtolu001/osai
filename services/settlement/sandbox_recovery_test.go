package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAuthenticatedSandboxRecoveryPreservesDistinctAttemptsAndReturnedFunds(t *testing.T) {
	for _, outcome := range []string{"FAILED", "RETURNED", "REVERSED"} {
		t.Run(outcome, func(t *testing.T) { verifySandboxPayoutExecutorRecovery(t, outcome, true) })
	}
}

func verifyApprovedSandboxRecovery(t *testing.T, r *sandboxPayoutRuntime, terms sandboxaccounting.Terms, settlementID, outcome string, setOutcome func(string), posts func() int) {
	t.Helper()
	ctx := context.Background()
	makerToken, checkerToken := strings.Repeat("m", 40), strings.Repeat("c", 40)
	raw, _ := json.Marshal([]recoveryOperator{{ActorID: "sbx:maker", Token: makerToken, Role: "maker", Institutions: []string{terms.InstitutionID}}, {ActorID: "sbx:checker", Token: checkerToken, Role: "checker", Institutions: []string{terms.InstitutionID}}})
	t.Setenv("OSAI_SANDBOX_RECOVERY_OPERATORS_JSON", string(raw))
	operators, err := loadRecoveryOperators()
	if err != nil {
		t.Fatal(err)
	}
	handler := r.recoveryHandler(operators)
	call := func(path, token string, input recoveryRequest) int {
		body, _ := json.Marshal(input)
		req := httptest.NewRequest("POST", "/v1/operator/sandbox-recovery/"+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Code
	}
	actions := []string{"release_return", "refund_fee"}
	if outcome == "FAILED" {
		actions = []string{"retry_payout"}
	}
	for _, action := range actions {
		input := recoveryRequest{ID: terms.TradeID + ":" + action, InstitutionID: terms.InstitutionID, SettlementID: settlementID, Action: action, Reason: "explicit sandbox operator fixture approval"}
		if code := call("propose", "wrong", input); code != 401 {
			t.Fatalf("unauthenticated proposal=%d", code)
		}
		if code := call("propose", checkerToken, input); code != 401 {
			t.Fatalf("wrong maker role=%d", code)
		}
		foreign := input
		foreign.InstitutionID = "sbx:foreign"
		if code := call("propose", makerToken, foreign); code != 403 {
			t.Fatalf("cross-institution proposal=%d", code)
		}
		if code := call("approve", checkerToken, input); code != 409 {
			t.Fatalf("approval without proposal=%d", code)
		}
		if code := call("propose", makerToken, input); code != 200 {
			t.Fatalf("proposal=%d", code)
		}
		changed := input
		changed.Reason = "substituted reason"
		if code := call("propose", makerToken, changed); code != 409 {
			t.Fatalf("changed proposal=%d", code)
		}
		if code := call("approve", makerToken, input); code != 401 {
			t.Fatalf("self approval=%d", code)
		}
		if code := call("approve", checkerToken, input); code != 200 {
			t.Fatalf("checker approval=%d", code)
		}
		if action == "retry_payout" {
			approval := r.beneficiaries.(*payoutBeneficiaryApprovalFixture)
			approval.denied.Store(true)
			if err = r.recoverApproved(ctx); err == nil {
				t.Fatal("disabled beneficiary approved a new retry")
			}
			position, e := r.position(ctx, terms.InstitutionID, terms.TradeID)
			if e != nil || position.State != "FX_PAYOUT_FAILED" || posts() != 1 {
				t.Fatal("beneficiary revocation moved funds or sent another payout", e)
			}
			approval.denied.Store(false)
		}
		if err = r.recoverApproved(ctx); err == nil {
			t.Fatal("committed recovery reply loss not exercised")
		}
		// Reconstruct the runtime after the Ledger commit but before local acknowledgement.
		restarted := &sandboxPayoutRuntime{repository: r.repository, ledger: r.ledger, client: r.client, beneficiaries: r.beneficiaries}
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if e := restarted.recoverApproved(ctx); e != nil {
					t.Error(e)
				}
			}()
		}
		wg.Wait()
		if code := call("propose", makerToken, input); code != 200 {
			t.Fatalf("proposal replay after apply=%d", code)
		}
		if code := call("approve", checkerToken, input); code != 200 {
			t.Fatalf("approval replay after apply=%d", code)
		}
		v, e := r.repository.SandboxRecovery(ctx, recoveryID(terms.InstitutionID, input.ID), terms.InstitutionID)
		if e != nil || v.State != "APPLIED" {
			t.Fatalf("recovered action state=%s error=%v", v.State, e)
		}
		if action == "retry_payout" {
			if v.NextSettlementID == "" || v.NextSettlementID == settlementID {
				t.Fatal("retry reused failed attempt")
			}
			next, e := r.repository.GetByID(ctx, v.NextSettlementID)
			if e != nil || next.Supersedes != settlementID {
				t.Fatal("retry lost original attempt", e)
			}
			old, e := r.repository.GetByID(ctx, settlementID)
			if e != nil || old.State != "FAILED" || old.ClientRef == next.ClientRef {
				t.Fatal("failed attempt history replaced", e)
			}
			setOutcome("SUCCEEDED")
			approval := r.beneficiaries.(*payoutBeneficiaryApprovalFixture)
			approval.denied.Store(true)
			if e = r.process(ctx, next.ID); e == nil {
				t.Fatal("revoked beneficiary reached retry dispatch")
			}
			job, e := r.repository.SandboxPayoutJob(ctx, next.ID)
			if e != nil || job.DispatchStarted || posts() != 1 {
				t.Fatal("revoked beneficiary consumed dispatch fence or submitted payout", e)
			}
			approval.denied.Store(false)
			if e = r.process(ctx, next.ID); e != nil {
				t.Fatal(e)
			}
			if posts() != 2 {
				t.Fatalf("retry POST count=%d", posts())
			}
		}
	}
	p, err := r.position(ctx, terms.InstitutionID, terms.TradeID)
	if err != nil {
		t.Fatal(err)
	}
	want := "REFUNDED"
	if outcome == "FAILED" {
		want = "COMPLETE"
	}
	if p.State != want {
		t.Fatalf("recovery position=%s expected=%s", p.State, want)
	}
	var sourceAvailable, destinationAvailable int64
	for _, x := range []struct {
		currency string
		target   *int64
	}{{"NGN", &sourceAvailable}, {"USD", &destinationAvailable}} {
		if err = r.repository.DB.QueryRow(`SELECT COALESCE(sum(e.credit-e.debit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.account_name='available' AND e.currency=$2`, terms.TradeID, x.currency).Scan(x.target); err != nil {
			t.Fatal(err)
		}
	}
	if outcome == "FAILED" {
		if sourceAvailable != 0 || destinationAvailable != 0 {
			t.Fatal("retry invented available balance")
		}
	} else if sourceAvailable != terms.FeeMinor || destinationAvailable != terms.Plan.DestinationMinor || posts() != 1 {
		t.Fatalf("return release/refund economics NGN=%d USD=%d POSTs=%d", sourceAvailable, destinationAvailable, posts())
	}
}
