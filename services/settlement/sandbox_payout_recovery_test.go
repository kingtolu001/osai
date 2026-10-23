package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/fxevidence"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpayout"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgergrpc"
	"github.com/osai/osai/services/ledger/ledgerstore"
	"github.com/osai/osai/services/settlement/settlementstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type payoutBeneficiaryApprovalFixture struct{ denied atomic.Bool }

func (v *payoutBeneficiaryApprovalFixture) GetApprovedBeneficiary(_ context.Context, req *customerv1.BeneficiaryRequest, _ ...grpc.CallOption) (*customerv1.Beneficiary, error) {
	if v.denied.Load() {
		return nil, status.Error(codes.FailedPrecondition, "fixture beneficiary disabled")
	}
	return &customerv1.Beneficiary{BeneficiaryId: req.BeneficiaryId, InstitutionId: req.InstitutionId, Status: "ACTIVE", ApprovalStatus: "APPROVED"}, nil
}

type payoutLostFinalReceipt struct {
	ledgerv1.LedgerServiceClient
	mu           sync.Mutex
	lost         bool
	lostReturn   bool
	lostRecovery map[string]bool
}

func (c *payoutLostFinalReceipt) ApplySandboxAccounting(ctx context.Context, req *ledgerv1.SandboxAccountingCommand, options ...grpc.CallOption) (*ledgerv1.SandboxAccountingResponse, error) {
	response, err := c.LedgerServiceClient.ApplySandboxAccounting(ctx, req, options...)
	if err != nil {
		return nil, err
	}
	var command sandboxaccounting.Command
	if err = json.Unmarshal([]byte(req.CommandJson), &command); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if (command.Event == "payout_complete" || command.Event == "payout_failed") && !c.lost {
		c.lost = true
		return nil, status.Error(codes.Unavailable, "fixture lost reply after real Ledger commit")
	}
	if (command.Event == "return" || command.Event == "reversal") && !c.lostReturn {
		c.lostReturn = true
		return nil, status.Error(codes.Unavailable, "fixture lost return reply after real Ledger commit")
	}
	if command.Event == "retry_payout" || command.Event == "release_return" || command.Event == "refund_fee" {
		if c.lostRecovery == nil {
			c.lostRecovery = map[string]bool{}
		}
		if !c.lostRecovery[command.Event] {
			c.lostRecovery[command.Event] = true
			return nil, status.Error(codes.Unavailable, "fixture lost recovery receipt after real Ledger commit")
		}
	}
	return response, nil
}

func TestSandboxPayoutExecutorUnknownAndLostFinalityRecoverWithoutSecondPost(t *testing.T) {
	verifySandboxPayoutExecutorRecovery(t, "SUCCEEDED")
}

func TestSandboxPayoutExecutorFailureAfterFXRestrictsDestinationWithoutEarningFee(t *testing.T) {
	verifySandboxPayoutExecutorRecovery(t, "FAILED")
}

func TestSandboxPayoutReturnAndReversalRecoverWithoutDuplicateRefund(t *testing.T) {
	for _, outcome := range []string{"RETURNED", "REVERSED"} {
		t.Run(outcome, func(t *testing.T) { verifySandboxPayoutExecutorRecovery(t, outcome) })
	}
}

