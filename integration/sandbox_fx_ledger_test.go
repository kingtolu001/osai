package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/fxevidence"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type loseFXReceipt struct {
	ledgerv1.LedgerServiceClient
	mu   sync.Mutex
	lost bool
}

type sandboxFXCustomerFixture struct {
	customerv1.UnimplementedCustomerServiceServer
}

func (*sandboxFXCustomerFixture) GetApprovedBeneficiary(_ context.Context, req *customerv1.BeneficiaryRequest) (*customerv1.Beneficiary, error) {
	return &customerv1.Beneficiary{BeneficiaryId: req.BeneficiaryId, InstitutionId: req.InstitutionId, Status: "ACTIVE", ApprovalStatus: "APPROVED"}, nil
}

func (*sandboxFXCustomerFixture) AuthenticateAPIClient(_ context.Context, req *customerv1.CredentialAuthRequest) (*customerv1.AuthContext, error) {
	if req.PublicId != "sbx-public-client" || req.Secret != "local-public-client-fixture" {
		return nil, status.Error(codes.Unauthenticated, "fixture credential rejected")
	}
	return &customerv1.AuthContext{InstitutionId: "sbx:public", InstitutionStatus: "ACTIVE", ApiEnabled: true, Environment: "sandbox", Entitlements: []string{"quotes:read", "quotes:write", "trades:read"}}, nil
}

func (*sandboxFXCustomerFixture) GetInstitutionAccess(_ context.Context, req *customerv1.InstitutionRequest) (*customerv1.AuthContext, error) {
	if req.InstitutionId != "sbx:public" {
		return nil, status.Error(codes.PermissionDenied, "fixture institution rejected")
	}
	return &customerv1.AuthContext{InstitutionId: req.InstitutionId, InstitutionStatus: "ACTIVE", ApiEnabled: true, Environment: "sandbox", Entitlements: []string{"quotes:read", "quotes:write", "trades:read"}}, nil
}

func (c *loseFXReceipt) ApplySandboxAccounting(ctx context.Context, req *ledgerv1.SandboxAccountingCommand, options ...grpc.CallOption) (*ledgerv1.SandboxAccountingResponse, error) {
	response, err := c.LedgerServiceClient.ApplySandboxAccounting(ctx, req, options...)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lost {
		c.lost = true
		return nil, status.Error(codes.Unavailable, "injected lost response after commit")
	}
	return response, nil
}

