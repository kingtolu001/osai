package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// This proxy injects one genuinely lost reply after the actual Ledger process
// commits. It never changes the command or pretends a failed write succeeded.
type reserveReplyProxy struct {
	ledgerv1.UnimplementedLedgerServiceServer
	client ledgerv1.LedgerServiceClient
	mu     sync.Mutex
	lost   bool
}

func (p *reserveReplyProxy) ApplySandboxAccounting(ctx context.Context, req *ledgerv1.SandboxAccountingCommand) (*ledgerv1.SandboxAccountingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	response, err := p.client.ApplySandboxAccounting(metadata.NewOutgoingContext(ctx, md.Copy()), req)
	if err != nil {
		return nil, err
	}
	var command sandboxaccounting.Command
	if err := json.Unmarshal([]byte(req.CommandJson), &command); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lost && command.Event == "reserve" && command.Terms.InstitutionID == "sbx:public" {
		p.lost = true
		return nil, status.Error(codes.Unavailable, "injected lost reserve reply after commit")
	}
	return response, nil
}

func verifyPublicFXAcceptanceRecovery(t *testing.T, runtime *proofRuntime, db *sql.DB, ledger ledgerv1.LedgerServiceClient, tradeAddress, customerAddress string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &reserveReplyProxy{client: ledger}
	server := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(server, proxy)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	runtime.stop("trade")
	runtime.env["OSAI_LEDGER_GRPC_ADDR"] = listener.Addr().String()
	runtime.env["OSAI_CUSTOMER_GRPC_ADDR"] = customerAddress
	runtime.start("trade", "./services/trade-orchestrator", tradeAddress)
	quoteAddress, quoteHTTP, gatewayAddress := loopbackPort(t), loopbackPort(t), loopbackPort(t)
	for key, value := range map[string]string{
		"OSAI_QUOTE_GRPC_ADDR": quoteAddress, "OSAI_QUOTE_PORT": quoteHTTP,
		"OSAI_QUOTE_POSTGRES_DSN": "", "OSAI_QUOTE_PRICING_OPERATORS_JSON": "",
		"OSAI_API_GATEWAY_PORT":       gatewayAddress,
		"OSAI_CUSTOMER_GRPC_ADDR":     customerAddress,
		"OSAI_SETTLEMENT_GRPC_ADDR":   customerAddress,
		"OSAI_NOTIFICATION_GRPC_ADDR": customerAddress,
		"OSAI_REPORTING_GRPC_ADDR":    customerAddress,
	} {
		runtime.env[key] = value
	}
	runtime.start("quote", "./services/quote", quoteAddress)
	runtime.start("gateway", "./services/api-gateway", gatewayAddress)
	store, err := quotecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	policy := quotecore.FeeSchedule{ID: "sbx:public-fee", Version: 1, InstitutionID: "sbx:public", SourceCurrency: "NGN", DestinationCurrency: "USD", SourceExponent: 2, DestinationExponent: 2, MinimumSourcePrincipalMinor: 1, MaximumSourcePrincipalMinor: 1000000, MinimumTotalDebitMinor: 1, MaximumTotalDebitMinor: 2000000, MinimumDestinationMinor: 1, MaximumDestinationMinor: 2000000, FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500}
	request := quotecore.QuoteRequest{CustomerID: policy.InstitutionID, QuoteMode: quotecore.QuoteModeFixedSource, BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "USD", DestinationRail: "sbx:provider", IdempotencyKey: "sbx:public-quote"}
	priced, err := quotecore.PriceQuote(policy, quotecore.QuotePricingInput{Mode: request.QuoteMode, SourceAmountMinor: 10000, RateScaled: 1000000})
	if err != nil {
		t.Fatal(err)
	}
	// An explicit fixture firm quote, not a claim of a real USD market quote.
	q, err := store.CreatePricedFromProvider(request, quotecore.ProviderQuote{ProviderID: "sbx:liquidity", BaseCurrency: "NGN", QuoteCurrency: "USD", BaseAmountMinor: 9500, RateMinor: 1000000, AmountOutMinor: 95, ExpiresAt: time.Now().Add(5 * time.Second)}, policy, priced)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"fixture_deposit":true}`
	hash := sha256.Sum256([]byte(payload))
	command := sandboxaccounting.Command{PolicyVersion: sandboxaccounting.Version, EventID: "sbx:public-deposit", Event: "fund", Actor: "sbx:test-funder", CorrelationID: "sbx:public-deposit", Reason: "explicit local fixture deposit", EvidencePayload: payload, EvidenceHash: hex.EncodeToString(hash[:]), Terms: sandboxaccounting.Terms{TradeID: "sbx:public-deposit", InstitutionID: policy.InstitutionID, PrincipalMinor: 10000}}
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err = ledger.ApplySandboxAccounting(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxAccountingCommand{CommandJson: string(raw)}); err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + gatewayAddress + "/v1/quotes/" + q.ID + "/accept"
	const body = `{"beneficiary_id":"sbx:beneficiary"}`
	const auth = "ApiKey sbx-public-client:local-public-client-fixture"
	code, _ := proofHTTP(t, http.MethodPost, endpoint, auth, "sbx:public-accept", "sbx:public-correlation", body)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("ambiguous public reserve response must remain retryable: code=%d", code)
	}
	var tradeID string
	if err = db.QueryRow(`SELECT id FROM trades WHERE institution_id=$1 AND quote_id=$2`, policy.InstitutionID, q.ID).Scan(&tradeID); err != nil {
		t.Fatal(err)
	}
	if proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'='reserve'`, tradeID) != 1 {
		t.Fatal("lost reply did not follow an actual single committed hold")
	}
	// Restart the Quote owner, then take over the expired edge lease. Quote TTL
	// remains unchanged: recovery must use its durable pre-expiry selection.
	runtime.stop("quote")
	runtime.start("quote", "./services/quote", quoteAddress)
	// No customer retry drives this step: the restarted Quote process must
	// recover its own durable selection through the real Trade/Ledger RPCs.
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM quotes WHERE id=$1 AND body->>'Status'='ACCEPTED'`, q.ID) == 1
	})
	if time.Now().Before(q.ExpiresAt) {
		<-time.After(time.Until(q.ExpiresAt.Add(time.Millisecond)))
	}
	if _, err = db.Exec(`UPDATE customer_api_idempotency SET locked_until=now()-interval '1 second' WHERE institution_id=$1 AND endpoint=$2 AND idempotency_key=$3`, policy.InstitutionID, "/v1/quotes/"+q.ID+"/accept", "sbx:public-accept"); err != nil {
		t.Fatal(err)
	}
	code, response := proofHTTP(t, http.MethodPost, endpoint, auth, "sbx:public-accept", "sbx:public-retry-correlation", body)
	if code != http.StatusOK || response["trade_id"] != tradeID {
		t.Fatalf("public acceptance recovery after expiry failed: code=%d", code)
	}
	waitFor(t, 25*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM trade_sandbox_fx_postings p JOIN trade_fx_executions e ON e.execution_id=p.execution_id WHERE e.trade_id=$1 AND p.posted_at IS NOT NULL`, tradeID) == 1
	})
	if proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'='reserve'`, tradeID) != 1 || proofCount(db, `SELECT count(*) FROM ledger_sandbox_events WHERE trade_id=$1 AND command_json::jsonb->>'Event'='fx_complete'`, tradeID) != 1 {
		t.Fatal("public retry duplicated the reserve or FX posting")
	}
	if proofCount(db, `SELECT count(*) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1 AND e.account_name='fee_revenue'`, tradeID) != 0 {
		t.Fatal("public FX acceptance earned fee before payout")
	}
	if proofCount(db, `SELECT count(*) FROM quote_outbox WHERE event_id=$1`, "evt_quote_accepted_"+q.ID) != 1 {
		t.Fatal("public acceptance did not atomically persist one Quote event")
	}
	secondGateway := loopbackPort(t)
	runtime.env["OSAI_API_GATEWAY_PORT"] = secondGateway
	runtime.start("gateway-second", "./services/api-gateway", secondGateway)
	// Freeze only these fixture buckets to make shared capacity independent of
	// wall-clock speed. No real customer counters or configurations are touched.
	if _, err = db.Exec(`UPDATE api_rate_limit_buckets SET tokens=10,last_refill_at=clock_timestamp()+interval '1 hour' WHERE scope IN ('institution:sbx:public','credential:sbx-public-client')`); err != nil {
		t.Fatal(err)
	}
	type burstResult struct {
		code    int
		message string
	}
	results := make(chan burstResult, 20)
	var load sync.WaitGroup
	for i := 0; i < 20; i++ {
		load.Add(1)
		go func(i int) {
			defer load.Done()
			address := gatewayAddress
			if i%2 == 0 {
				address = secondGateway
			}
			req, err := http.NewRequest(http.MethodGet, "http://"+address+"/v1/trades/"+tradeID, nil)
			if err != nil {
				results <- burstResult{}
				return
			}
			req.Header.Set("Authorization", auth)
			response, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
			if err != nil {
				results <- burstResult{}
				return
			}
			var failure struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if response.StatusCode == http.StatusServiceUnavailable {
				_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
			} else {
				_, _ = io.Copy(io.Discard, response.Body)
			}
			response.Body.Close()
			results <- burstResult{code: response.StatusCode, message: failure.Error.Message}
		}(i)
	}
	load.Wait()
	close(results)
	accepted, limited := 0, 0
	for result := range results {
		switch result.code {
		case http.StatusOK:
			accepted++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("shared gateway burst returned code=%d message=%q", result.code, result.message)
		}
	}
	if accepted != 10 || limited != 10 {
		t.Fatalf("two gateways escaped shared burst: accepted=%d limited=%d", accepted, limited)
	}
	runtime.stop("gateway-second")
	runtime.start("gateway-second", "./services/api-gateway", secondGateway)
	req, err := http.NewRequest(http.MethodGet, "http://"+secondGateway+"/v1/trades/"+tradeID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	responseAfterRestart, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseAfterRestart.Body.Close()
	if responseAfterRestart.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("gateway restart reset shared burst: code=%d", responseAfterRestart.StatusCode)
	}
	if _, err = db.Exec(`ALTER TABLE api_rate_limit_buckets RENAME TO api_rate_limit_buckets_fault_fixture`); err != nil {
		t.Fatal(err)
	}
	// Restore even on assertion failure; this database belongs solely to this
	// proof and is dropped by the outer test's cleanup.
	defer db.Exec(`ALTER TABLE IF EXISTS api_rate_limit_buckets_fault_fixture RENAME TO api_rate_limit_buckets`)
	failedDependencyResponse, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	failedDependencyResponse.Body.Close()
	if failedDependencyResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable rate-limit storage did not fail closed: code=%d", failedDependencyResponse.StatusCode)
	}
	if _, err = db.Exec(`ALTER TABLE api_rate_limit_buckets_fault_fixture RENAME TO api_rate_limit_buckets`); err != nil {
		t.Fatal(err)
	}
	t.Log("SHARED_RATE_LIMIT two actual Gateway processes admitted exactly 10 of 20 simultaneous fixture requests; throttling survived process restart")
	t.Log("RATE_LIMIT_FAILURE isolated proof table temporarily unavailable: actual Gateway returned HTTP 503 without a local-memory fallback; table restored")
	t.Log("PUBLIC_FX_ACCEPTANCE Gateway -> Quote -> Trade -> Ledger: lost committed reserve reply, automatic Quote restart recovery and unchanged expiry recovered the original trade and one FX posting before customer retry; fixture quote/customer only, destination payout remains gated")
}
