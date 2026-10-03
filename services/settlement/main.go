package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/postgres"
	"github.com/osai/osai/pkg/provider"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"github.com/osai/osai/services/settlement/settlementstore"
	"github.com/osai/osai/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func main() {
	if err := observability.Init("settlement"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	db, err := postgres.Open("settlement")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := settlementstore.EnsureSchema(db); err != nil {
		log.Fatal(err)
	}
	repository := settlementstore.Postgres{DB: db}
	store, err := settlementcore.NewStoreWithPersistence(nil, repository)
	if err != nil {
		log.Fatal(err)
	}
	var temporalClient client.Client
	flutterwaveEnabled := os.Getenv("FLW_SETTLEMENT_ENABLED") == "true"
	if flutterwaveEnabled || os.Getenv("FLW_SECRET_KEY") != "" {
		if os.Getenv("FLW_SECRET_KEY") == "" || (flutterwaveEnabled && os.Getenv("FLW_SECRET_HASH") == "") || (os.Getenv("FLW_ENV") != "sandbox" && os.Getenv("FLW_ENV") != "test") {
			log.Fatal("Flutterwave sandbox settlement configuration unavailable")
		}
		if os.Getenv("FLW_ENV") == "sandbox" && !strings.HasPrefix(os.Getenv("FLW_SECRET_KEY"), "FLWSECK_TEST-") {
			log.Fatal("Flutterwave sandbox credential unavailable")
		}
		addr := os.Getenv("OSAI_TEMPORAL_ADDR")
		if addr == "" {
			addr = "localhost:7233"
		}
		temporalClient, err = client.Dial(client.Options{HostPort: addr})
		if err != nil {
			log.Fatal(err)
		}
		defer temporalClient.Close()
	}
	grpcPort := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if grpcPort == "" {
		grpcPort = ":50055"
	}
	listener, err := net.Listen("tcp", grpcPort)
	if err != nil {
		log.Fatalf("settlement gRPC listen failed: %v", err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
	customerAddr := os.Getenv("OSAI_CUSTOMER_GRPC_ADDR")
	if customerAddr == "" {
		customerAddr = "localhost:50052"
	}
	customerConn, err := grpc.Dial(customerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal("customer beneficiary service unavailable")
	}
	defer customerConn.Close()
	selector := settlementRouteSelector{Routes: map[string]settlementRoute{}}
	if flutterwaveEnabled {
		attempts := flutterwave.PostgresAttempts{DB: db}
		if err := attempts.EnsureSchema(); err != nil {
			log.Fatal(err)
		}
		timeout := 8 * time.Second
		if raw := os.Getenv("FLW_TIMEOUT_MS"); raw != "" {
			ms, err := strconv.Atoi(raw)
			if err != nil || ms < 1000 || ms > 30000 {
				log.Fatal("invalid Flutterwave timeout")
			}
			timeout = time.Duration(ms) * time.Millisecond
		}
		rail := flutterwave.New(flutterwave.Config{BaseURL: os.Getenv("FLW_BASE_URL"), SecretKey: os.Getenv("FLW_SECRET_KEY"), SecretHash: os.Getenv("FLW_SECRET_HASH"), Env: os.Getenv("FLW_ENV"), Enabled: true, Attempts: attempts, Timeout: timeout})
		selector.Routes["flutterwave"] = settlementRoute{Config: provider.RouteConfig{Enabled: true, Environment: os.Getenv("FLW_ENV"), Currencies: []string{"NGN"}, Rails: []string{"bank_transfer"}, Timeout: timeout, KillSwitch: os.Getenv("FLW_KILL_SWITCH") == "true", Transfer: true, Reconciliation: true, AccountResolution: false}, Rail: rail}
	}
	settlementServer := &settlementGRPCServer{store: store, repository: repository, temporal: temporalClient, reconciliationToken: os.Getenv("OSAI_RECONCILIATION_TOKEN"), beneficiaries: customerv1.NewCustomerServiceClient(customerConn), selector: selector}
	settlementv1.RegisterSettlementServiceServer(server, settlementServer)
	if temporalClient != nil {
		for _, instruction := range settlementServer.pendingFlutterwave() {
			if err := settlementServer.ensureWorkflow(context.Background(), instruction); err != nil {
				log.Printf("Flutterwave workflow recovery unavailable for settlement %s: %v", instruction.ID, err)
			}
		}
	}
	log.Printf("settlement gRPC listening on %s", grpcPort)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "settlement"})
	})
	go func() {
		port := os.Getenv("OSAI_SETTLEMENT_PORT")
		if port == "" {
			port = ":8085"
		}
		log.Printf("settlement service listening on %s", port)
		if err := http.ListenAndServe(port, nil); err != nil {
			log.Printf("settlement http server failed: %v", err)
		}
	}()
	if err := server.Serve(listener); err != nil {
		log.Fatalf("settlement gRPC server failed: %v", err)
	}
}

