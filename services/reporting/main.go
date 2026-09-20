package main

import (
	"context"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osai/osai/pkg/observability"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type balanceStore struct {
	mu      sync.RWMutex
	balances map[string][]*reportingv1.Balance
	items   map[string][]*reportingv1.Transaction
}

func newBalanceStore() *balanceStore {
	return &balanceStore{
		balances: map[string][]*reportingv1.Balance{},
		items:    map[string][]*reportingv1.Transaction{},
	}
}

func (s *balanceStore) upsertBalance(instID, currency string, available, held int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, balance := range s.balances[instID] {
		if balance.Currency == currency {
			balance.AvailableMinor = available
			balance.HeldMinor = held
			balance.ReservedMinor = 0
			balance.TotalMinor = available + held
			return
		}
	}
	s.balances[instID] = append(s.balances[instID], &reportingv1.Balance{
		InstitutionId: instID,
		Currency:      currency,
		AvailableMinor: available,
		HeldMinor:     held,
		ReservedMinor: 0,
		TotalMinor:    available + held,
	})
}

func (s *balanceStore) addTransaction(item *reportingv1.Transaction) {
	if item == nil { return }
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]*reportingv1.Transaction{}, s.items[item.InstitutionId]...)
	list = append(list, item)
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].OccurredAt > list[j].OccurredAt
	})
	s.items[item.InstitutionId] = list
}

func (s *balanceStore) getBalances(instID string) []*reportingv1.Balance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	balances := s.balances[instID]
	result := make([]*reportingv1.Balance, 0, len(balances))
	for _, balance := range balances {
		result = append(result, &reportingv1.Balance{
			InstitutionId: balance.InstitutionId,
			Currency:      balance.Currency,
			AvailableMinor: balance.AvailableMinor,
			HeldMinor:     balance.HeldMinor,
			ReservedMinor: balance.ReservedMinor,
			TotalMinor:    balance.TotalMinor,
		})
	}
	return result
}

func (s *balanceStore) listTransactions(instID string, page, pageSize int) ([]*reportingv1.Transaction, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := s.items[instID]
	if page <= 0 { page = 1 }
	if pageSize <= 0 || pageSize > 100 { pageSize = 25 }
	start := (page - 1) * pageSize
	if start >= len(items) {
		return []*reportingv1.Transaction{}, len(items)
	}
	if start+pageSize > len(items) { pageSize = len(items) - start }
	result := make([]*reportingv1.Transaction, 0, pageSize)
	for _, item := range items[start : start+pageSize] {
		result = append(result, &reportingv1.Transaction{
			TransactionId: item.TransactionId,
			InstitutionId: item.InstitutionId,
			Type:          item.Type,
			AmountMinor:   item.AmountMinor,
			Currency:      item.Currency,
			Status:        item.Status,
			OccurredAt:    item.OccurredAt,
			CorrelationId: item.CorrelationId,
			TradeId:       item.TradeId,
			SettlementId:  item.SettlementId,
		})
	}
	return result, len(items)
}

type grpcServer struct {
	reportingv1.UnimplementedReportingServiceServer
	store *balanceStore
}

func (s *grpcServer) GetBalances(ctx context.Context, req *reportingv1.GetBalancesRequest) (*reportingv1.GetBalancesResponse, error) {
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id is required")
	}
	return &reportingv1.GetBalancesResponse{Balances: s.store.getBalances(req.InstitutionId)}, nil
}

func (s *grpcServer) ListTransactions(ctx context.Context, req *reportingv1.ListTransactionsRequest) (*reportingv1.ListTransactionsResponse, error) {
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id is required")
	}
	page := int(req.Page)
	pageSize := int(req.PageSize)
	if page <= 0 { page = 1 }
	if pageSize <= 0 || pageSize > 100 { pageSize = 25 }
	items, total := s.store.listTransactions(req.InstitutionId, page, pageSize)
	return &reportingv1.ListTransactionsResponse{Items: items, Page: int32(page), PageSize: int32(pageSize), Total: int32(total)}, nil
}

func main() {
	if err := observability.Init("reporting"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	store := newBalanceStore()
	store.upsertBalance("inst_sandbox_local", "NGN", 2500000, 500000)
	store.upsertBalance("inst_sandbox_local", "USD", 125000, 10000)
	store.addTransaction(&reportingv1.Transaction{TransactionId: "txn_seed_1", InstitutionId: "inst_sandbox_local", Type: "TRADE", AmountMinor: 45000, Currency: "NGN", Status: "POSTED", OccurredAt: time.Now().Add(-2 * time.Hour).Format(time.RFC3339), CorrelationId: "corr_seed_1", TradeId: "trd_seed_1"})
	store.addTransaction(&reportingv1.Transaction{TransactionId: "txn_seed_2", InstitutionId: "inst_sandbox_local", Type: "SETTLEMENT", AmountMinor: -25000, Currency: "NGN", Status: "CONFIRMED", OccurredAt: time.Now().Add(-1 * time.Hour).Format(time.RFC3339), CorrelationId: "corr_seed_2", SettlementId: "sett_seed_1"})
	grpcAddr := os.Getenv("OSAI_REPORTING_GRPC_ADDR")
	if grpcAddr == "" { grpcAddr = ":50056" }
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil { log.Fatalf("reporting gRPC listen failed: %v", err) }
	server := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
	reportingv1.RegisterReportingServiceServer(server, &grpcServer{store: store})
	log.Printf("reporting gRPC listening on %s", grpcAddr)
	if err := server.Serve(listener); err != nil && err != grpc.ErrServerStopped { log.Fatalf("reporting gRPC server failed: %v", err) }
}
