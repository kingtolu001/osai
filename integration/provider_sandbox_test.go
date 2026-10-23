package integration

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/notification-webhook/webhookcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// This opt-in test builds and starts the same service binaries as the local
// deployment. It uses a loopback HTTP provider and a fresh PostgreSQL database.
func TestProviderSandboxSeparateServices(t *testing.T) { runProviderSandboxSeparateServices(t, false) }

func TestSandboxFeeSeparateServices(t *testing.T) { runProviderSandboxSeparateServices(t, true) }

func runProviderSandboxSeparateServices(t *testing.T, enableFees bool) {
	adminDSN, temporal := os.Getenv("OSAI_POSTGRES_DSN"), os.Getenv("OSAI_TEMPORAL_ADDR")
	if adminDSN == "" || temporal == "" {
		t.Skip("local PostgreSQL and Temporal addresses required")
	}
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skip("local PostgreSQL unavailable")
	}
	databaseName := "osai_proof_" + randomHex(t, 6)
	if _, err := admin.Exec(`CREATE DATABASE ` + databaseName); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + databaseName
	dsn := u.String()
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + databaseName + ` WITH (FORCE)`) })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	provider := &fakeTransferProvider{status: "NEW"}
	fake := httptest.NewServer(provider)
	defer fake.Close()
	webhookProbe := &customerWebhookProbe{secret: randomHex(t, 32)}
	customerWebhook := httptest.NewServer(webhookProbe)
	defer customerWebhook.Close()
	ports := map[string]string{}
	for _, name := range []string{"customer_grpc", "customer_http", "ledger", "notification_grpc", "webhook", "settlement_grpc", "settlement_http", "trade", "quote_grpc", "quote_http", "quote_admin", "reporting", "gateway"} {
		ports[name] = loopbackPort(t)
	}
	institution := "inst_proof_" + randomHex(t, 6)
	clientID := "ck_proof_" + randomHex(t, 6)
	clientSecret, makerToken, checkerToken, wrongToken := randomHex(t, 32), randomHex(t, 32), randomHex(t, 32), randomHex(t, 32)
	pricingMakerToken, pricingCheckerToken := randomHex(t, 32), randomHex(t, 32)
	writeToken, notifyToken, webhookSecret, hash, reconToken := randomHex(t, 32), randomHex(t, 32), randomHex(t, 32), randomHex(t, 32), randomHex(t, 32)
	webhookAdminToken, webhookEncryptionKey, tradeSettlementToken := randomHex(t, 32), randomHex(t, 32), randomHex(t, 32)
	prefundToken := randomHex(t, 32)
	operatorJSON, _ := json.Marshal([]map[string]any{
		{"actor_id": "maker-a", "token": makerToken, "role": "maker", "institutions": []string{institution}},
		{"actor_id": "checker-a", "token": checkerToken, "role": "checker", "institutions": []string{institution}},
		{"actor_id": "checker-b", "token": wrongToken, "role": "checker", "institutions": []string{"inst_other"}},
	})
	pricingOperatorJSON, _ := json.Marshal([]map[string]any{
		{"actor_id": "pricing-maker", "token": pricingMakerToken, "role": "maker", "institutions": []string{institution}},
		{"actor_id": "pricing-checker", "token": pricingCheckerToken, "role": "checker", "institutions": []string{institution}},
	})
	env := map[string]string{
		"OSAI_POSTGRES_DSN": dsn, "OSAI_TEMPORAL_ADDR": temporal,
		"OSAI_CUSTOMER_GRPC_ADDR": ports["customer_grpc"], "OSAI_KYB_PORT": ports["customer_http"],
		"OSAI_LEDGER_GRPC_ADDR": ports["ledger"], "OSAI_NOTIFICATION_GRPC_ADDR": ports["notification_grpc"], "OSAI_WEBHOOK_PORT": ports["webhook"],
		"OSAI_SETTLEMENT_GRPC_ADDR": ports["settlement_grpc"], "OSAI_SETTLEMENT_PORT": ports["settlement_http"], "OSAI_TRADE_GRPC_ADDR": ports["trade"],
		"OSAI_QUOTE_GRPC_ADDR": ports["quote_grpc"], "OSAI_QUOTE_PORT": ports["quote_http"], "OSAI_REPORTING_GRPC_ADDR": ports["reporting"], "OSAI_API_GATEWAY_PORT": ports["gateway"],
		"OSAI_API_ENVIRONMENT": "sandbox",
		"OSAI_SANDBOX_SEED":    "1", "OSAI_SANDBOX_INSTITUTION_ID": institution, "OSAI_SANDBOX_CLIENT_ID": clientID, "OSAI_SANDBOX_CLIENT_SECRET": clientSecret,
		"OSAI_SANDBOX_ENTITLEMENTS":       "quotes:read,quotes:write,webhooks:write,providers:read,beneficiaries:write,trades:read,settlements:read,reports:read,balances:read,transactions:read,credentials:read,credentials:write",
		"OSAI_BENEFICIARY_OPERATORS_JSON": string(operatorJSON), "OSAI_BENEFICIARY_WRITE_TOKEN": writeToken,
		"OSAI_QUOTE_PRICING_OPERATORS_JSON": string(pricingOperatorJSON), "OSAI_QUOTE_PRICING_ADMIN_ADDR": ports["quote_admin"],
		"OSAI_NOTIFICATION_TOKEN": notifyToken, "OSAI_WEBHOOK_SECRET": webhookSecret, "OSAI_WEBHOOK_ADMIN_TOKEN": webhookAdminToken,
		"OSAI_WEBHOOK_CONFIG_ENCRYPTION_KEY": webhookEncryptionKey, "OSAI_WEBHOOK_RETRY_DELAY": "200ms",
		"OSAI_WEBHOOK_MAX_ATTEMPTS": "4", "OSAI_RECONCILIATION_TOKEN": reconToken, "OSAI_TRADE_SETTLEMENT_TOKEN": tradeSettlementToken,
		"OSAI_FLW_TRADE_ROUTING_ENABLED": "true", "FLW_SETTLEMENT_ENABLED": "true", "FLW_KILL_SWITCH": "false",
		"FLW_ENV": "test", "FLW_BASE_URL": fake.URL + "/v3", "FLW_SECRET_KEY": "local-" + randomHex(t, 20), "FLW_SECRET_HASH": hash,
		"FLW_RECONCILIATION_ENABLED": "true", "OSAI_FLW_EXPECTED_FEE_MINOR": "0", "OSAI_LOCAL_RECONCILIATION_LAG": "0s",
		"OSAI_LOCAL_PROOF_MODE": "true", "OSAI_LOCAL_PREFUND_TOKEN": prefundToken, "OSAI_CUSTODY_BANK_ID": "local_test_bank",
	}
	env["OSAI_SANDBOX_ACCOUNTING_POLICY"] = ""
	env["OSAI_TEMPORAL_TASK_QUEUE"] = "osai-proof-" + randomHex(t, 8)
	if enableFees {
		env["OSAI_SANDBOX_ACCOUNTING_POLICY"] = "osai-sandbox-accounting/v1"
		env["OSAI_SANDBOX_LEDGER_TOKEN"] = randomHex(t, 32)
	}
	runtime := newProofRuntime(t, env)
	for _, item := range []struct{ name, pkg, port string }{
		{"customer", "./services/customer-kyb", "customer_grpc"},
		{"ledger", "./cmd/ledger-server", "ledger"},
		{"notification", "./services/notification-webhook", "webhook"},
		{"settlement", "./services/settlement", "settlement_grpc"},
		{"worker", "./cmd/temporal-worker", ""},
		{"trade", "./services/trade-orchestrator", "trade"},
		{"quote", "./services/quote", "quote_grpc"},
		{"reporting", "./services/reporting", "reporting"},
		{"gateway", "./services/api-gateway", "gateway"},
	} {
		runtime.start(item.name, item.pkg, ports[item.port])
	}
	pricingPolicy, err := json.Marshal(map[string]any{
		"institution_id": institution, "source_currency": "NGN", "destination_currency": "NGN",
		"source_minor_exponent": 2, "destination_minor_exponent": 2,
		"minimum_source_principal_minor": 1, "maximum_source_principal_minor": 100_000_000,
		"minimum_total_debit_minor": 1, "maximum_total_debit_minor": 100_000_000,
		"minimum_destination_minor": 1, "maximum_destination_minor": 100_000_000,
		"fixed_fee_minor": 0, "fee_basis_points": 0, "minimum_fee_minor": 0, "maximum_fee_minor": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposalBody, err := json.Marshal(map[string]any{"fee_schedule": json.RawMessage(pricingPolicy), "reason": "local integration fixture"})
	if err != nil {
		t.Fatal(err)
	}
	quoteAdmin := "http://" + ports["quote_admin"]
	waitFor(t, 15*time.Second, func() bool {
		connection, dialErr := net.DialTimeout("tcp", ports["quote_admin"], 100*time.Millisecond)
		if dialErr != nil {
			return false
		}
		_ = connection.Close()
		return true
	})
	pricingCorrelation := "pricing-" + institution
	proposalCode, proposal := proofHTTP(t, http.MethodPost, quoteAdmin+"/v1/operator/pricing/propose", "Bearer "+pricingMakerToken, "", pricingCorrelation, string(proposalBody))
	if proposalCode != http.StatusCreated {
		t.Fatalf("pricing policy proposal: code=%d response=%v", proposalCode, proposal)
	}
	decisionBody, err := json.Marshal(map[string]string{"proposal_id": stringField(t, proposal, "proposal_id"), "institution_id": institution, "reason": "approve local integration fixture"})
	if err != nil {
		t.Fatal(err)
	}
	decisionCode, _ := proofHTTP(t, http.MethodPost, quoteAdmin+"/v1/operator/pricing/approve", "Bearer "+pricingCheckerToken, "", pricingCorrelation, string(decisionBody))
	if decisionCode != http.StatusNoContent {
		t.Fatalf("pricing policy approval: code=%d", decisionCode)
	}
	api := "http://" + ports["gateway"]
	op := "http://" + ports["customer_http"]
	correlation := "cor_proof_" + randomHex(t, 6)
	clientAuth := "ApiKey " + clientID + ":" + clientSecret
	call := func(method, endpoint, auth, key, body string) (int, map[string]any) {
		return proofHTTP(t, method, endpoint, auth, key, correlation, body)
	}
	providerStatusCode, providerStatus := call(http.MethodGet, api+"/v1/providers/status", clientAuth, "", "")
	routes, routesOK := providerStatus["items"].([]any)
	if providerStatusCode != http.StatusOK || !routesOK || len(routes) == 0 {
		t.Fatalf("provider status: code=%d response=%v", providerStatusCode, providerStatus)
	}
	for _, rawRoute := range routes {
		route, ok := rawRoute.(map[string]any)
		if !ok || route["route_id"] == "" || route["provider_id"] != nil {
			t.Fatalf("provider status leaked identity or malformed route: %v", rawRoute)
		}
	}
	webhookConfigBody, err := json.Marshal(map[string]any{
		"endpoint_url":           customerWebhook.URL,
		"signing_secret":         webhookProbe.secret,
		"subscribed_event_types": []string{"quote.accepted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	webhookStatus, webhookConfig := call(http.MethodPost, api+"/v1/webhook-endpoints", clientAuth, "webhook-config-1", string(webhookConfigBody))
	if webhookStatus != http.StatusOK || webhookConfig["configured"] != true {
		t.Fatalf("configure customer webhook through public API: code=%d response=%v", webhookStatus, webhookConfig)
	}
	if proofCount(db, `SELECT count(*) FROM customer_credentials WHERE public_id=$1 AND institution_id=$2`, clientID, institution) != 1 ||
		proofCount(db, `SELECT count(*) FROM customer_institutions WHERE id=$1 AND body->'entitlements' @> '["quotes:read","quotes:write","webhooks:write","providers:read"]'::jsonb`, institution) != 1 ||
		proofCount(db, `SELECT count(*) FROM customer_webhook_config WHERE institution_id=$1`, institution) != 1 {
		t.Fatal("customer credentials, entitlements, and webhook configuration must be persisted by customer-kyb")
	}
	runtime.stop("customer")
	runtime.start("customer", "./services/customer-kyb", ports["customer_grpc"])
	restartedCustomerStatus, restartedCustomerResponse := call("GET", api+"/v1/providers/status", clientAuth, "", "")
	if restartedCustomerStatus != http.StatusOK {
		t.Fatalf("customer API credential did not survive customer-kyb restart: code=%d response=%v", restartedCustomerStatus, restartedCustomerResponse)
	}
	verifyPublicCredentialLifecycle(t, api, clientAuth, correlation, func() {
		runtime.stop("customer")
		runtime.start("customer", "./services/customer-kyb", ports["customer_grpc"])
	})
	ledgerConn, err := grpc.Dial(ports["ledger"], grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer ledgerConn.Close()
	ledgerClient := ledgerv1.NewLedgerServiceClient(ledgerConn)
	fixtureContext := metadata.AppendToOutgoingContext(context.Background(), "x-osai-local-prefund-token", prefundToken)
	if enableFees {
		fixtureContext = metadata.AppendToOutgoingContext(fixtureContext, "x-osai-sandbox-ledger-token", env["OSAI_SANDBOX_LEDGER_TOKEN"])
	}
	statusCode, beneficiary := call("POST", api+"/v1/beneficiaries", clientAuth, "beneficiary-1", `{"name":"Proof recipient","bank_code":"044","account_number":"0690000040"}`)
	if statusCode != http.StatusAccepted || beneficiary["approval_status"] != "PENDING" {
		t.Fatalf("beneficiary registration: code=%d response=%v", statusCode, beneficiary)
	}
	beneficiaryID := stringField(t, beneficiary, "beneficiary_id")
	operatorBody := fmt.Sprintf(`{"institution_id":%q,"beneficiary_id":%q,"reason":"verified local sandbox recipient"}`, institution, beneficiaryID)
	if code, _ := call("POST", op+"/v1/operator/beneficiaries/propose", "Bearer "+makerToken, "", operatorBody); code != http.StatusNoContent {
		t.Fatalf("maker proposal: %d", code)
	}
	if code, _ := call("POST", op+"/v1/operator/beneficiaries/approve", "Bearer "+wrongToken, "", operatorBody); code != http.StatusForbidden {
		t.Fatalf("wrong institution checker: %d", code)
	}
	if code, _ := call("POST", op+"/v1/operator/beneficiaries/approve", "Bearer "+checkerToken, "", operatorBody); code != http.StatusNoContent {
		t.Fatalf("checker approval: %d", code)
	}
	statusCode, quote := call("POST", api+"/v1/quotes", clientAuth, "quote-1", `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":"flutterwave","customer_reference":"proof-order-1"}`)
	if statusCode != http.StatusOK || quote["amount_out_minor"] != float64(10000) ||
		quote["quote_mode"] != "FIXED_SOURCE" || quote["source_principal_minor"] != float64(10000) ||
		quote["total_debit_minor"] != float64(10000) || quote["fee_schedule_id"] == "" {
		t.Fatalf("quote: code=%d response=%v", statusCode, quote)
	}
	quoteID := stringField(t, quote, "quote_id")
	acceptBody := fmt.Sprintf(`{"beneficiary_id":%q}`, beneficiaryID)
	statusCode, accepted := call("POST", api+"/v1/quotes/"+quoteID+"/accept", clientAuth, "accept-1", acceptBody)
	if statusCode != http.StatusOK {
		t.Fatalf("quote acceptance: code=%d response=%v", statusCode, accepted)
	}
	tradeID := stringField(t, accepted, "trade_id")
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.accepted' AND delivered_at IS NOT NULL`, tradeID) == 1
	})
	eventID := "evt_quote_accepted_" + quoteID
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM outbound_events WHERE event_id=$1 AND status='RETRY' AND attempt_count=1`, eventID) == 1
	})
	var storedSecret []byte
	if err := db.QueryRow(`SELECT secret_ciphertext FROM webhook_config WHERE institution_id=$1`, institution).Scan(&storedSecret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedSecret), webhookProbe.secret) {
		t.Fatal("notification persisted customer webhook signing secret in plaintext")
	}
	runtime.stop("notification")
	runtime.start("notification", "./services/notification-webhook", ports["webhook"])
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM outbound_events WHERE event_id=$1 AND status='SUCCESS' AND attempt_count=2`, eventID) == 1
	})
	if webhookProbe.count() != 2 || webhookProbe.invalidSignature() {
		t.Fatalf("customer webhook delivery count/signature: attempts=%d invalid_signature=%v", webhookProbe.count(), webhookProbe.invalidSignature())
	}
	statusCode, waitingTrade := call("GET", api+"/v1/trades/"+tradeID, clientAuth, "", "")
	if statusCode != http.StatusOK || waitingTrade["status"] != "FUNDING_REQUIRED" || waitingTrade["settlement_id"] != "" || provider.postCount() != 0 || proofCount(db, `SELECT count(*) FROM settlement_instructions`) != 0 {
		t.Fatalf("zero-prefund trade reached provider: code=%d trade=%v posts=%d", statusCode, waitingTrade, provider.postCount())
	}
	if code, replay := call("POST", api+"/v1/quotes/"+quoteID+"/accept", clientAuth, "accept-1", acceptBody); code != http.StatusOK || replay["trade_id"] != tradeID {
		t.Fatalf("acceptance replay: code=%d response=%v", code, replay)
	}
	fixture, err := ledgerClient.PostLocalPrefund(fixtureContext, &ledgerv1.LocalPrefundCommand{InstitutionId: institution, Amount: &ledgerv1.Money{AmountMinor: 30000, Currency: "NGN"}, EvidenceReference: "bank_proof_" + randomHex(t, 6), CorrelationId: correlation})
	if err != nil || fixture.JournalId == "" {
		t.Fatalf("Ledger prefund fixture failed: %v", err)
	}
	code, beforeFundingHold := call("GET", api+"/v1/balances", clientAuth, "", "")
	beforeFundingNGN := balanceForCurrency(t, beforeFundingHold, "NGN")
	if code != http.StatusOK || beforeFundingNGN["available_minor"] != float64(30000) || beforeFundingNGN["held_minor"] != float64(0) {
		t.Fatalf("prefund balance before hold: code=%d response=%v", code, beforeFundingHold)
	}
	runtime.stop("worker")
	statusCode, fundedAcceptance := call("POST", api+"/v1/quotes/"+quoteID+"/accept", clientAuth, "accept-after-prefund", acceptBody)
	if statusCode != http.StatusOK || fundedAcceptance["trade_id"] != tradeID {
		t.Fatalf("funded acceptance did not resume trade: code=%d response=%v", statusCode, fundedAcceptance)
	}
	statusCode, trade := call("GET", api+"/v1/trades/"+tradeID, clientAuth, "", "")
	if statusCode != http.StatusOK {
		t.Fatalf("trade read: code=%d response=%v", statusCode, trade)
	}
	settlementID := stringField(t, trade, "settlement_id")
	code, afterFundingHold := call("GET", api+"/v1/balances", clientAuth, "", "")
	afterFundingNGN := balanceForCurrency(t, afterFundingHold, "NGN")
	if code != http.StatusOK || afterFundingNGN["available_minor"] != float64(20000) || afterFundingNGN["held_minor"] != float64(10000) || provider.postCount() != 0 {
		t.Fatalf("funding hold before provider submission: code=%d balance=%v provider_posts=%d", code, afterFundingHold, provider.postCount())
	}
	if proofCount(db, `SELECT count(*) FROM ledger_funding_reservations WHERE trade_id=$1 AND state='ACTIVE'`, tradeID) != 1 {
		t.Fatal("one active Ledger reservation required")
	}
	if proofCount(db, `SELECT count(*) FROM trade_transitions WHERE trade_id=$1 AND to_state='FUNDED'`, tradeID) != 1 || proofCount(db, `SELECT count(*) FROM trade_transitions WHERE trade_id=$1 AND from_state='FUNDED' AND to_state='SETTLING'`, tradeID) != 1 {
		t.Fatal("trade did not pass through FUNDED before SETTLING")
	}
	replayCommand := &ledgerv1.FundingReservationCommand{InstitutionId: institution, TradeId: tradeID, Amount: &ledgerv1.Money{AmountMinor: 10000, Currency: "NGN"}, CorrelationId: correlation}
	if _, err := ledgerClient.ReserveFunding(fixtureContext, replayCommand); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("incomplete funding replay must report a payload conflict: %v", err)
	}
	if err := db.QueryRow(`SELECT source_principal_minor,fee_minor,pricing_hash,pricing_snapshot_json FROM ledger_funding_reservations WHERE trade_id=$1`, tradeID).Scan(&replayCommand.SourcePrincipalMinor, &replayCommand.FeeMinor, &replayCommand.PricingHash, &replayCommand.PricingSnapshotJson); err != nil {
		t.Fatal(err)
	}
	replayFunding, err := ledgerClient.ReserveFunding(fixtureContext, replayCommand)
	if err != nil || !replayFunding.AlreadyApplied || proofCount(db, `SELECT count(*) FROM ledger_journals WHERE dedup_key=$1`, "hold:"+tradeID) != 1 {
		t.Fatalf("funding reservation replay changed hold: response=%v error=%v", replayFunding, err)
	}
	runtime.start("worker", "./cmd/temporal-worker", "")
	providerStarted := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if provider.postCount() == 1 {
			providerStarted = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !providerStarted {
		var state string
		_ = db.QueryRow(`SELECT state FROM settlement_instructions WHERE id=$1`, settlementID).Scan(&state)
		workerLog, _ := os.ReadFile(filepath.Join(runtime.dir, "worker.log"))
		t.Fatalf("funded provider submission did not start: settlement_state=%s hold_state=ACTIVE worker_log=%s", state, strings.ReplaceAll(string(workerLog), "0690000040", "[redacted]"))
	}
	runtime.stop("trade")
	runtime.stop("worker")
	provider.setStatus("SUCCESSFUL")
	runtime.start("worker", "./cmd/temporal-worker", "")
	confirmed := false
	until := time.Now().Add(75 * time.Second)
	for time.Now().Before(until) {
		code, state := call("GET", api+"/v1/settlements/"+settlementID, clientAuth, "", "")
		if code == http.StatusOK && state["status"] == "CONFIRMED" {
			confirmed = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !confirmed {
		var state string
		_ = db.QueryRow(`SELECT state FROM settlement_instructions WHERE id=$1`, settlementID).Scan(&state)
		for _, name := range []string{"worker", "ledger", "settlement"} {
			if raw, readErr := os.ReadFile(filepath.Join(runtime.dir, name+".log")); readErr == nil {
				lines := strings.Split(string(raw), "\n")
				if len(lines) > 12 {
					lines = lines[len(lines)-12:]
				}
				t.Logf("%s last diagnostics: %s", name, strings.ReplaceAll(strings.Join(lines, "\n"), "0690000040", "[redacted]"))
			}
		}
		t.Fatalf("settlement did not confirm after worker restart: state=%s transitions=%d evidence=%d journals=%d provider_posts=%d", state, proofCount(db, `SELECT count(*) FROM settlement_transitions WHERE settlement_id=$1`, settlementID), proofCount(db, `SELECT count(*) FROM settlement_evidence WHERE settlement_id=$1`, settlementID), proofCount(db, `SELECT count(*) FROM ledger_journals`), provider.postCount())
	}
	if got := proofCount(db, `SELECT count(*) FROM settlement_finality_outbox WHERE settlement_id=$1 AND status='CONFIRMED' AND delivered_at IS NULL`, settlementID); got != 1 {
		t.Fatalf("expected one unacknowledged durable settlement finality event, got %d", got)
	}
	runtime.start("trade", "./services/trade-orchestrator", ports["trade"])
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM settlement_finality_outbox WHERE settlement_id=$1 AND delivered_at IS NOT NULL`, settlementID) == 1
	})
	statusCode, final := call("GET", api+"/v1/settlements/"+settlementID, clientAuth, "", "")
	if statusCode != http.StatusOK || final["status"] != "CONFIRMED" {
		t.Fatalf("customer settlement status: code=%d response=%v", statusCode, final)
	}
	code, afterFinality := call("GET", api+"/v1/balances", clientAuth, "", "")
	afterFinalityNGN := balanceForCurrency(t, afterFinality, "NGN")
	if code != http.StatusOK || afterFinalityNGN["available_minor"] != float64(20000) || afterFinalityNGN["held_minor"] != float64(0) {
		t.Fatalf("settlement did not consume hold: code=%d balance=%v", code, afterFinality)
	}
	var finalTrade map[string]any
	statusCode = http.StatusOK
	waitFor(t, 20*time.Second, func() bool {
		statusCode, finalTrade = call("GET", api+"/v1/trades/"+tradeID, clientAuth, "", "")
		return statusCode == http.StatusOK && finalTrade["status"] == "SETTLED"
	})
	if statusCode != http.StatusOK || finalTrade["status"] != "SETTLED" || proofCount(db, `SELECT count(*) FROM trade_settlement_event_inbox WHERE event_id=$1`, "settlement_finality:"+settlementID+":CONFIRMED") != 1 {
		t.Fatalf("customer trade final status: code=%d response=%v", statusCode, finalTrade)
	}
	reportFrom := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	reportTo := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	tradesURL := fmt.Sprintf("%s/v1/trades?page=1&page_size=1&from=%s&to=%s&currency=NGN&status=SETTLED&customer_reference=proof-order-1",
		api, url.QueryEscape(reportFrom), url.QueryEscape(reportTo))
	statusCode, tradesPage := call(http.MethodGet, tradesURL, clientAuth, "", "")
	tradeItems, tradeItemsOK := tradesPage["items"].([]any)
	var listedTrade map[string]any
	var listedTradeOK bool
	if len(tradeItems) == 1 {
		listedTrade, listedTradeOK = tradeItems[0].(map[string]any)
	}
	if statusCode != http.StatusOK || tradesPage["total"] != float64(1) || tradesPage["page"] != float64(1) ||
		!tradeItemsOK || !listedTradeOK || listedTrade["trade_id"] != tradeID ||
		listedTrade["customer_reference"] != "proof-order-1" {
		t.Fatalf("filtered customer trade list: code=%d response=%v", statusCode, tradesPage)
	}
	settlementsURL := fmt.Sprintf("%s/v1/reports/settlements?page=1&page_size=1&from=%s&to=%s&currency=NGN&status=CONFIRMED",
		api, url.QueryEscape(reportFrom), url.QueryEscape(reportTo))
	statusCode, settlementPage := call(http.MethodGet, settlementsURL, clientAuth, "", "")
	settlementItems, settlementItemsOK := settlementPage["items"].([]any)
	if statusCode != http.StatusOK || settlementPage["total"] != float64(1) || settlementPage["page"] != float64(1) ||
		len(settlementItems) != 1 || !settlementItemsOK {
		t.Fatalf("filtered customer settlement report: code=%d response=%v", statusCode, settlementPage)
	}
	runtime.start("reconciliation", "./cmd/reconciliation", "")
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM reconciliation_evidence WHERE client_ref=$1`, provider.reference()) > 0
	})
	if got := proofCount(db, `SELECT count(*) FROM reconciliation_breaks`); got != 0 {
		t.Fatalf("clean reconciliation created %d breaks", got)
	}
	for query, expected := range map[string]int{
		`SELECT count(*) FROM customer_institutions`: 1, `SELECT count(*) FROM customer_beneficiaries`: 1,
		`SELECT count(*) FROM quotes`: 1, `SELECT count(*) FROM trades`: 1,
		`SELECT count(*) FROM settlement_instructions`: 1, `SELECT count(*) FROM ledger_journals`: 3,
	} {
		if got := proofCount(db, query); got != expected {
			t.Fatalf("%s: got %d want %d", query, got, expected)
		}
	}
	if provider.postCount() != 1 {
		t.Fatalf("provider creates=%d", provider.postCount())
	}
	var journalID string
	if err := db.QueryRow(`SELECT journal_id FROM ledger_journals WHERE trade_id=$1 AND dedup_key=$2`, tradeID, "settlement:"+tradeID).Scan(&journalID); err != nil {
		t.Fatal(err)
	}
	statusCode, transactions := call("GET", api+"/v1/transactions", clientAuth, "", "")
	if statusCode != http.StatusOK || transactions["total"] != float64(2) {
		t.Fatalf("customer transaction read: code=%d response=%v", statusCode, transactions)
	}
	items, ok := transactions["items"].([]any)
	settlementVisible := false
	prefundVisible := false
	for _, item := range items {
		entry, valid := item.(map[string]any)
		if valid && entry["transaction_id"] == journalID && entry["amount_minor"] == float64(-10000) {
			settlementVisible = true
		}
		if valid && entry["transaction_id"] == fixture.JournalId && entry["amount_minor"] == float64(30000) {
			prefundVisible = true
		}
	}
	if !ok || !settlementVisible || !prefundVisible {
		t.Fatalf("customer transaction did not reflect settlement: %v", transactions)
	}
	var debit, credit int64
	if err := db.QueryRow(`SELECT COALESCE(sum(debit),0),COALESCE(sum(credit),0) FROM ledger_entries WHERE journal_id=$1`, journalID).Scan(&debit, &credit); err != nil || debit != credit || debit <= 0 {
		t.Fatalf("journal imbalance debit=%d credit=%d err=%v", debit, credit, err)
	}
	var evidenceID string
	if err := db.QueryRow(`SELECT id FROM reconciliation_evidence WHERE client_ref=$1 LIMIT 1`, provider.reference()).Scan(&evidenceID); err != nil {
		t.Fatal(err)
	}
	t.Logf("GOLDEN institution=%s beneficiary=%s quote=%s trade=%s settlement=%s client_ref=%s provider_ref=%s journal=%s reconciliation_evidence=%s correlation=%s", institution, beneficiaryID, quoteID, tradeID, settlementID, provider.reference(), provider.providerRef(), journalID, evidenceID, correlation)
	callback := fmt.Sprintf(`{"event":"transfer.completed","data":{"id":%s,"reference":%q,"amount":100,"currency":"NGN","status":"SUCCESSFUL","created_at":"2026-10-03T00:00:00Z"}}`, provider.providerRef(), provider.reference())
	sendCallback := func() int {
		request, err := http.NewRequest(http.MethodPost, "http://"+ports["webhook"]+"/v1/webhooks/providers/flutterwave", bytes.NewBufferString(callback))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Verif-Hash", hash)
		response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if code := sendCallback(); code != http.StatusOK {
		t.Fatalf("late callback status=%d", code)
	}
	if got := proofCount(db, `SELECT count(*) FROM provider_webhook_inbox WHERE client_ref=$1`, provider.reference()); got != 1 {
		t.Fatalf("durable callback count=%d", got)
	}
	runtime.stop("notification")
	runtime.start("notification", "./services/notification-webhook", ports["webhook"])
	if code := sendCallback(); code != http.StatusOK {
		t.Fatalf("duplicate callback after restart status=%d", code)
	}
	if got := proofCount(db, `SELECT count(*) FROM provider_webhook_inbox WHERE client_ref=$1`, provider.reference()); got != 1 {
		t.Fatalf("callback dedup after restart=%d", got)
	}
	provider.setStatementAmount(101)
	waitFor(t, 15*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM reconciliation_breaks WHERE type='amount_mismatch' AND state='OPEN'`) == 1
	})
	var breakID string
	if err := db.QueryRow(`SELECT id FROM reconciliation_breaks WHERE type='amount_mismatch'`).Scan(&breakID); err != nil {
		t.Fatal(err)
	}
	runtime.stop("reconciliation")
	runtime.start("reconciliation", "./cmd/reconciliation", "")
	time.Sleep(3 * time.Second)
	if got := proofCount(db, `SELECT count(*) FROM reconciliation_breaks WHERE type='amount_mismatch'`); got != 1 {
		t.Fatalf("replayed mismatch produced %d logical breaks", got)
	}
	provider.setStatementAmount(100)
	waitFor(t, 15*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM reconciliation_breaks WHERE id=$1 AND state='RESOLVED'`, breakID) == 1
	})
	if got := proofCount(db, `SELECT count(*) FROM ledger_journals`); got != 3 || provider.postCount() != 1 {
		t.Fatalf("reconciliation changed economic effects: journals=%d provider_posts=%d", got, provider.postCount())
	}
	t.Logf("REPLAY callback_inbox=1 provider_posts=1 prefund_journals=1 hold_journals=1 settlement_journals=1 mismatch_break=%s resolved_after_matching_evidence", breakID)
	runtime.stop("reconciliation")
	provider.setStatus("NEW")
	provider.setHoldCreate(12 * time.Second)
	code, secondQuote := call("POST", api+"/v1/quotes", clientAuth, "quote-timeout", `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":"flutterwave"}`)
	if code != http.StatusOK {
		t.Fatalf("timeout quote: code=%d response=%v", code, secondQuote)
	}
	code, secondAcceptance := call("POST", api+"/v1/quotes/"+stringField(t, secondQuote, "quote_id")+"/accept", clientAuth, "accept-timeout", acceptBody)
	if code != http.StatusOK {
		t.Fatalf("timeout quote acceptance: code=%d response=%v", code, secondAcceptance)
	}
	secondTradeID := stringField(t, secondAcceptance, "trade_id")
	code, secondTrade := call("GET", api+"/v1/trades/"+secondTradeID, clientAuth, "", "")
	if code != http.StatusOK {
		t.Fatalf("timeout trade read: code=%d response=%v", code, secondTrade)
	}
	secondSettlementID := stringField(t, secondTrade, "settlement_id")
	waitFor(t, 15*time.Second, func() bool { return provider.postCount() == 2 })
	secondReference := provider.reference()
	secondProviderRef := provider.providerRef()
	waitFor(t, 20*time.Second, func() bool {
		return proofCount(db, `SELECT count(*) FROM settlement_transitions WHERE settlement_id=$1 AND to_state='UNKNOWN'`, secondSettlementID) > 0
	})
	provider.setHoldCreate(0)
	provider.setStatus("SUCCESSFUL")
	runtime.stop("worker")
	runtime.env["FLW_SETTLEMENT_ENABLED"] = "false"
	runtime.env["FLW_KILL_SWITCH"] = "true"
	runtime.start("worker", "./cmd/temporal-worker", "")
	runtime.stop("quote")
	runtime.start("quote", "./services/quote", ports["quote_grpc"])
	replayedQuoteCode, replayedQuote := call("GET", api+"/v1/quotes/"+quoteID, clientAuth, "", "")
	if replayedQuoteCode != http.StatusOK || replayedQuote["fee_schedule_id"] != quote["fee_schedule_id"] ||
		replayedQuote["total_debit_minor"] != quote["total_debit_minor"] || replayedQuote["rate_minor"] != quote["rate_minor"] {
		t.Fatalf("priced quote snapshot did not survive Quote Service restart: code=%d response=%v", replayedQuoteCode, replayedQuote)
	}
	if code, _ := call("POST", api+"/v1/quotes", clientAuth, "quote-killed", `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":"flutterwave"}`); code == http.StatusOK {
		t.Fatal("kill switch admitted a new Flutterwave quote")
	}
	waitFor(t, 75*time.Second, func() bool {
		code, state := call("GET", api+"/v1/settlements/"+secondSettlementID, clientAuth, "", "")
		return code == http.StatusOK && state["status"] == "CONFIRMED"
	})
	if provider.postCount() != 2 || provider.reference() != secondReference || provider.providerRef() != secondProviderRef || proofCount(db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1 AND dedup_key=$2`, secondTradeID, "settlement:"+secondTradeID) != 1 {
		t.Fatalf("UNKNOWN recovery duplicated effect: posts=%d ref=%s journals=%d", provider.postCount(), provider.reference(), proofCount(db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1`, secondTradeID))
	}
	t.Logf("UNKNOWN settlement=%s client_ref=%s provider_ref=%s transitions=UNKNOWN,CONFIRMED provider_posts=2 total_journals=2 kill_switch=on", secondSettlementID, secondReference, secondProviderRef)
	runtime.env["FLW_SETTLEMENT_ENABLED"] = "true"
	runtime.env["FLW_KILL_SWITCH"] = "false"
	runtime.stop("worker")
	runtime.start("worker", "./cmd/temporal-worker", "")
	runtime.stop("quote")
	runtime.start("quote", "./services/quote", ports["quote_grpc"])
	provider.setHealthDown(true)
	if code, _ := call("POST", api+"/v1/quotes", clientAuth, "quote-degraded", `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":"flutterwave"}`); code == http.StatusOK {
		t.Fatal("degraded provider admitted a new Flutterwave quote")
	}
	provider.setHealthDown(false)
	provider.setHoldCreate(4 * time.Second)
	code, thirdQuote := call("POST", api+"/v1/quotes", clientAuth, "quote-early-callback", `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":"flutterwave"}`)
	if code != http.StatusOK {
		t.Fatalf("early callback quote: code=%d response=%v", code, thirdQuote)
	}
	code, thirdAcceptance := call("POST", api+"/v1/quotes/"+stringField(t, thirdQuote, "quote_id")+"/accept", clientAuth, "accept-early-callback", acceptBody)
	if code != http.StatusOK {
		t.Fatalf("early callback acceptance: code=%d response=%v", code, thirdAcceptance)
	}
	thirdTradeID := stringField(t, thirdAcceptance, "trade_id")
	code, thirdTrade := call("GET", api+"/v1/trades/"+thirdTradeID, clientAuth, "", "")
	if code != http.StatusOK {
		t.Fatalf("early callback trade: code=%d response=%v", code, thirdTrade)
	}
	thirdSettlementID := stringField(t, thirdTrade, "settlement_id")
	waitFor(t, 15*time.Second, func() bool { return provider.postCount() == 3 })
	if got := provider.createResponseCount(); got != 2 {
		t.Fatalf("create response already released before early callback: %d", got)
	}
	thirdRef, thirdProviderRef := provider.reference(), provider.providerRef()
	earlyCallback := fmt.Sprintf(`{"event":"transfer.completed","data":{"id":%s,"reference":%q,"amount":100,"currency":"NGN","status":"SUCCESSFUL","created_at":"2026-10-03T00:00:00Z"}}`, thirdProviderRef, thirdRef)
	request, err := http.NewRequest(http.MethodPost, "http://"+ports["webhook"]+"/v1/webhooks/providers/flutterwave", bytes.NewBufferString(earlyCallback))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Verif-Hash", hash)
	response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || proofCount(db, `SELECT count(*) FROM provider_webhook_inbox WHERE client_ref=$1`, thirdRef) != 1 {
		t.Fatalf("early callback not persisted: status=%d", response.StatusCode)
	}
	if got := provider.createResponseCount(); got != 2 {
		t.Fatalf("callback arrived after create response: responses=%d", got)
	}
	waitFor(t, 75*time.Second, func() bool {
		code, state := call("GET", api+"/v1/settlements/"+thirdSettlementID, clientAuth, "", "")
		return code == http.StatusOK && state["status"] == "CONFIRMED"
	})
	if provider.postCount() != 3 || proofCount(db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1 AND dedup_key=$2`, thirdTradeID, "settlement:"+thirdTradeID) != 1 {
		t.Fatalf("early callback duplicated effect: posts=%d journals=%d", provider.postCount(), proofCount(db, `SELECT count(*) FROM ledger_journals WHERE trade_id=$1`, thirdTradeID))
	}
	t.Logf("CALLBACK_ORDER settlement=%s client_ref=%s callback_before_create_response=true webhook_inbox=1 journal=1 degraded_route_rejected=true", thirdSettlementID, thirdRef)
	code, balance := call("GET", api+"/v1/balances", clientAuth, "", "")
	if code != http.StatusOK {
		t.Errorf("customer balance read: code=%d response=%v", code, balance)
	}
	ngnBalance := balanceForCurrency(t, balance, "NGN")
	if ngnBalance["available_minor"] != float64(0) || ngnBalance["held_minor"] != float64(0) {
		t.Fatalf("three funded payouts did not consume 30000 minor: %v", balance)
	}
	code, unfundedQuote := call("POST", api+"/v1/quotes", clientAuth, "quote-unfunded", `{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":"flutterwave"}`)
	if code != http.StatusOK {
		t.Fatalf("unfunded quote unavailable: %d %v", code, unfundedQuote)
	}
	code, unfundedAcceptance := call("POST", api+"/v1/quotes/"+stringField(t, unfundedQuote, "quote_id")+"/accept", clientAuth, "accept-unfunded", acceptBody)
	if code != http.StatusOK {
		t.Fatalf("unfunded trade acceptance unavailable: %d %v", code, unfundedAcceptance)
	}
	code, unfundedTrade := call("GET", api+"/v1/trades/"+stringField(t, unfundedAcceptance, "trade_id"), clientAuth, "", "")
	if code != http.StatusOK || unfundedTrade["status"] != "FUNDING_REQUIRED" || unfundedTrade["settlement_id"] != "" || provider.postCount() != 3 || proofCount(db, `SELECT count(*) FROM settlement_instructions`) != 3 {
		t.Fatalf("zero-prefund trade reached payout: code=%d trade=%v provider_posts=%d", code, unfundedTrade, provider.postCount())
	}
	t.Logf("FUNDING before=30000/0 reserved=20000/10000 after_first=20000/0 after_three=0/0 unfunded_trade=FUNDING_REQUIRED provider_posts=3")
	_, err = ledgerClient.PostLocalPrefund(fixtureContext, &ledgerv1.LocalPrefundCommand{InstitutionId: institution, Amount: &ledgerv1.Money{AmountMinor: 10000, Currency: "NGN"}, EvidenceReference: "bank_failure_" + randomHex(t, 6), CorrelationId: correlation})
	if err != nil {
		t.Fatalf("verified failure-case prefund unavailable: %v", err)
	}
	provider.setStatus("FAILED")
	code, resumed := call("POST", api+"/v1/quotes/"+stringField(t, unfundedQuote, "quote_id")+"/accept", clientAuth, "accept-after-funding", acceptBody)
	if code != http.StatusOK || resumed["trade_id"] != unfundedAcceptance["trade_id"] {
		t.Fatalf("funding-required trade did not resume: code=%d response=%v", code, resumed)
	}
	failureTradeID := stringField(t, resumed, "trade_id")
	code, fundingTrade := call("GET", api+"/v1/trades/"+failureTradeID, clientAuth, "", "")
	if code != http.StatusOK || fundingTrade["settlement_id"] == "" {
		t.Fatalf("funded retry did not create settlement: code=%d response=%v", code, fundingTrade)
	}
	failureSettlementID := stringField(t, fundingTrade, "settlement_id")
	waitFor(t, 75*time.Second, func() bool {
		code, state := call("GET", api+"/v1/settlements/"+failureSettlementID, clientAuth, "", "")
		return code == http.StatusOK && state["status"] == "FAILED"
	})
	code, failureBalance := call("GET", api+"/v1/balances", clientAuth, "", "")
	failureNGN := balanceForCurrency(t, failureBalance, "NGN")
	if code != http.StatusOK || failureNGN["available_minor"] != float64(10000) || failureNGN["held_minor"] != float64(0) || provider.postCount() != 4 || proofCount(db, `SELECT count(*) FROM ledger_funding_reservations WHERE trade_id=$1 AND state='RELEASED'`, failureTradeID) != 1 {
		t.Fatalf("provider failure did not release funded hold: balance=%v posts=%d", failureBalance, provider.postCount())
	}
	t.Logf("FAILURE trade=%s settlement=%s hold=RELEASED available=10000 held=0 provider_posts=4", failureTradeID, failureSettlementID)
	if fundingCredits := proofCount(db, `SELECT count(*) FROM ledger_entries WHERE owner=$1 AND account_code='2000' AND credit>0`, institution); fundingCredits == 0 {
		t.Errorf("funding gate absent: three provider payouts completed without any institution prefund journal; public balance=%v", balance)
	}

	feeSchedule, err := json.Marshal(map[string]any{
		"institution_id": institution, "source_currency": "NGN", "destination_currency": "NGN",
		"source_minor_exponent": 2, "destination_minor_exponent": 2,
		"minimum_source_principal_minor": 1, "maximum_source_principal_minor": 100_000_000,
		"minimum_total_debit_minor": 1, "maximum_total_debit_minor": 100_000_000,
		"minimum_destination_minor": 1, "maximum_destination_minor": 100_000_000,
		"fixed_fee_minor": 500, "fee_basis_points": 0, "minimum_fee_minor": 500, "maximum_fee_minor": 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	feeProposalBody, err := json.Marshal(map[string]any{"fee_schedule": json.RawMessage(feeSchedule), "reason": "exercise fee-bearing funding"})
	if err != nil {
		t.Fatal(err)
	}
	feeCorrelation := "pricing-fee-" + institution
	feeProposalCode, feeProposal := proofHTTP(t, http.MethodPost, quoteAdmin+"/v1/operator/pricing/propose", "Bearer "+pricingMakerToken, "", feeCorrelation, string(feeProposalBody))
	if feeProposalCode != http.StatusCreated {
		t.Fatalf("fee schedule proposal: code=%d response=%v", feeProposalCode, feeProposal)
	}
	feeDecisionBody, err := json.Marshal(map[string]string{
		"proposal_id": stringField(t, feeProposal, "proposal_id"), "institution_id": institution,
		"reason": "approve fee-bearing integration fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := proofHTTP(t, http.MethodPost, quoteAdmin+"/v1/operator/pricing/approve", "Bearer "+pricingCheckerToken, "", feeCorrelation, string(feeDecisionBody)); code != http.StatusNoContent {
		t.Fatalf("fee schedule approval: code=%d", code)
	}
	feeRail := "sim_lp_1"
	if enableFees {
		feeRail = "flutterwave"
	}
	feeQuoteBody := fmt.Sprintf(`{"base_amount_minor":20000,"base_currency":"NGN","quote_currency":"NGN","destination_rail":%q,"customer_reference":"proof-fee-order"}`, feeRail)
	feeQuoteCode, feeQuote := call(http.MethodPost, api+"/v1/quotes", clientAuth, "quote-fee-1", feeQuoteBody)
	if feeQuoteCode != http.StatusOK || feeQuote["source_principal_minor"] != float64(19500) ||
		feeQuote["fee_minor"] != float64(500) || feeQuote["total_debit_minor"] != float64(20000) {
		t.Fatalf("fee-bearing quote: code=%d response=%v", feeQuoteCode, feeQuote)
	}
	feeQuoteID := stringField(t, feeQuote, "quote_id")
	feeAcceptBody := `{}`
	if enableFees {
		feeAcceptBody = acceptBody
	}
	feeAcceptCode, feeAcceptance := call(http.MethodPost, api+"/v1/quotes/"+feeQuoteID+"/accept", clientAuth, "accept-fee-1", feeAcceptBody)
	if feeAcceptCode != http.StatusOK || feeAcceptance["trade_id"] == "" {
		t.Fatalf("fee-bearing quote acceptance: code=%d response=%v", feeAcceptCode, feeAcceptance)
	}
	feeTradeID := stringField(t, feeAcceptance, "trade_id")
	feeTradeCode, feeTrade := call(http.MethodGet, api+"/v1/trades/"+feeTradeID, clientAuth, "", "")
	if feeTradeCode != http.StatusOK || feeTrade["status"] != "FUNDING_REQUIRED" {
		t.Fatalf("unfunded fee-bearing trade must remain recoverable: code=%d trade=%v", feeTradeCode, feeTrade)
	}
	if replayCode, replay := call(http.MethodPost, api+"/v1/quotes/"+feeQuoteID+"/accept", clientAuth, "accept-fee-1", feeAcceptBody); replayCode != http.StatusOK || replay["trade_id"] != feeTradeID {
		t.Fatalf("fee acceptance replay changed identity: code=%d response=%v", replayCode, replay)
	}
	if _, err = ledgerClient.PostLocalPrefund(fixtureContext, &ledgerv1.LocalPrefundCommand{
		InstitutionId: institution, Amount: &ledgerv1.Money{AmountMinor: 9_999, Currency: "NGN"},
		EvidenceReference: "bank_fee_short_" + randomHex(t, 6), CorrelationId: correlation,
	}); err != nil {
		t.Fatal(err)
	}
	// Replaying a completed edge request returns its original receipt. A new
	// funding attempt uses a new request key; Quote retains the original trade.
	if code, response := call(http.MethodPost, api+"/v1/quotes/"+feeQuoteID+"/accept", clientAuth, "accept-fee-after-short-prefund", feeAcceptBody); code != http.StatusOK || response["trade_id"] != feeTradeID {
		t.Fatalf("insufficient fee funding retry lost trade identity: code=%d response=%v", code, response)
	}
	if proofCount(db, `SELECT count(*) FROM ledger_funding_reservations WHERE trade_id=$1`, feeTradeID) != 0 {
		t.Fatal("insufficient all-in fee debit created a partial Ledger hold")
	}
	if _, err = ledgerClient.PostLocalPrefund(fixtureContext, &ledgerv1.LocalPrefundCommand{
		InstitutionId: institution, Amount: &ledgerv1.Money{AmountMinor: 1, Currency: "NGN"},
		EvidenceReference: "bank_fee_remainder_" + randomHex(t, 6), CorrelationId: correlation,
	}); err != nil {
		t.Fatal(err)
	}
	if enableFees {
		provider.mu.Lock()
		provider.status = "SUCCESSFUL"
		provider.mu.Unlock()
	}
	if code, response := call(http.MethodPost, api+"/v1/quotes/"+feeQuoteID+"/accept", clientAuth, "accept-fee-after-exact-prefund", feeAcceptBody); code != http.StatusOK || response["trade_id"] != feeTradeID {
		t.Fatalf("fee-bearing funding did not recover after exact prefund: code=%d response=%v", code, response)
	}

	if enableFees {
		waitFor(t, 30*time.Second, func() bool {
			return proofCount(db, `SELECT count(*) FROM ledger_funding_reservations WHERE trade_id=$1 AND state='CONSUMED'`, feeTradeID) == 1
		})
		waitFor(t, 20*time.Second, func() bool {
			return proofCount(db, `SELECT count(*) FROM trades WHERE id=$1 AND status='SETTLED'`, feeTradeID) == 1
		})
		var earned, held int64
		if err := db.QueryRow(`SELECT COALESCE(sum(CASE WHEN e.account_name='SANDBOX_SERVICE_FEE' THEN e.credit-e.debit ELSE 0 END),0),COALESCE(sum(CASE WHEN e.account_code='2100' THEN e.credit-e.debit ELSE 0 END),0) FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE j.trade_id=$1`, feeTradeID).Scan(&earned, &held); err != nil {
			t.Fatal(err)
		}
		if earned != 500 || held != 0 || provider.postCount() != 5 || proofCount(db, `SELECT count(*) FROM ledger_fee_finality WHERE trade_id=$1`, feeTradeID) != 1 {
			t.Fatalf("fee finality incorrect earned=%d held=%d posts=%d", earned, held, provider.postCount())
		}
		// Replay through the full customer API after service restart must not repay.
		runtime.stop("ledger")
		runtime.start("ledger", "./cmd/ledger-server", ports["ledger"])
		if code, response := call(http.MethodPost, api+"/v1/quotes/"+feeQuoteID+"/accept", clientAuth, "accept-fee-after-ledger-restart", feeAcceptBody); code != http.StatusOK || response["trade_id"] != feeTradeID {
			t.Fatalf("fee replay failed: code=%d response=%v", code, response)
		}
		if provider.postCount() != 5 || proofCount(db, `SELECT count(*) FROM ledger_fee_finality WHERE trade_id=$1`, feeTradeID) != 1 {
			t.Fatal("fee restart replay duplicated effect")
		}
		_, balances := call(http.MethodGet, api+"/v1/balances", clientAuth, "", "")
		ngn := balanceForCurrency(t, balances, "NGN")
		if ngn["available_minor"] != float64(0) || ngn["held_minor"] != float64(0) {
			t.Fatalf("fee customer balances wrong: %v", ngn)
		}
		t.Logf("FEE_APPLICATION trade=%s principal=19500 fee=500 reserved=20000 earned=500 held=0 provider_posts=5 restart_replay=one_effect", feeTradeID)
		return
	}
	feeTradeCode, feeTrade = call(http.MethodGet, api+"/v1/trades/"+feeTradeID, clientAuth, "", "")
	pricingSnapshot, snapshotOK := feeTrade["pricing_snapshot"].(map[string]any)
	if feeTradeCode != http.StatusOK || feeTrade["status"] != "FEE_ACCOUNTING_REQUIRED" || !snapshotOK ||
		pricingSnapshot["source_principal_minor"] != float64(19_500) ||
		pricingSnapshot["fee_minor"] != float64(500) || pricingSnapshot["total_debit_minor"] != float64(20_000) {
		t.Fatalf("funded trade did not expose immutable fee terms or remain payout-gated: code=%d trade=%v", feeTradeCode, feeTrade)
	}
	var reservedPrincipal, reservedFee int64
	var pricingHash, reservationState string
	if err = db.QueryRow(`SELECT source_principal_minor,fee_minor,pricing_hash,state FROM ledger_funding_reservations WHERE trade_id=$1`, feeTradeID).Scan(&reservedPrincipal, &reservedFee, &pricingHash, &reservationState); err != nil {
		t.Fatal(err)
	}
	if reservedPrincipal != 19_500 || reservedFee != 500 || len(pricingHash) != 64 || reservationState != "ACTIVE" ||
		proofCount(db, `SELECT count(*) FROM settlement_instructions WHERE trade_id=$1`, feeTradeID) != 0 ||
		proofCount(db, `SELECT count(*) FROM ledger_journals WHERE dedup_key=$1`, "hold:"+feeTradeID) != 1 {
		t.Fatalf("Ledger did not atomically reserve the exact all-in debit or trade reached payout: principal=%d fee=%d hash=%q state=%s", reservedPrincipal, reservedFee, pricingHash, reservationState)
	}
	feeBalancesCode, feeBalances := call(http.MethodGet, api+"/v1/balances", clientAuth, "", "")
	feeNGN := balanceForCurrency(t, feeBalances, "NGN")
	if feeBalancesCode != http.StatusOK || feeNGN["available_minor"] != float64(0) || feeNGN["held_minor"] != float64(20_000) || provider.postCount() != 4 {
		t.Fatalf("all-in fee hold or fail-closed payout gate incorrect: code=%d balance=%v provider_posts=%d", feeBalancesCode, feeBalances, provider.postCount())
	}
	t.Logf("FEE_FUNDING trade=%s principal=19500 fee=500 reserved=20000 state=FEE_ACCOUNTING_REQUIRED payout=blocked", feeTradeID)
}

type fakeTransferProvider struct {
	mu              sync.Mutex
	status          string
	ref             string
	posts           int
	createResponses int
	id              int64
	createdAt       string
	statementAmount int64
	transferAmount  int64
	holdCreate      time.Duration
	healthDown      bool
}

func (p *fakeTransferProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(r.URL.Path, "/v3/transfers") {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	healthDown := p.healthDown
	p.mu.Unlock()
	if healthDown && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v3/transfers" {
		var body struct {
			Reference string `json:"reference"`
			Account   string `json:"account_number"`
			Bank      string `json:"account_bank"`
			Amount    int64  `json:"amount"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Reference == "" || body.Account != "0690000040" || body.Bank != "044" || body.Amount <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.posts++
		p.ref, p.id = body.Reference, 837460+int64(p.posts)
		p.createdAt, p.statementAmount, p.transferAmount = time.Now().UTC().Format(time.RFC3339), body.Amount, body.Amount
		hold := p.holdCreate
		p.mu.Unlock()
		if hold > 0 {
			time.Sleep(hold)
		}
		p.mu.Lock()
		p.createResponses++
		p.mu.Unlock()
	}
	p.mu.Lock()
	amount := p.transferAmount
	if amount == 0 {
		amount = 100
	}
	if r.URL.Query().Get("from") != "" {
		amount = p.statementAmount
	}
	data := fmt.Sprintf(`{"id":%d,"reference":%q,"status":%q,"amount":%d,"fee":0,"currency":"NGN","account_number":"0690000040","created_at":%q}`, p.id, p.ref, p.status, amount, p.createdAt)
	ref := p.ref
	p.mu.Unlock()
	if r.Method == http.MethodGet && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/v3/transfers"), "/") {
		if ref == "" || (r.URL.Query().Get("reference") != "" && r.URL.Query().Get("reference") != ref) {
			_, _ = io.WriteString(w, `{"status":"success","data":[]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":[%s]}`, data)
		return
	}
	_, _ = fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
}

func (p *fakeTransferProvider) postCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.posts }
func (p *fakeTransferProvider) createResponseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createResponses
}
func (p *fakeTransferProvider) reference() string { p.mu.Lock(); defer p.mu.Unlock(); return p.ref }
func (p *fakeTransferProvider) providerRef() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprint(p.id)
}
func (p *fakeTransferProvider) setStatus(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = value
}
func (p *fakeTransferProvider) setStatementAmount(value int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statementAmount = value
}
func (p *fakeTransferProvider) setHoldCreate(value time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holdCreate = value
}
func (p *fakeTransferProvider) setHealthDown(value bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.healthDown = value
}

type proofRuntime struct {
	t         *testing.T
	dir       string
	env       map[string]string
	binaries  map[string]string
	processes map[string]*exec.Cmd
}

type customerWebhookProbe struct {
	mu                sync.Mutex
	secret            string
	attempts          int
	invalidSignatures bool
}

func (p *customerWebhookProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	valid := false
	if err == nil {
		valid, err = webhookcore.VerifyCustomerPayload(p.secret, r.Header.Get("X-Osai-Event-Id"), r.Header.Get("X-Osai-Timestamp"), body, r.Header.Get("X-Osai-Signature"))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if err != nil || !valid || r.Header.Get("X-Osai-Event-Id") == "" || r.Header.Get("X-Osai-Event-Type") != "quote.accepted" {
		p.invalidSignatures = true
	}
	if p.attempts == 1 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (p *customerWebhookProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

func (p *customerWebhookProbe) invalidSignature() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.invalidSignatures
}

func newProofRuntime(t *testing.T, env map[string]string) *proofRuntime {
	r := &proofRuntime{t: t, dir: t.TempDir(), env: env, binaries: map[string]string{}, processes: map[string]*exec.Cmd{}}
	t.Cleanup(func() {
		for name := range r.processes {
			r.stop(name)
		}
	})
	return r
}

func (r *proofRuntime) start(name, pkg, addr string) {
	binary := r.binaries[pkg]
	if binary == "" {
		binary = filepath.Join(r.dir, strings.ReplaceAll(strings.TrimPrefix(pkg, "./"), "/", "_")+".exe")
		cmd := exec.Command("go", "build", "-p", "1", "-o", binary, pkg)
		cmd.Dir = ".."
		if output, err := cmd.CombinedOutput(); err != nil {
			r.t.Fatalf("build %s: %v %s", pkg, err, output)
		}
		r.binaries[pkg] = binary
	}
	cmd := exec.Command(binary)
	cmd.Dir = ".."
	cmd.Env = os.Environ()
	for key, value := range r.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	logFile, err := os.Create(filepath.Join(r.dir, name+".log"))
	if err != nil {
		r.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		r.t.Fatalf("start %s: %v", name, err)
	}
	_ = logFile.Close()
	r.processes[name] = cmd
	if addr != "" {
		ready := false
		until := time.Now().Add(75 * time.Second)
		for time.Now().Before(until) {
			c, e := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if e == nil {
				_ = c.Close()
				ready = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ready {
			logText, _ := os.ReadFile(filepath.Join(r.dir, name+".log"))
			r.t.Fatalf("service %s failed to listen on %s: %s", name, addr, strings.ReplaceAll(string(logText), "0690000040", "[redacted]"))
		}
	}
	r.t.Logf("SERVICE %s pid=%d port=%s health=started", name, cmd.Process.Pid, addr)
}

func (r *proofRuntime) stop(name string) {
	cmd := r.processes[name]
	if cmd == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	delete(r.processes, name)
}

func randomHex(t *testing.T, bytesCount int) string {
	t.Helper()
	b := make([]byte, bytesCount)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
func loopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}
func waitFor(t *testing.T, duration time.Duration, ready func() bool) {
	t.Helper()
	until := time.Now().Add(duration)
	for time.Now().Before(until) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("runtime condition did not become ready before timeout")
}
func proofCount(db *sql.DB, query string, args ...any) int {
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		return -1
	}
	return count
}
func stringField(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	v, ok := row[key].(string)
	if !ok || v == "" {
		t.Fatalf("missing %s in %v", key, row)
	}
	return v
}

func proofHTTP(t *testing.T, method, endpoint, auth, key, correlation, body string) (int, map[string]any) {
	t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		req, err := http.NewRequest(method, endpoint, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req.Header.Set("X-Osai-Correlation-Id", correlation)
		req.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, endpoint, err)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			response.Body.Close()
			t.Log("RATE_LIMIT: retrying throttled proof request after one second")
			time.Sleep(time.Second)
			continue
		}
		defer response.Body.Close()
		var payload map[string]any
		_ = json.NewDecoder(response.Body).Decode(&payload)
		return response.StatusCode, payload
	}
	t.Fatal("proof request remained throttled after bounded retries")
	return 0, nil
}

func balanceForCurrency(t *testing.T, response map[string]any, currency string) map[string]any {
	t.Helper()
	balances, ok := response["balances"].([]any)
	if !ok {
		return nil
	}
	for _, raw := range balances {
		balance, ok := raw.(map[string]any)
		if ok && balance["currency"] == currency {
			return balance
		}
	}
	return nil
}
