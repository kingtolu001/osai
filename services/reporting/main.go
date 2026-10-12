package main

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/pkg/observability"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type grpcServer struct {
	reportingv1.UnimplementedReportingServiceServer
	ledgerClient ledgerv1.LedgerServiceClient
}

func (s *grpcServer) GetBalances(ctx context.Context, req *reportingv1.GetBalancesRequest) (*reportingv1.GetBalancesResponse, error) {
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id is required")
	}
	if s.ledgerClient == nil {
		return nil, status.Error(codes.Unavailable, "ledger read service unavailable")
	}
	result, err := s.ledgerClient.GetCustomerBalances(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: req.InstitutionId})
	if err != nil {
		return nil, err
	}
	response := &reportingv1.GetBalancesResponse{Balances: make([]*reportingv1.Balance, 0, len(result.Balances))}
	for _, balance := range result.Balances {
		response.Balances = append(response.Balances, &reportingv1.Balance{
			InstitutionId:  req.InstitutionId,
			Currency:       balance.Currency,
			AvailableMinor: balance.AvailableMinor,
			HeldMinor:      balance.HeldMinor,
			TotalMinor:     balance.AvailableMinor + balance.HeldMinor,
		})
	}
	return response, nil
}

func (s *grpcServer) ListTransactions(ctx context.Context, req *reportingv1.ListTransactionsRequest) (*reportingv1.ListTransactionsResponse, error) {
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id is required")
	}
	if s.ledgerClient == nil {
		return nil, status.Error(codes.Unavailable, "ledger read service unavailable")
	}
	page, pageSize := req.Page, req.PageSize
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 25
	}
	result, err := s.ledgerClient.ListCustomerTransactions(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: req.InstitutionId, Page: page, PageSize: pageSize})
	if err != nil {
		return nil, err
	}
	response := &reportingv1.ListTransactionsResponse{Page: page, PageSize: pageSize, Total: result.Total, Items: make([]*reportingv1.Transaction, 0, len(result.Items))}
	for _, item := range result.Items {
		response.Items = append(response.Items, &reportingv1.Transaction{
			TransactionId: item.JournalId,
			InstitutionId: req.InstitutionId,
			Type:          "LEDGER",
			AmountMinor:   item.AmountMinor,
			Currency:      item.Currency,
			Status:        "POSTED",
			OccurredAt:    item.OccurredAt,
			TradeId:       item.TradeId,
		})
	}
	return response, nil
}

func main() {
	if err := observability.Init("reporting"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	ledgerAddr := strings.TrimSpace(os.Getenv("OSAI_LEDGER_GRPC_ADDR"))
	if ledgerAddr == "" {
		log.Fatal("OSAI_LEDGER_GRPC_ADDR is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ledgerConn, err := grpc.DialContext(ctx, ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		log.Fatalf("dial ledger service: %v", err)
	}
	defer ledgerConn.Close()

	grpcAddr := os.Getenv("OSAI_REPORTING_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":50056"
	}
	listener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("reporting gRPC listen failed: %v", err)
	}
	server := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	reportingv1.RegisterReportingServiceServer(server, &grpcServer{ledgerClient: ledgerv1.NewLedgerServiceClient(ledgerConn)})
	log.Printf("reporting gRPC listening on %s", grpcAddr)
	if err := server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
		log.Fatalf("reporting gRPC server failed: %v", err)
	}
}
