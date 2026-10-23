package integration

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
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpayout"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/notification-webhook/webhookcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type payoutCommitReplyProxy struct {
	ledgerv1.UnimplementedLedgerServiceServer
	client       ledgerv1.LedgerServiceClient
	mu           sync.Mutex
	lost         bool
	lostComplete bool
}

func (p *payoutCommitReplyProxy) GetSandboxPosition(ctx context.Context, req *ledgerv1.SandboxPositionRequest) (*ledgerv1.SandboxPositionResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	return p.client.GetSandboxPosition(metadata.NewOutgoingContext(ctx, md.Copy()), req)
}

func (p *payoutCommitReplyProxy) ApplySandboxAccounting(ctx context.Context, req *ledgerv1.SandboxAccountingCommand) (*ledgerv1.SandboxAccountingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	response, err := p.client.ApplySandboxAccounting(metadata.NewOutgoingContext(ctx, md.Copy()), req)
	if err != nil {
		return nil, err
	}
	var command sandboxaccounting.Command
	if err = json.Unmarshal([]byte(req.CommandJson), &command); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if command.Event == "payout_failed" && !p.lost {
		p.lost = true
		return nil, status.Error(codes.Unavailable, "injected lost payout commit reply")
	}

	if command.Event == "payout_complete" && !p.lostComplete {
		p.lostComplete = true
		p.lost = true
		return nil, status.Error(codes.Unavailable, "injected lost successful payout commit reply")
	}
	return response, nil
}

