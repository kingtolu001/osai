package main

import (
	"context"
	"github.com/osai/osai/pkg/observability"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type grpcServer struct {
	reportingv1.UnimplementedReportingServiceServer
	ledger ledgerv1.LedgerServiceClient
}

func (s *grpcServer) GetBalances(ctx context.Context, req *reportingv1.GetBalancesRequest) (*reportingv1.GetBalancesResponse, error) {
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	resp, err := s.ledger.GetCustomerBalances(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: req.InstitutionId})
	if err != nil {
		return nil, err
	}
	out := &reportingv1.GetBalancesResponse{}
	for _, item := range resp.Balances {
		out.Balances = append(out.Balances, &reportingv1.Balance{InstitutionId: req.InstitutionId, Currency: item.Currency, AvailableMinor: item.AvailableMinor, HeldMinor: item.HeldMinor, TotalMinor: item.AvailableMinor + item.HeldMinor})
	}
	return out, nil
}
func (s *grpcServer) ListTransactions(ctx context.Context, req *reportingv1.ListTransactionsRequest) (*reportingv1.ListTransactionsResponse, error) {
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	page, size := req.Page, req.PageSize
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 25
	}
	resp, err := s.ledger.ListCustomerTransactions(ctx, &ledgerv1.CustomerReadRequest{InstitutionId: req.InstitutionId, Page: page, PageSize: size})
	if err != nil {
		return nil, err
	}
	out := &reportingv1.ListTransactionsResponse{Page: page, PageSize: size, Total: resp.Total}
	for _, item := range resp.Items {
		out.Items = append(out.Items, &reportingv1.Transaction{TransactionId: item.JournalId, InstitutionId: req.InstitutionId, Type: "LEDGER_JOURNAL", AmountMinor: item.AmountMinor, Currency: item.Currency, Status: "POSTED", OccurredAt: item.OccurredAt, TradeId: item.TradeId})
	}
	return out, nil
}
func main() {
	if err := observability.Init("reporting"); err != nil {
		log.Printf("otel init warning: %v", err)
	}
	ledgerAddr := os.Getenv("OSAI_LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = "localhost:50051"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	addr := os.Getenv("OSAI_REPORTING_GRPC_ADDR")
	if addr == "" {
		addr = ":50056"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	server := grpc.NewServer(grpc.UnaryInterceptor(observability.UnaryServerInterceptor()))
	reportingv1.RegisterReportingServiceServer(server, &grpcServer{ledger: ledgerv1.NewLedgerServiceClient(conn)})
	log.Printf("reporting gRPC listening on %s", addr)
	log.Fatal(server.Serve(listener))
}