func verifySandboxPayoutExecutorRecovery(t *testing.T, finalStatus string, approvals ...bool) {
	t.Helper()
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accounting, err := ledgerstore.NewSandboxAccounting(ctx, db, "local-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	repository := settlementstore.Postgres{DB: db}
	if err = settlementstore.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if err = repository.EnsureSandboxPayoutSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err = repository.EnsureSandboxRecoverySchema(ctx); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	terms := sandboxaccounting.Terms{TradeID: "trd_" + id, InstitutionID: "sbx:payout-executor:" + id, PrincipalMinor: 9500, FeeMinor: 500, Plan: &sandboxaccounting.FXPlan{QuoteID: "sbx:quote:" + id, Executor: "sbx:executor", AppliedRate: "0.01", DestinationMinor: 95, RateDirection: fxevidence.DestinationPerSource, BeneficiaryID: "sbx:beneficiary", ProviderID: "sbx:provider"}}
	seal := func(c *sandboxaccounting.Command, payload any) {
		raw, _ := json.Marshal(payload)
		hash := sha256.Sum256(raw)
		c.EvidencePayload, c.EvidenceHash = string(raw), hex.EncodeToString(hash[:])
	}
	for revision, event := range []string{"fund", "reserve", "fx_complete"} {
		c := sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, ExpectedRevision: int64(revision), EventID: "sbx:" + id + ":" + event, Event: event, Actor: "sbx:fixture", CorrelationID: "sbx:correlation:" + id, Reason: "explicit local executor fixture", Terms: terms}
		seal(&c, map[string]bool{"fixture": true})
		if event == "fx_complete" {
			c.Execution = &fxevidence.FXExecutionEvidence{TradeID: terms.TradeID, InstitutionID: terms.InstitutionID, QuoteID: terms.Plan.QuoteID, Executor: terms.Plan.Executor, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9500, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95, AppliedRate: "0.01", RateDirection: fxevidence.DestinationPerSource, ExecutionReference: "sbx:execution:" + id, SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
			seal(&c, c.Execution)
		}
		if _, _, err = accounting.Apply(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const token = "local-payout-executor-ledger-token-123456789"
	server := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(server, &ledgergrpc.Server{SandboxToken: token, SandboxAccounting: accounting})
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ledger := &payoutLostFinalReceipt{LedgerServiceClient: ledgerv1.NewLedgerServiceClient(connection)}
	key := "local-payout-executor-fixture-key-123456789"
	var mu sync.Mutex
	posts := 0
	outcome := "UNKNOWN"
	sign := func(raw []byte) string {
		h := hmac.New(sha256.New, []byte(key))
		h.Write(raw)
		return hex.EncodeToString(h.Sum(nil))
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request sandboxpayout.Request
		if json.Unmarshal(raw, &request) != nil || r.Header.Get("X-Osai-Sandbox-Payout-Signature") != sign(raw) {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if request.Operation == "submit" {
			posts++
			w.WriteHeader(503)
			return
		}
		payload, _ := json.Marshal(sandboxpayout.Payload{Instruction: request.Instruction, QueryReference: request.Nonce, Status: outcome, ProviderReference: "sbx:payout-reference:" + request.Instruction.Binding.ClientReference, ObservedAt: time.Now().UTC()})
		_ = json.NewEncoder(w).Encode(sandboxpayout.Envelope{Payload: payload, Signature: sign(payload)})
	}))
	defer fixture.Close()
	for k, v := range map[string]string{"OSAI_SANDBOX_ACCOUNTING_POLICY": sandboxaccounting.Version, "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": fixture.URL + "/v3", "FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": token, "OSAI_POSTGRES_DSN": dsn, "OSAI_TRADE_POSTGRES_DSN": "", "OSAI_SETTLEMENT_POSTGRES_DSN": "", "OSAI_LEDGER_POSTGRES_DSN": "", "OSAI_QUOTE_POSTGRES_DSN": ""} {
		t.Setenv(k, v)
	}
	client, err := sandboxpayout.New(fixture.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := repository.CreateForTrade(ctx, terms.InstitutionID, terms.TradeID, terms.Plan.QuoteID, "sbx:correlation:"+id, terms.Plan.BeneficiaryID, "USD", 95, "fixture payout", "sbx:provider", "", "")
	if err != nil {
		t.Fatal(err)
	}
	approval := &payoutBeneficiaryApprovalFixture{}
	runtime := &sandboxPayoutRuntime{repository: repository, ledger: ledger, client: client, beneficiaries: approval}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = runtime.process(ctx, instruction.ID) }()
	}
	wg.Wait()
	if err = runtime.process(ctx, instruction.ID); err != nil {
		t.Fatal(err)
	}
	position, err := accounting.Position(ctx, terms.InstitutionID, terms.TradeID)
	if err != nil || position.State != "FX_HELD" || position.PayoutState != "SUBMITTED" {
		t.Fatalf("UNKNOWN changed Ledger funds: %+v %v", position, err)
	}
	current, err := repository.GetByID(ctx, instruction.ID)
	if err != nil || current.State != "UNKNOWN" {
		t.Fatalf("ambiguous payout state: %+v %v", current, err)
	}
	mu.Lock()
	postCount := posts
	outcome = finalStatus
	if finalStatus == "RETURNED" || finalStatus == "REVERSED" {
		outcome = "SUCCEEDED"
	}
	mu.Unlock()
	if postCount != 1 {
		t.Fatalf("concurrent ambiguous POST count=%d", postCount)
	}
	if err = runtime.process(ctx, instruction.ID); err == nil || !strings.Contains(err.Error(), "lost reply") {
		t.Fatalf("injected committed reply loss not observed: %v", err)
	}
	wantPosition, wantInstruction, event := "COMPLETE", "CONFIRMED", "payout_complete"
	wantFee, wantRestricted := int64(500), int64(0)
	if finalStatus == "FAILED" {
		wantPosition, wantInstruction, event = "FX_PAYOUT_FAILED", "FAILED", "payout_failed"
		wantFee, wantRestricted = 0, 95
	}
	position, err = accounting.Position(ctx, terms.InstitutionID, terms.TradeID)
	if err != nil || position.State != wantPosition {
		t.Fatalf("real Ledger finality did not commit: %+v %v", position, err)
	}
	// Reconstruct the executor with the same durable stores. Its first action is
	// replay of the frozen final command, rather than another provider POST.
	runtime = &sandboxPayoutRuntime{repository: settlementstore.Postgres{DB: db}, ledger: ledger, client: client, beneficiaries: approval}
	if err = runtime.process(ctx, instruction.ID); err != nil {
		t.Fatal(err)
	}
	current, err = repository.GetByID(ctx, instruction.ID)
	if err != nil || string(current.State) != wantInstruction {
		t.Fatalf("recovered finality: %+v %v", current, err)
	}
	mu.Lock()
	postCount = posts
	mu.Unlock()
	if postCount != 1 {
		t.Fatal("restart sent a duplicate payout")
	}
	var completions, finality int
	if err = db.QueryRow(`SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'=$2`, terms.TradeID, event).Scan(&completions); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM settlement_finality_outbox WHERE settlement_id=$1`, instruction.ID).Scan(&finality); err != nil {
		t.Fatal(err)
	}
	if completions != 1 || finality != 1 {
		t.Fatalf("duplicate finality: journals=%d events=%d", completions, finality)
	}
	var fee, restricted, sourceAvailable int64
	for _, query := range []struct {
		currency, role string
		target         *int64
	}{
		{"NGN", "fee_revenue", &fee}, {"USD", "restricted", &restricted}, {"NGN", "available", &sourceAvailable},
	} {
		if err = db.QueryRow(`SELECT COALESCE(sum(e.credit-e.debit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.currency=$2 AND e.account_name=$3`, terms.TradeID, query.currency, query.role).Scan(query.target); err != nil {
			t.Fatal(err)
		}
	}
	if fee != wantFee || restricted != wantRestricted || sourceAvailable != 0 {
		t.Fatalf("payout recovery economics: fee=%d restricted USD=%d available NGN=%d", fee, restricted, sourceAvailable)
	}
	if finalStatus == "RETURNED" || finalStatus == "REVERSED" {
		mu.Lock()
		outcome = finalStatus
		mu.Unlock()
		if err = runtime.recoverReturn(ctx, instruction.ID); err == nil || !strings.Contains(err.Error(), "lost return reply") {
			t.Fatalf("lost committed return reply not observed: %v", err)
		}
		position, err = accounting.Position(ctx, terms.InstitutionID, terms.TradeID)
		if err != nil || position.State != "RETURNED" {
			t.Fatalf("destination return did not commit: %+v %v", position, err)
		}
		runtime = &sandboxPayoutRuntime{repository: settlementstore.Postgres{DB: db}, ledger: ledger, client: client, beneficiaries: approval}
		for range 2 {
			if err = runtime.recoverReturn(ctx, instruction.ID); err != nil {
				t.Fatal(err)
			}
		}
		current, err = repository.GetByID(ctx, instruction.ID)
		if err != nil || string(current.State) != finalStatus {
			t.Fatalf("returned payout recovery: %+v %v", current, err)
		}
		var returned, sourceRefund, returnEvents int64
		if err = db.QueryRow(`SELECT COALESCE(sum(e.credit-e.debit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.currency='USD' AND e.account_name='restricted'`, terms.TradeID).Scan(&returned); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT COALESCE(sum(e.credit-e.debit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.currency='NGN' AND e.account_name='available'`, terms.TradeID).Scan(&sourceRefund); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM settlement_finality_outbox WHERE settlement_id=$1 AND status=$2`, instruction.ID, finalStatus).Scan(&returnEvents); err != nil {
			t.Fatal(err)
		}
		if returned != 95 || sourceRefund != 0 || returnEvents != 1 {
			t.Fatalf("returned funds or event duplication: USD=%d NGN refund=%d finality=%d", returned, sourceRefund, returnEvents)
		}
		mu.Lock()
		postCount = posts
		mu.Unlock()
		if postCount != 1 {
			t.Fatal("return recovery resubmitted original payout")
		}
	}
	if len(approvals) > 0 && approvals[0] {
		verifyApprovedSandboxRecovery(t, runtime, terms, instruction.ID, finalStatus, func(value string) { mu.Lock(); outcome = value; mu.Unlock() }, func() int { mu.Lock(); defer mu.Unlock(); return posts })
	}
}