// This proves Trade's durable relay against an actual Ledger subprocess. The
// FX endpoint is a signed loopback fixture, not a real FX/custody provider.
func TestSandboxFXLedgerSeparateProcessRecovery(t *testing.T) {
	if os.Getenv("OSAI_SANDBOX_FX_SERVICES_VERIFY") != "true" {
		t.Skip("explicit subprocess sandbox FX verification required")
	}
	adminDSN := os.Getenv("OSAI_POSTGRES_DSN")
	if !sandboxpolicy.LocalProofDSN(adminDSN) {
		t.Fatal("local proof PostgreSQL URL required")
	}
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatal(err)
	}
	database := "osai_proof_fx_" + randomHex(t, 6)
	if _, err = admin.Exec(`CREATE DATABASE ` + database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP DATABASE IF EXISTS ` + database + ` WITH (FORCE)`) })
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	dsn := u.String()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := tradecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	key, token := "local-"+randomHex(t, 32), randomHex(t, 32)
	var mu sync.Mutex
	posts := 0
	mac := func(raw []byte) string {
		h := hmac.New(sha256.New, []byte(key))
		h.Write(raw)
		return hex.EncodeToString(h.Sum(nil))
	}
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !hmac.Equal([]byte(r.Header.Get("X-Osai-Sandbox-FX-Signature")), []byte(mac(raw))) {
			w.WriteHeader(401)
			return
		}
		var request tradecore.SandboxFXRequest
		if json.Unmarshal(raw, &request) != nil {
			w.WriteHeader(400)
			return
		}
		if request.Operation == "submit" {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(503)
			return
		}
		if request.Operation != "query" {
			w.WriteHeader(400)
			return
		}
		e := request.Execution
		evidence := &fxevidence.FXExecutionEvidence{TradeID: e.TradeID, InstitutionID: e.InstitutionID, QuoteID: e.QuoteID, Executor: e.ExecutorID, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9500, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95, AppliedRate: "0.01", RateDirection: fxevidence.DestinationPerSource, ExecutionReference: "sbx:execution:" + e.ID, SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
		payload, _ := json.Marshal(tradecore.SandboxFXPayload{Status: tradecore.FXQueryExecuted, ExecutorID: e.ExecutorID, RequestReference: e.RequestReference, RequestHash: e.RequestHash, PricingSnapshotHash: e.PricingSnapshotHash, RecoveryGeneration: e.RecoveryGeneration, ProviderRef: evidence.ExecutionReference, QueryReference: request.Nonce, ObservedAt: time.Now().UTC(), Execution: evidence})
		json.NewEncoder(w).Encode(tradecore.SandboxFXEnvelope{Payload: payload, Signature: mac(payload)})
	}))
	t.Cleanup(fx.Close)
	address := loopbackPort(t)
	env := map[string]string{"OSAI_POSTGRES_DSN": dsn, "OSAI_SANDBOX_ACCOUNTING_POLICY": sandboxaccounting.Version, "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": fx.URL + "/v3", "FLW_SECRET_KEY": "local-fixture-only", "OSAI_SANDBOX_LEDGER_TOKEN": token, "OSAI_LEDGER_GRPC_ADDR": address, "OSAI_CUSTODY_BANK_ID": "sbx:bank", "OSAI_TRADE_POSTGRES_DSN": "", "OSAI_SETTLEMENT_POSTGRES_DSN": "", "OSAI_LEDGER_POSTGRES_DSN": ""}
	for k, v := range env {
		t.Setenv(k, v)
	}
	env["OSAI_SANDBOX_FX_ACCEPTANCE"] = ""
	t.Setenv("OSAI_SANDBOX_FX_ACCEPTANCE", "")
	runtime := newProofRuntime(t, env)
	runtime.start("ledger", "./cmd/ledger-server", address)
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ledger := ledgerv1.NewLedgerServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	snapshot := pricing.Snapshot{Mode: "FIXED_SOURCE", RequestedAmountMinor: 10000, SourcePrincipalMinor: 9500, FeeMinor: 500, TotalDebitMinor: 10000, DestinationAmountMinor: 95, SourceCurrency: "NGN", DestinationCurrency: "USD", RateScaled: 1000000, RateDirection: "USD_PER_NGN", RateScale: pricing.RateScale, SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "sbx:fee", FeeScheduleVersion: 1, FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500, MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1000000, MinimumDebitMinor: 1, MaximumDebitMinor: 2000000, MinimumDestinationMinor: 1, MaximumDestinationMinor: 2000000, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	snapshot.Hash, err = pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	trade, err := store.CreateTradeWithPricing("sbx:institution", "sbx:quote", "sbx:accept", "sbx:correlation", 9500, "NGN", "USD", snapshot, "sbx:beneficiary", "sbx:provider", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	terms, err := store.SandboxFXTerms(trade.ID, "sbx:executor")
	if err != nil {
		t.Fatal(err)
	}
	var payoutBinding *sandboxaccounting.PayoutBinding
	apply := func(event string, revision int64) {
		t.Helper()
		payload := `{"fixture":true}`
		hash := sha256.Sum256([]byte(payload))
		command := sandboxaccounting.Command{ExpectedRevision: revision, PolicyVersion: sandboxaccounting.Version, EventID: fmt.Sprintf("sbx:%s:%d", event, revision), Event: event, Actor: "sbx:checker", Maker: "sbx:maker", Checker: "sbx:checker", CorrelationID: trade.CorrelationID, Reason: "explicit test fixture", EvidencePayload: payload, EvidenceHash: hex.EncodeToString(hash[:]), Terms: terms}
		switch event {
		case "payout_bind", "payout_submitted", "payout_failed", "unknown", "payout_complete", "return":
			command.Payout = payoutBinding
		}
		raw, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = ledger.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	apply("fund", 0)
	apply("reserve", 1)
	if err = store.SetStatus(trade.ID, tradecore.StateFunded); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateFXExecution(trade.ID, "sbx:intent", "sbx:executor"); err != nil {
		t.Fatal(err)
	}
	if err = store.EnqueueSandboxFXPosting(ctx, trade.ID, 2); err == nil {
		t.Fatal("unexecuted FX queued for accounting")
	}
	client, err := tradecore.NewSandboxFXClient(fx.URL, "sbx:executor", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SubmitFXExecution(ctx, trade.ID, client, client); err == nil {
		t.Fatal("injected submit timeout not reported")
	}
	store, err = tradecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecoverFXExecution(ctx, trade.ID, client, client); err != nil {
		t.Fatal(err)
	}
	store, err = tradecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	completed, found, err := store.GetFXExecution(trade.ID)
	if err != nil || !found || len(completed.VerifiedResponse) == 0 {
		t.Fatalf("authenticated response not durable after restart: %v", err)
	}
	original, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	completed.VerifiedResponse[0] ^= 1
	corrupted, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE trade_fx_executions SET body=$2::jsonb WHERE trade_id=$1`, trade.ID, string(corrupted)); err != nil {
		t.Fatal(err)
	}
	if err = store.EnqueueSandboxFXPosting(ctx, trade.ID, 2); err == nil {
		t.Fatal("corrupted durable response queued for Ledger")
	}
	if _, err = db.Exec(`UPDATE trade_fx_executions SET body=$2::jsonb WHERE trade_id=$1`, trade.ID, string(original)); err != nil {
		t.Fatal(err)
	}
	if err = store.EnqueueSandboxFXPosting(ctx, trade.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err = store.EnqueueSandboxFXPosting(ctx, trade.ID, 3); err == nil {
		t.Fatal("queued command revision mutated")
	}
	lost := &loseFXReceipt{LedgerServiceClient: ledger}
	if n, err := store.RelaySandboxFXPostings(ctx, lost, 25); err == nil || n != 0 {
		t.Fatalf("lost receipt: %d %v", n, err)
	}
	if proofCount(db, `SELECT count(*) FROM trade_sandbox_fx_postings WHERE posted_at IS NOT NULL`) != 0 {
		t.Fatal("ambiguous receipt acknowledged")
	}
	runtime.stop("ledger")
	runtime.start("ledger", "./cmd/ledger-server", address)
	store, err = tradecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			defer stop()
			_, err := store.RelaySandboxFXPostings(callCtx, ledger, 25)
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if proofCount(db, `SELECT count(*) FROM trade_sandbox_fx_postings WHERE posted_at IS NOT NULL`) != 1 || proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE command_json::jsonb->>'Event'='fx_complete'`) != 1 || proofCount(db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1`, trade.ID) != 4 {
		t.Fatal("FX posting duplicated or receipt missing")
	}
	// Simulate loss of the local acknowledgement and prove the real Trade
	// background process recovers the immutable command, without a test caller
	// invoking RelaySandboxFXPostings on its behalf.
	if _, err := db.Exec(`UPDATE trade_sandbox_fx_postings SET posted_at=NULL,receipt_json=NULL`); err != nil {
		t.Fatal(err)
	}
	tradeAddress := loopbackPort(t)
	for k, v := range map[string]string{
		"OSAI_SANDBOX_FX_LEDGER_POSTING": "true", "OSAI_TRADE_GRPC_ADDR": tradeAddress,
		"OSAI_TRADE_SETTLEMENT_TOKEN": randomHex(t, 32), "OSAI_NOTIFICATION_TOKEN": randomHex(t, 32),
		"OSAI_SETTLEMENT_GRPC_ADDR": loopbackPort(t), "OSAI_NOTIFICATION_GRPC_ADDR": loopbackPort(t), "OSAI_CUSTOMER_GRPC_ADDR": loopbackPort(t),
		"OSAI_SANDBOX_FX_BASE_URL": "", "OSAI_SANDBOX_FX_SIGNING_KEY": "", "OSAI_SANDBOX_FX_EXECUTOR_ID": "",
	} {
		runtime.env[k] = v
	}
	runtime.start("trade", "./services/trade-orchestrator", tradeAddress)
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM trade_sandbox_fx_postings WHERE posted_at IS NOT NULL`) == 1
	})
	runtime.stop("trade")
	runtime.start("trade", "./services/trade-orchestrator", tradeAddress)
	if proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE command_json::jsonb->>'Event'='fx_complete'`) != 1 || proofCount(db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1`, trade.ID) != 4 {
		t.Fatal("Trade background replay duplicated Ledger posting")
	}
	for i, event := range []string{"payout_bind", "payout_submitted", "payout_failed", "retry_payout", "payout_bind", "payout_submitted", "unknown", "payout_complete", "return", "release_return", "refund_fee"} {
		if event == "payout_bind" {
			payoutBinding = &sandboxaccounting.PayoutBinding{SettlementID: fmt.Sprintf("sbx:payout:%d", i), ClientReference: fmt.Sprintf("sbx:payout-ref:%d", i), ProviderID: "sbx:provider", BeneficiaryID: "sbx:beneficiary", Currency: "USD", AmountMinor: 95}
		}
		apply(event, int64(i+3))
	}
	var ngnHeld, usdAvailable int64
	if err := db.QueryRow(`SELECT COALESCE(sum(credit-debit),0) FROM ledger_entries WHERE currency='NGN' AND account_name='held'`).Scan(&ngnHeld); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COALESCE(sum(credit-debit),0) FROM ledger_entries WHERE currency='USD' AND account_name='available'`).Scan(&usdAvailable); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	postCount := posts
	mu.Unlock()
	if ngnHeld != 0 || usdAvailable != 95 || postCount != 1 {
		t.Fatalf("recovery balances: NGN held=%d USD available=%d FX submissions=%d", ngnHeld, usdAvailable, postCount)
	}
	// Drive the source acceptance API in the actual Trade process. A separate
	// actual Customer service supplies durable authenticated test approval. No bank
	// details or real custody/partner approval are implied.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	customerServer := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(customerServer, &sandboxFXCustomerFixture{})
	go customerServer.Serve(listener)
	t.Cleanup(func() { customerServer.Stop(); listener.Close() })
	actualCustomerAddress, approvedBeneficiaryID := startApprovedSandboxCustomer(t, runtime)
	runtime.stop("trade")
	for k, v := range map[string]string{"OSAI_CUSTOMER_GRPC_ADDR": actualCustomerAddress, "OSAI_SANDBOX_FX_ACCEPTANCE": "true", "OSAI_SANDBOX_FX_BASE_URL": fx.URL, "OSAI_SANDBOX_FX_SIGNING_KEY": key, "OSAI_SANDBOX_FX_EXECUTOR_ID": "sbx:executor"} {
		runtime.env[k] = v
	}
	runtime.start("trade", "./services/trade-orchestrator", tradeAddress)
	tradeConn, err := grpc.NewClient(tradeAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tradeConn.Close() })
	tradeClient := tradev1.NewTradeServiceClient(tradeConn)
	protoSnapshot := &tradev1.PricingSnapshot{QuoteMode: snapshot.Mode, RequestedAmountMinor: snapshot.RequestedAmountMinor, SourcePrincipalMinor: snapshot.SourcePrincipalMinor, FeeMinor: snapshot.FeeMinor, TotalDebitMinor: snapshot.TotalDebitMinor, DestinationAmountMinor: snapshot.DestinationAmountMinor, SourceCurrency: snapshot.SourceCurrency, DestinationCurrency: snapshot.DestinationCurrency, RateScaled: snapshot.RateScaled, RateDirection: snapshot.RateDirection, RateScale: snapshot.RateScale, FeeScheduleId: snapshot.FeeScheduleID, FeeScheduleVersion: snapshot.FeeScheduleVersion, SourceMinorExponent: snapshot.SourceExponent, DestinationMinorExponent: snapshot.DestinationExponent, FixedFeeMinor: snapshot.FixedFeeMinor, MinimumFeeMinor: snapshot.MinimumFeeMinor, MaximumFeeMinor: snapshot.MaximumFeeMinor, MinimumSourcePrincipalMinor: snapshot.MinimumPrincipalMinor, MaximumSourcePrincipalMinor: snapshot.MaximumPrincipalMinor, MinimumTotalDebitMinor: snapshot.MinimumDebitMinor, MaximumTotalDebitMinor: snapshot.MaximumDebitMinor, MinimumDestinationMinor: snapshot.MinimumDestinationMinor, MaximumDestinationMinor: snapshot.MaximumDestinationMinor, ExpiresAt: snapshot.ExpiresAt, SnapshotHash: snapshot.Hash}
	request := &tradev1.CreateTradeFromAcceptedQuoteRequest{InstitutionId: "sbx:institution", QuoteId: "quo_0123456789ab", IdempotencyKey: "sbx:api-accept", CorrelationId: "sbx:api-correlation", BaseAmountMinor: 9500, BaseCurrency: "NGN", QuoteCurrency: "USD", Amount: &tradev1.Money{AmountMinor: 95, Currency: "USD"}, BeneficiaryId: approvedBeneficiaryID, SettlementProviderId: "sbx:provider", PricingSnapshot: protoSnapshot}
	accepted, err := tradeClient.CreateTradeFromAcceptedQuote(ctx, request)
	if err != nil || accepted.Status != string(tradecore.StateFundingRequired) {
		t.Fatalf("unfunded acceptance: %v %v", accepted, err)
	}
	if proofCount(db, `SELECT count(*) FROM trade_fx_executions WHERE trade_id=$1`, accepted.TradeId) != 0 {
		t.Fatal("unfunded trade created an FX execution")
	}
	depositTerms := sandboxaccounting.Terms{TradeID: "sbx:explicit-api-deposit", InstitutionID: "sbx:institution", PrincipalMinor: 10000}
	depositPayload := `{"fixture_deposit":true}`
	depositHash := sha256.Sum256([]byte(depositPayload))
	depositRaw, err := json.Marshal(sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, EventID: "sbx:explicit-api-deposit", Event: "fund", Actor: "sbx:test-funder", CorrelationID: "sbx:deposit", Reason: "explicit fixture prefund", EvidencePayload: depositPayload, EvidenceHash: hex.EncodeToString(depositHash[:]), Terms: depositTerms})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: string(depositRaw)}); err != nil {
		t.Fatal(err)
	}
	replayed, err := tradeClient.CreateTradeFromAcceptedQuote(ctx, request)
	if err != nil || replayed.TradeId != accepted.TradeId {
		t.Fatalf("funded API replay: %v %v", replayed, err)
	}
	waitFor(t, 25*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM trade_sandbox_fx_postings p JOIN trade_fx_executions e ON e.execution_id=p.execution_id WHERE e.trade_id=$1 AND p.posted_at IS NOT NULL`, accepted.TradeId) == 1
	})
	if proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'='reserve'`, accepted.TradeId) != 1 || proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'='fx_complete'`, accepted.TradeId) != 1 {
		t.Fatal("API acceptance duplicated hold or FX posting")
	}
	if proofCount(db, `SELECT count(*) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.account_name='fee_revenue'`, accepted.TradeId) != 0 {
		t.Fatal("fee earned before destination payout")
	}
	final, err := tradeClient.GetTrade(ctx, &tradev1.GetTradeRequest{InstitutionId: "sbx:institution", TradeId: accepted.TradeId})
	if err != nil || final.Status != string(tradecore.StateFXExecuted) || final.SettlementId != "" {
		t.Fatalf("unverified destination payout inferred: %v %v", final, err)
	}
	mu.Lock()
	postCount = posts
	mu.Unlock()
	if postCount != 2 {
		t.Fatalf("FX submissions=%d; want one per funded trade", postCount)
	}
	t.Log("signed FX timeout/recovery -> durable queue -> authenticated Ledger subprocess; lost commit response, restart, four concurrent relays and actual Trade background subprocess replay produced one FX event; payout recovery/return/fee refund retained USD principal")
	t.Log("actual Trade acceptance API: insufficient prefund refused, explicit fixture deposit reserved once, one FX submit recovered by query and posted through background workers; USD payout and fee earning remained gated")
	verifySandboxFXPayoutRuntime(t, runtime, db, ledger, tradeClient, tradeAddress, actualCustomerAddress, accepted.TradeId)
	runtime.stop("actual-customer")
	verifyPublicFXAcceptanceRecovery(t, runtime, db, ledger, tradeAddress, listener.Addr().String())
}