type settlementGRPCServer struct {
	settlementv1.UnimplementedSettlementServiceServer
	store               *settlementcore.Store
	repository          settlementstore.Postgres
	temporal            client.Client
	reconciliationToken string
	beneficiaries       interface {
		GetApprovedBeneficiary(context.Context, *customerv1.BeneficiaryRequest, ...grpc.CallOption) (*customerv1.Beneficiary, error)
	}
	selector interface {
		Check(context.Context, string, string, string) error
	}
}

func (s *settlementGRPCServer) ListConfirmedSettlements(ctx context.Context, req *settlementv1.ListConfirmedSettlementsRequest) (*settlementv1.ListConfirmedSettlementsResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if s == nil || s.reconciliationToken == "" || !ok || len(md.Get("x-osai-reconciliation-token")) != 1 || subtle.ConstantTimeCompare([]byte(md.Get("x-osai-reconciliation-token")[0]), []byte(s.reconciliationToken)) != 1 {
		return nil, status.Error(codes.PermissionDenied, "reconciliation access denied")
	}
	if req == nil || req.ProviderId == "" || req.FromUnix <= 0 || req.ToUnix <= req.FromUnix || req.PageSize < 1 || req.PageSize > 100 {
		return nil, status.Error(codes.InvalidArgument, "invalid reconciliation request")
	}
	items, err := s.repository.ListConfirmedForReconciliation(ctx, req.ProviderId, time.Unix(req.FromUnix, 0), time.Unix(req.ToUnix, 0), req.AfterSettlementId, int(req.PageSize))
	if err != nil {
		return nil, status.Error(codes.Unavailable, "settlement snapshot unavailable")
	}
	response := &settlementv1.ListConfirmedSettlementsResponse{}
	for _, item := range items {
		response.Settlements = append(response.Settlements, &settlementv1.SettlementResponse{SettlementId: item.ID, InstitutionId: item.InstitutionID, TradeId: item.TradeID, QuoteId: item.QuoteID, Status: string(item.State), Beneficiary: item.Beneficiary, AmountMinor: item.AmountMinor, Currency: item.Currency, CorrelationId: item.CorrelationID, ProviderId: item.ProviderID, ClientRef: item.ClientRef, ProviderRef: item.ProviderRef})
	}
	if len(items) == int(req.PageSize) {
		response.NextSettlementId = items[len(items)-1].ID
	}
	return response, nil
}

func (s *settlementGRPCServer) CreateSettlementForTrade(ctx context.Context, req *settlementv1.CreateSettlementForTradeRequest) (*settlementv1.SettlementResponse, error) {
	if s == nil || s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	if strings.TrimSpace(req.TradeId) == "" {
		return nil, status.Error(codes.InvalidArgument, "trade_id required")
	}
	providerID := req.ProviderId
	if providerID == "" {
		providerID = "sim_lp_1"
	}
	if providerID != "sim_lp_1" && providerID != "flutterwave" {
		return nil, status.Error(codes.InvalidArgument, "unsupported settlement provider")
	}
	if providerID == "flutterwave" {
		if os.Getenv("FLW_SETTLEMENT_ENABLED") != "true" || s.temporal == nil {
			return nil, status.Error(codes.FailedPrecondition, "Flutterwave settlement disabled")
		}
		if req.Amount == nil || req.Amount.Currency != "NGN" || req.Amount.AmountMinor <= 0 || req.Amount.AmountMinor%100 != 0 || req.BeneficiaryId == "" || req.BankCode != "" || req.AccountNumber != "" || s.beneficiaries == nil {
			return nil, status.Error(codes.InvalidArgument, "approved beneficiary and whole NGN amount required")
		}
	}
	amountMinor := int64(0)
	currency := ""
	if req.Amount != nil {
		amountMinor = req.Amount.AmountMinor
		currency = req.Amount.Currency
	}
	if amountMinor <= 0 {
		amountMinor = 1
	}
	if strings.TrimSpace(currency) == "" {
		currency = "USD"
	}
	beneficiary, bankCode, accountNumber := req.Beneficiary, "", ""
	if providerID == "flutterwave" {
		resolved, err := s.beneficiaries.GetApprovedBeneficiary(ctx, &customerv1.BeneficiaryRequest{InstitutionId: req.InstitutionId, BeneficiaryId: req.BeneficiaryId})
		if err != nil || resolved == nil || resolved.InstitutionId != req.InstitutionId || resolved.BeneficiaryId != req.BeneficiaryId || resolved.Status != "ACTIVE" || resolved.ApprovalStatus != "APPROVED" || !bankDigits(resolved.BankCode, 3, 6) || !bankDigits(resolved.AccountNumber, 10, 10) {
			return nil, status.Error(codes.PermissionDenied, "beneficiary unavailable")
		}
		beneficiary, bankCode, accountNumber = resolved.AccountNumber, resolved.BankCode, resolved.AccountNumber
		if s.selector == nil || s.selector.Check(ctx, providerID, currency, "bank_transfer") != nil {
			return nil, status.Error(codes.FailedPrecondition, "settlement provider unavailable")
		}
	}
	instruction, err := s.repository.CreateForTrade(ctx, req.InstitutionId, req.TradeId, req.QuoteId, req.CorrelationId, beneficiary, currency, amountMinor, req.Purpose, providerID, bankCode, accountNumber)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.store.Import(instruction)
	if providerID == "flutterwave" {
		if err := s.ensureWorkflow(ctx, instruction); err != nil {
			return nil, status.Error(codes.Unavailable, "settlement workflow unavailable")
		}
	}
	log.Printf("settlement instruction created: settlement_id=%s trade_id=%s quote_id=%s institution_id=%s correlation_id=%s", instruction.ID, req.TradeId, req.QuoteId, req.InstitutionId, req.CorrelationId)
	return &settlementv1.SettlementResponse{
		SettlementId:  instruction.ID,
		InstitutionId: req.InstitutionId,
		TradeId:       req.TradeId,
		QuoteId:       req.QuoteId,
		Status:        string(instruction.State),
		Beneficiary:   instruction.Beneficiary,
		AmountMinor:   instruction.AmountMinor,
		Currency:      instruction.Currency,
		CorrelationId: req.CorrelationId,
		ProviderId:    providerID,
		ClientRef:     instruction.ClientRef,
		ProviderRef:   instruction.ProviderRef,
	}, nil
}

