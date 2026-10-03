package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/provider"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"github.com/osai/osai/services/ledger/ledgergrpc"
	"github.com/osai/osai/services/settlement/ledgerclient"
	"github.com/osai/osai/services/settlement/settlementcore"
	"github.com/osai/osai/services/settlement/settlementstore"
	"github.com/osai/osai/workflows"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type preflightBeneficiaries struct{}

func (preflightBeneficiaries) GetApprovedBeneficiary(_ context.Context, req *customerv1.BeneficiaryRequest, _ ...grpc.CallOption) (*customerv1.Beneficiary, error) {
	if req.InstitutionId != "inst_sandbox" || req.BeneficiaryId != "ben_sandbox" {
		return nil, fmt.Errorf("beneficiary not found")
	}
	return &customerv1.Beneficiary{BeneficiaryId: req.BeneficiaryId, InstitutionId: req.InstitutionId, Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, nil
}

func TestFlutterwaveLocalPreflight(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	addr := os.Getenv("OSAI_TEMPORAL_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("local PostgreSQL and Temporal configuration required")
	}
	t.Setenv("FLW_ENV", "test")
	t.Setenv("FLW_SETTLEMENT_ENABLED", "true")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		t.Skip("local PostgreSQL unavailable")
	}
	if err = settlementstore.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	attempts := flutterwave.PostgresAttempts{DB: db}
	if err = attempts.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	temporalClient, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Skip("local Temporal unavailable")
	}
	defer temporalClient.Close()
	binary := filepath.Join(t.TempDir(), "notification-webhook.exe")
	if err := exec.Command("go", "build", "-o", binary, "github.com/osai/osai/services/notification-webhook").Run(); err != nil {
		t.Fatalf("notification service build failed: %v", err)
	}
	var webhookProcess *exec.Cmd
	stopWebhook := func() {
		if webhookProcess != nil {
			_ = webhookProcess.Process.Kill()
			_ = webhookProcess.Wait()
			webhookProcess = nil
		}
	}
	startWebhook := func() {
		webhookProcess = exec.Command(binary)
		webhookProcess.Env = append(os.Environ(), "OSAI_WEBHOOK_SECRET=local-only-outbound-test", "OSAI_NOTIFICATION_TOKEN=local-only-service-test", "FLW_SECRET_HASH=local-test-hash", "OSAI_NOTIFICATION_GRPC_ADDR=127.0.0.1:0", "OSAI_WEBHOOK_PORT=127.0.0.1:8083")
		if err := webhookProcess.Start(); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			connection, err := net.DialTimeout("tcp", "127.0.0.1:8083", 100*time.Millisecond)
			if err == nil {
				_ = connection.Close()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		stopWebhook()
		t.Fatal("notification service did not bind localhost:8083")
	}
	defer stopWebhook()
	var mu sync.Mutex
	ref := ""
	posts := 0
	providerStatus := "NEW"
	providerID := time.Now().UnixNano() % 1000000000
	var beforeCreateResponse func(int64, string)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		if r.Method == http.MethodPost {
			posts++
			providerID++
			var request struct {
				Reference string `json:"reference"`
				Amount    int64  `json:"amount"`
				Bank      string `json:"account_bank"`
				Account   string `json:"account_number"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Amount != 100 || request.Bank != "044" || request.Account != "0690000040" {
				t.Errorf("provider create request invalid")
			}
			ref = request.Reference
		}
		localID, localRef, localStatus, hook := providerID, ref, providerStatus, beforeCreateResponse
		mu.Unlock()
		if r.Method == http.MethodPost && hook != nil {
			hook(localID, localRef)
		}
		data := fmt.Sprintf(`{"id":%d,"reference":%q,"amount":100,"currency":"NGN","account_number":"0690000040","status":%q,"created_at":"2026-09-26T00:00:00Z"}`, localID, localRef, localStatus)
		if r.URL.Path == "/v3/transfers" && r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"status":"success","data":[%s]}`, data)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
	}))
	defer fake.Close()
	adapter := flutterwave.New(flutterwave.Config{BaseURL: fake.URL + "/v3", SecretKey: "local-test-key", SecretHash: "local-test-hash", Env: "test", Enabled: true, Attempts: attempts})
	ledger := ledgerapi.NewService()
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(grpcServer, &ledgergrpc.Server{Service: ledger})
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	repository := settlementstore.Postgres{DB: db}
	worker, err := workflows.StartSettlementWorker(context.Background(), temporalClient, &workflows.ProviderSettlementActivities{Rails: map[string]provider.SettlementRail{"flutterwave": adapter}, Ledger: ledgerclient.New(connection), Recorder: repository})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	store, err := settlementcore.NewStoreWithPersistence(nil, repository)
	if err != nil {
		t.Fatal(err)
	}
	server := &settlementGRPCServer{store: store, repository: repository, temporal: temporalClient, beneficiaries: preflightBeneficiaries{}, selector: settlementRouteSelector{Routes: map[string]settlementRoute{"flutterwave": {Config: provider.RouteConfig{Enabled: true, Environment: "test", Currencies: []string{"NGN"}, Rails: []string{"bank_transfer"}, Transfer: true}, Rail: adapter}}}}
	tradeID := "trd_flw_" + uuid.NewString()
	created, err := server.CreateSettlementForTrade(context.Background(), &settlementv1.CreateSettlementForTradeRequest{InstitutionId: "inst_sandbox", TradeId: tradeID, QuoteId: "quo_sandbox", CorrelationId: "corr_sandbox", ProviderId: "flutterwave", BeneficiaryId: "ben_sandbox", Amount: &settlementv1.Money{AmountMinor: 10000, Currency: "NGN"}, Purpose: "sandbox preflight"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := posts
		reference := ref
		mu.Unlock()
		if count == 1 && reference != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	count := posts
	reference := ref
	providerStatus = "SUCCESSFUL"
	mu.Unlock()
	if count != 1 || reference == "" {
		t.Fatal("provider fake did not receive exactly one create")
	}
	callback := fmt.Sprintf(`{"event":"transfer.completed","data":{"id":%d,"reference":%q,"amount":100,"currency":"NGN","status":"SUCCESSFUL","created_at":"2026-09-26T00:00:00Z"}}`, providerID, reference)
	startWebhook()
	send := func() int {
		request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:8083/v1/webhooks/providers/flutterwave", bytes.NewBufferString(callback))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Verif-Hash", "local-test-hash")
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if code := send(); code != http.StatusOK {
		t.Fatalf("matching callback returned %d", code)
	}
	for time.Now().Before(deadline) {
		if len(ledger.Journals()) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(ledger.Journals()) != 1 {
		t.Fatal("callback did not yield one verified ledger effect")
	}
	stopWebhook()
	startWebhook()
	if code := send(); code != http.StatusOK {
		t.Fatalf("duplicate after restart returned %d", code)
	}
	if len(ledger.Journals()) != 1 {
		t.Fatal("duplicate callback caused ledger effect")
	}
	got, err := repository.GetByID(context.Background(), created.SettlementId)
	if err != nil || got.State != settlementcore.Confirmed || got.ProviderRef != fmt.Sprint(providerID) || got.ClientRef != reference {
		t.Fatalf("settlement not persisted as confirmed: %+v %v", got, err)
	}
	finishedCtx, cancelFinished := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinished()
	if err := temporalClient.GetWorkflow(finishedCtx, "settlement-"+reference, "").Get(finishedCtx, nil); err != nil {
		t.Fatalf("workflow did not finish after ledger posting: %v", err)
	}
	mu.Lock()
	count = posts
	mu.Unlock()
	if count != 1 {
		t.Fatalf("provider received %d creates", count)
	}
	// A second transfer is allowed to settle by polling before its first
	// callback. That later callback must be acknowledged without another effect.
	second, err := server.CreateSettlementForTrade(context.Background(), &settlementv1.CreateSettlementForTradeRequest{InstitutionId: "inst_sandbox", TradeId: "trd_flw_" + uuid.NewString(), QuoteId: "quo_sandbox", CorrelationId: "corr_sandbox", ProviderId: "flutterwave", BeneficiaryId: "ben_sandbox", Amount: &settlementv1.Money{AmountMinor: 10000, Currency: "NGN"}, Purpose: "sandbox preflight"})
	if err != nil {
		t.Fatal(err)
	}
	secondDeadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(secondDeadline) {
		if len(ledger.Journals()) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(ledger.Journals()) != 2 {
		t.Fatal("polling did not produce second verified ledger effect")
	}
	mu.Lock()
	secondRef := ref
	secondID := providerID
	count = posts
	mu.Unlock()
	if count != 2 || secondRef == reference {
		t.Fatalf("second transfer missing or reused reference: posts=%d", count)
	}
	finishedSecond, cancelSecond := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSecond()
	if err := temporalClient.GetWorkflow(finishedSecond, "settlement-"+secondRef, "").Get(finishedSecond, nil); err != nil {
		t.Fatalf("second workflow did not finish: %v", err)
	}
	callback = fmt.Sprintf(`{"event":"transfer.completed","data":{"id":%d,"reference":%q,"amount":100,"currency":"NGN","status":"SUCCESSFUL","created_at":"2026-09-26T00:00:00Z"}}`, secondID, secondRef)
	if code := send(); code != http.StatusOK {
		t.Fatalf("late callback returned %d", code)
	}
	if code := send(); code != http.StatusOK {
		t.Fatalf("duplicate late callback returned %d", code)
	}
	if len(ledger.Journals()) != 2 {
		t.Fatal("late callback caused another ledger effect")
	}
	secondStored, err := repository.GetByID(context.Background(), second.SettlementId)
	if err != nil || secondStored.State != settlementcore.Confirmed {
		t.Fatalf("second settlement not confirmed: %+v %v", secondStored, err)
	}
	// The provider can send a callback while its create HTTP response is still
	// withheld. Temporal must retain the signal and verify status after create.
	earlyResult := make(chan int, 1)
	mu.Lock()
	beforeCreateResponse = func(id int64, reference string) {
		body := fmt.Sprintf(`{"event":"transfer.completed","data":{"id":%d,"reference":%q,"amount":100,"currency":"NGN","status":"SUCCESSFUL","created_at":"2026-09-26T00:00:00Z"}}`, id, reference)
		request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:8083/v1/webhooks/providers/flutterwave", bytes.NewBufferString(body))
		if err != nil {
			earlyResult <- 0
			return
		}
		request.Header.Set("Verif-Hash", "local-test-hash")
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			earlyResult <- 0
			return
		}
		_ = response.Body.Close()
		earlyResult <- response.StatusCode
	}
	mu.Unlock()
	third, err := server.CreateSettlementForTrade(context.Background(), &settlementv1.CreateSettlementForTradeRequest{InstitutionId: "inst_sandbox", TradeId: "trd_flw_" + uuid.NewString(), QuoteId: "quo_sandbox", CorrelationId: "corr_sandbox", ProviderId: "flutterwave", BeneficiaryId: "ben_sandbox", Amount: &settlementv1.Money{AmountMinor: 10000, Currency: "NGN"}, Purpose: "sandbox preflight"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-earlyResult:
		if code != http.StatusOK {
			t.Fatalf("callback before create response returned %d", code)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("early callback not delivered")
	}
	thirdDeadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(thirdDeadline) {
		if len(ledger.Journals()) == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(ledger.Journals()) != 3 {
		t.Fatal("early callback did not yield one verified third ledger effect")
	}
	thirdStored, err := repository.GetByID(context.Background(), third.SettlementId)
	if err != nil || thirdStored.State != settlementcore.Confirmed {
		t.Fatalf("third settlement not confirmed: %+v %v", thirdStored, err)
	}
	mu.Lock()
	count = posts
	mu.Unlock()
	if count != 3 {
		t.Fatalf("provider received %d creates for three settlements", count)
	}
}