func verifySandboxFXPayoutRuntime(t *testing.T, runtime *proofRuntime, db *sql.DB, ledger ledgerv1.LedgerServiceClient, tradeClient tradev1.TradeServiceClient, tradeAddress, customerAddress, tradeID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	original := map[string]string{}
	for k, v := range runtime.env {
		original[k] = v
	}
	defer func() {
		runtime.stop("settlement-peer")
		runtime.stop("settlement")
		runtime.stop("payout-notification")
		runtime.stop("trade")
		runtime.env = original
		runtime.start("trade", "./services/trade-orchestrator", tradeAddress)
	}()
	metricAddress, periodicMetricsVerified := startPayoutMetricForwarder(t)
	runtime.env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://" + metricAddress
	makerToken, checkerToken := randomHex(t, 32), randomHex(t, 32)
	operators, _ := json.Marshal([]map[string]any{{"actor_id": "sbx:recovery-maker", "token": makerToken, "role": "maker", "institutions": []string{"sbx:institution"}}, {"actor_id": "sbx:recovery-checker", "token": checkerToken, "role": "checker", "institutions": []string{"sbx:institution"}}})
	runtime.env["OSAI_SANDBOX_RECOVERY_OPERATORS_JSON"] = string(operators)
	key := "local-" + randomHex(t, 32)
	var mu sync.Mutex
	posts := 0
	outcome := "UNKNOWN"
	fixtureMAC := func(raw []byte) string {
		h := hmac.New(sha256.New, []byte(key))
		h.Write(raw)
		return hex.EncodeToString(h.Sum(nil))
	}
	payout := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var request sandboxpayout.Request
		if json.Unmarshal(raw, &request) != nil || !hmac.Equal([]byte(r.Header.Get("X-Osai-Sandbox-Payout-Signature")), []byte(fixtureMAC(raw))) {
			w.WriteHeader(401)
			return
		}
		if request.Instruction.TradeID != tradeID || request.Instruction.Binding.AmountMinor != 95 || request.Instruction.Binding.Currency != "USD" {
			t.Error("payout substituted source funds or another trade")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if request.Operation == "submit" {
			posts++
			w.WriteHeader(503)
			return
		}
		if request.Operation != "query" {
			w.WriteHeader(400)
			return
		}
		payload, _ := json.Marshal(sandboxpayout.Payload{Instruction: request.Instruction, QueryReference: request.Nonce, Status: outcome, ProviderReference: "sbx:payout:" + request.Instruction.Binding.ClientReference, ObservedAt: time.Now().UTC()})
		_ = json.NewEncoder(w).Encode(sandboxpayout.Envelope{Payload: payload, Signature: fixtureMAC(payload)})
	}))
	defer payout.Close()
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &payoutCommitReplyProxy{client: ledger}
	proxyServer := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(proxyServer, proxy)
	go proxyServer.Serve(proxyListener)
	defer proxyServer.Stop()
	defer proxyListener.Close()
	notifyAddress, webhookAddress := loopbackPort(t), loopbackPort(t)
	settlementAddress, settlementHTTP := loopbackPort(t), loopbackPort(t)
	for k, v := range map[string]string{
		"OSAI_SANDBOX_FX_PAYOUT": "true", "OSAI_SANDBOX_PAYOUT_BASE_URL": payout.URL, "OSAI_SANDBOX_PAYOUT_SIGNING_KEY": key,
		"OSAI_LEDGER_GRPC_ADDR": proxyListener.Addr().String(), "OSAI_CUSTOMER_GRPC_ADDR": customerAddress,
		"OSAI_SETTLEMENT_GRPC_ADDR": settlementAddress, "OSAI_SETTLEMENT_PORT": settlementHTTP, "FLW_SETTLEMENT_ENABLED": "",
		"OSAI_NOTIFICATION_GRPC_ADDR": notifyAddress, "OSAI_WEBHOOK_PORT": webhookAddress,
		"OSAI_WEBHOOK_CONFIG_ENCRYPTION_KEY": randomHex(t, 32), "OSAI_WEBHOOK_ADMIN_TOKEN": randomHex(t, 32),
		"OSAI_WEBHOOK_RETRY_DELAY": "100ms", "OSAI_WEBHOOK_MAX_ATTEMPTS": "4",
		"OSAI_SANDBOX_PAYOUT_UNKNOWN_ALERT_AFTER": "1s",
	} {
		runtime.env[k] = v
	}
	runtime.start("payout-notification", "./services/notification-webhook", notifyAddress)
	notifyConn, err := grpc.NewClient(notifyAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer notifyConn.Close()
	secret := randomHex(t, 32)
	var deliveredMu sync.Mutex
	delivered := map[string]int{}
	badDelivery := false
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		valid, verifyErr := webhookcore.VerifyCustomerPayload(secret, r.Header.Get("X-Osai-Event-Id"), r.Header.Get("X-Osai-Timestamp"), raw, r.Header.Get("X-Osai-Signature"))
		var event struct {
			EventID   string            `json:"event_id"`
			EventType string            `json:"event_type"`
			Payload   map[string]string `json:"payload"`
		}
		decodeErr := json.Unmarshal(raw, &event)
		deliveredMu.Lock()
		defer deliveredMu.Unlock()
		wantStatus := map[string]string{"trade.settled": "SETTLED", "trade.failed": "FAILED", "trade.returned": "RETURNED", "si.unknown_escalated": "UNKNOWN"}[event.EventType]
		if verifyErr != nil || !valid || decodeErr != nil || event.EventID != r.Header.Get("X-Osai-Event-Id") || event.EventType != r.Header.Get("X-Osai-Event-Type") || wantStatus == "" || event.Payload["status"] != wantStatus || event.Payload["trade_id"] != tradeID {
			badDelivery = true
		}
		delivered[event.EventType]++
		if delivered[event.EventType] == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer endpoint.Close()
	_, err = notificationv1.NewNotificationServiceClient(notifyConn).ConfigureWebhook(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+runtime.env["OSAI_WEBHOOK_ADMIN_TOKEN"]), &notificationv1.ConfigureWebhookRequest{InstitutionId: "sbx:institution", EndpointUrl: endpoint.URL, SigningSecret: secret, SubscribedEventTypes: []string{"trade.settled", "trade.failed", "trade.returned", "si.unknown_escalated"}})
	if err != nil {
		t.Fatal(err)
	}
	runtime.start("settlement", "./services/settlement", settlementAddress)
	// Two actual Settlement processes share the dispatch fence, not an in-memory
	// lock. The external fixture outlives their restarts and remembers the POST.
	runtime.env["OSAI_SETTLEMENT_GRPC_ADDR"], runtime.env["OSAI_SETTLEMENT_PORT"] = loopbackPort(t), loopbackPort(t)
	runtime.start("settlement-peer", "./services/settlement", runtime.env["OSAI_SETTLEMENT_GRPC_ADDR"])
	runtime.env["OSAI_SETTLEMENT_GRPC_ADDR"], runtime.env["OSAI_SETTLEMENT_PORT"] = settlementAddress, settlementHTTP
	runtime.stop("trade")
	runtime.start("trade", "./services/trade-orchestrator", tradeAddress)
	waitFor(t, 30*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return posts == 1 && proofCount(db, `SELECT count(*) FROM settlement_instructions WHERE trade_id=$1 AND state='UNKNOWN'`, tradeID) == 1
	})
	waitFor(t, 20*time.Second, func() bool {
		deliveredMu.Lock()
		defer deliveredMu.Unlock()
		return delivered["si.unknown_escalated"] >= 2
	})
	if proofCount(db, `SELECT count(*) FROM settlement_domain_outbox WHERE trade_id=$1 AND event_type='si.unknown_escalated'`, tradeID) != 1 || proofCount(db, `SELECT count(*) FROM ledger_sandbox_positions WHERE trade_id=$1 AND state='FX_HELD'`, tradeID) != 1 {
		t.Fatal("UNKNOWN alert duplicated or changed financial finality")
	}
	waitFor(t, 25*time.Second, periodicMetricsVerified)
	t.Log("PERIODIC_PAYOUT_METRICS actual Settlement process exported UNKNOWN, unresolved and age gauges through an observing forwarder; existing OTLP collector accepted the export")
	if proofCount(db, `SELECT count(*) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.account_name='fee_revenue'`, tradeID) != 0 {
		t.Fatal("payout acknowledgement earned fee before success")
	}
	runtime.stop("settlement-peer")
	runtime.stop("settlement")
	mu.Lock()
	outcome = "FAILED"
	mu.Unlock()
	runtime.start("settlement", "./services/settlement", settlementAddress)
	waitFor(t, 20*time.Second, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return proxy.lost })
	runtime.stop("settlement")
	// Reconstruct Ledger and Settlement after the actual final journal commit,
	// then recover the frozen signed observation and identical command.
	runtime.stop("ledger")
	ledgerAddress := original["OSAI_LEDGER_GRPC_ADDR"]
	runtime.env["OSAI_LEDGER_GRPC_ADDR"] = ledgerAddress
	runtime.start("ledger", "./cmd/ledger-server", ledgerAddress)
	runtime.env["OSAI_LEDGER_GRPC_ADDR"] = proxyListener.Addr().String()
	runtime.start("settlement", "./services/settlement", settlementAddress)

	var failedSettlement string
	waitFor(t, 25*time.Second, func() bool {
		response, e := tradeClient.GetTrade(ctx, &tradev1.GetTradeRequest{InstitutionId: "sbx:institution", TradeId: tradeID})
		if e == nil && response.Status == "FAILED" {
			failedSettlement = response.SettlementId
			return failedSettlement != ""
		}
		return false
	})
	waitFor(t, 20*time.Second, func() bool { deliveredMu.Lock(); defer deliveredMu.Unlock(); return delivered["trade.failed"] >= 2 })
	if proofCount(db, `SELECT count(*) FROM ledger_sandbox_positions WHERE trade_id=$1 AND state='FX_PAYOUT_FAILED'`, tradeID) != 1 || proofCount(db, `SELECT count(*) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.account_name='fee_revenue'`, tradeID) != 0 {
		t.Fatal("failed payout did not restrict proceeds and withhold fee")
	}
	approveAction := func(action, settlementID string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"id": "sbx:process:" + tradeID + ":" + action, "institution_id": "sbx:institution", "settlement_id": settlementID, "action": action, "reason": "explicit sandbox process recovery approval"})
		for _, request := range []struct{ path, token string }{{"propose", makerToken}, {"approve", checkerToken}} {
			code, _ := proofHTTP(t, http.MethodPost, "http://"+settlementHTTP+"/v1/operator/sandbox-recovery/"+request.path, "Bearer "+request.token, "", "sbx:recovery", string(body))
			if code != 200 {
				t.Fatalf("actual recovery %s %s returned %d", action, request.path, code)
			}
		}
	}
	mu.Lock()
	outcome = "SUCCEEDED"
	mu.Unlock()
	approveAction("retry_payout", failedSettlement)
	waitFor(t, 20*time.Second, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return proxy.lostComplete })
	runtime.stop("settlement")
	runtime.stop("ledger")
	runtime.env["OSAI_LEDGER_GRPC_ADDR"] = ledgerAddress
	runtime.start("ledger", "./cmd/ledger-server", ledgerAddress)
	runtime.env["OSAI_LEDGER_GRPC_ADDR"] = proxyListener.Addr().String()
	runtime.start("settlement", "./services/settlement", settlementAddress)
	waitFor(t, 25*time.Second, func() bool {
		response, err := tradeClient.GetTrade(ctx, &tradev1.GetTradeRequest{InstitutionId: "sbx:institution", TradeId: tradeID})
		return err == nil && response.Status == "SETTLED" && response.SettlementId != "" && response.SettlementId != failedSettlement
	})
	waitFor(t, 20*time.Second, func() bool { deliveredMu.Lock(); defer deliveredMu.Unlock(); return delivered["trade.settled"] >= 2 })
	mu.Lock()
	postCount := posts
	mu.Unlock()
	deliveredMu.Lock()
	invalidDelivery := badDelivery
	deliveredMu.Unlock()
	if postCount != 2 || invalidDelivery {
		t.Fatalf("payout submissions=%d invalid customer delivery=%t", postCount, invalidDelivery)
	}
	if proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'='payout_complete'`, tradeID) != 1 || proofCount(db, `SELECT count(*) FROM settlement_finality_outbox WHERE trade_id=$1 AND status='CONFIRMED'`, tradeID) != 1 {
		t.Fatal("payout finality duplicated")
	}
	var usdHeld, ngnFee int64
	if err = db.QueryRow(`SELECT COALESCE(sum(e.credit-e.debit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.currency='USD' AND e.account_name='held'`, tradeID).Scan(&usdHeld); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COALESCE(sum(e.credit-e.debit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.account_name='fee_revenue'`, tradeID).Scan(&ngnFee); err != nil {
		t.Fatal(err)
	}
	if usdHeld != 0 || ngnFee != 500 {
		t.Fatalf("settled economics: USD held=%d NGN earned fee=%d", usdHeld, ngnFee)
	}

	mu.Lock()
	outcome = "RETURNED"
	mu.Unlock()
	var returnedSettlement string
	waitFor(t, 25*time.Second, func() bool {
		response, e := tradeClient.GetTrade(ctx, &tradev1.GetTradeRequest{InstitutionId: "sbx:institution", TradeId: tradeID})
		if e == nil && response.Status == "RETURNED" {
			returnedSettlement = response.SettlementId
			return returnedSettlement != ""
		}
		return false
	})
	waitFor(t, 20*time.Second, func() bool { deliveredMu.Lock(); defer deliveredMu.Unlock(); return delivered["trade.returned"] >= 2 })
	approveAction("release_return", returnedSettlement)
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM ledger_sandbox_positions WHERE trade_id=$1 AND state='RETURN_RELEASED'`, tradeID) == 1
	})
	approveAction("refund_fee", returnedSettlement)
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM ledger_sandbox_positions WHERE trade_id=$1 AND state='REFUNDED'`, tradeID) == 1 && proofCount(db, `SELECT count(*) FROM settlement_sandbox_recovery WHERE trade_id=$1 AND state='APPLIED'`, tradeID) == 3
	})
	var feeRefund, destinationReleased int64
	if err = db.QueryRow(`SELECT COALESCE(sum(e.credit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND j.kind='sandbox.refund_fee' AND e.account_name='available' AND e.currency='NGN'`, tradeID).Scan(&feeRefund); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COALESCE(sum(e.credit),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND j.kind='sandbox.release_return' AND e.account_name='available' AND e.currency='USD'`, tradeID).Scan(&destinationReleased); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	postCount = posts
	mu.Unlock()
	deliveredMu.Lock()
	invalidDelivery = badDelivery
	deliveredMu.Unlock()
	if feeRefund != 500 || destinationReleased != 95 || postCount != 2 || invalidDelivery {
		t.Fatalf("approved return recovery fee=%d USD=%d POSTs=%d bad_delivery=%t", feeRefund, destinationReleased, postCount, invalidDelivery)
	}
	t.Log("actual Customer -> Trade -> Settlement -> Ledger: UNKNOWN and periodic metrics, verified payout failure, lost failed receipt/restart, authenticated maker/checker distinct retry, lost successful receipt/restart, one POST per attempt, NGN500 fee only after success, full USD95 return, approved USD release and NGN500 fee refund, signed UNKNOWN/FAILED/SETTLED/RETURNED webhooks retried; no recreated source principal")
}