func (s *settlementGRPCServer) GetSettlement(ctx context.Context, req *settlementv1.GetSettlementRequest) (*settlementv1.SettlementResponse, error) {
	if s == nil || s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	if strings.TrimSpace(req.SettlementId) == "" {
		return nil, status.Error(codes.InvalidArgument, "settlement_id required")
	}
	instruction, err := s.repository.GetByID(ctx, req.SettlementId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "settlement not found")
	}
	if strings.TrimSpace(instruction.InstitutionID) != "" && instruction.InstitutionID != req.InstitutionId {
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	return &settlementv1.SettlementResponse{
		SettlementId:  instruction.ID,
		InstitutionId: req.InstitutionId,
		TradeId:       instruction.TradeID,
		QuoteId:       instruction.QuoteID,
		Status:        string(instruction.State),
		Beneficiary:   instruction.Beneficiary,
		AmountMinor:   instruction.AmountMinor,
		Currency:      instruction.Currency,
		CorrelationId: instruction.CorrelationID,
		ProviderId:    instruction.ProviderID,
		ClientRef:     instruction.ClientRef,
		ProviderRef:   instruction.ProviderRef,
	}, nil
}

func bankDigits(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func (s *settlementGRPCServer) ensureWorkflow(ctx context.Context, instruction settlementcore.Instruction) error {
	if s.temporal == nil {
		return errors.New("Temporal settlement unavailable")
	}
	interval := time.Duration(0)
	if os.Getenv("FLW_ENV") == "test" {
		interval = time.Second
	}
	_, err := s.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "settlement-" + instruction.ClientRef, TaskQueue: workflows.TaskQueue}, workflows.TemporalSettlementWorkflow, workflows.TemporalSettlementInput{
		InstructionID: instruction.ID, ClientRef: instruction.ClientRef, ProviderID: instruction.ProviderID, BankCode: instruction.BankCode, AccountNumber: instruction.AccountNumber, Beneficiary: instruction.Beneficiary, AmountMinor: instruction.AmountMinor, Currency: instruction.Currency, Purpose: instruction.Purpose, PollInterval: interval,
	})
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil
	}
	return err
}

func (s *settlementGRPCServer) pendingFlutterwave() []settlementcore.Instruction {
	items, err := s.repository.LoadInstructions()
	if err != nil {
		log.Printf("Flutterwave settlement recovery scan unavailable: %v", err)
		return nil
	}
	pending := make([]settlementcore.Instruction, 0)
	for _, item := range items {
		if item.ProviderID == "flutterwave" && !item.State.Terminal() && item.State != settlementcore.ManualReview {
			pending = append(pending, item)
		}
	}
	return pending
}

func init() { fmt.Println("settlement service initialized") }
