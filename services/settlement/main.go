package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/pkg/observability"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func main() {
	if err := observability.Init("settlement"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	store := settlementcore.NewStore(nil)
	grpcPort := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if grpcPort == "" {
		grpcPort = ":50055"
	}
	listener, err := net.Listen("tcp", grpcPort)
	if err != nil {
		log.Fatalf("settlement gRPC listen failed: %v", err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
	settlementv1.RegisterSettlementServiceServer(server, &settlementGRPCServer{store: store})
	log.Printf("settlement gRPC listening on %s", grpcPort)

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "settlement"})
	})
	http.HandleFunc("/v1/settlements", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Beneficiary string `json:"beneficiary"`
			Currency    string `json:"currency"`
			AmountMinor int64  `json:"amount_minor"`
			Purpose     string `json:"purpose"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		instruction, err := store.Create(req.Beneficiary, req.Currency, req.AmountMinor, req.Purpose)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "VALIDATION_ERROR", "message": err.Error()}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"settlement_id": instruction.ID, "state": string(instruction.State), "beneficiary": instruction.Beneficiary, "amount_minor": instruction.AmountMinor, "currency": instruction.Currency, "purpose": instruction.Purpose, "created_at": instruction.CreatedAt.UTC().Format(time.RFC3339)})
	})
	http.HandleFunc("/v1/settlements/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/settlements/")
		if path == "" || strings.Contains(path, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		instruction, ok := store.Get(path)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"settlement_id": instruction.ID, "state": string(instruction.State), "beneficiary": instruction.Beneficiary, "amount_minor": instruction.AmountMinor, "currency": instruction.Currency, "purpose": instruction.Purpose, "created_at": instruction.CreatedAt.UTC().Format(time.RFC3339)})
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
	store *settlementcore.Store
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
	instruction, err := s.store.CreateWithInstitution(req.InstitutionId, req.Beneficiary, currency, amountMinor, req.Purpose)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
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
	instruction, ok := s.store.Get(req.SettlementId)
	if !ok {
		return nil, status.Error(codes.NotFound, "settlement not found")
	}
	if strings.TrimSpace(instruction.InstitutionID) != "" && instruction.InstitutionID != req.InstitutionId {
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	return &settlementv1.SettlementResponse{
		SettlementId:  instruction.ID,
		InstitutionId: req.InstitutionId,
		TradeId:       "",
		QuoteId:       "",
		Status:        string(instruction.State),
		Beneficiary:   instruction.Beneficiary,
		AmountMinor:   instruction.AmountMinor,
		Currency:      instruction.Currency,
		CorrelationId: req.CorrelationId,
	}, nil
}

func init() { fmt.Println("settlement service initialized") }
