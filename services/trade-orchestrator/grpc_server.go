package main

import (
	"context"
	"log"
	"strings"
	"time"

	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type tradeServer struct {
	tradev1.UnimplementedTradeServiceServer
	store *tradecore.Store
	settlementClient settlementv1.SettlementServiceClient
}

func newTradeServer(store *tradecore.Store, settlementClient settlementv1.SettlementServiceClient) *tradeServer {
	if store == nil { store = tradecore.NewStore() }
	return &tradeServer{store: store, settlementClient: settlementClient}
}

func (s *tradeServer) CreateTradeFromAcceptedQuote(ctx context.Context, req *tradev1.CreateTradeFromAcceptedQuoteRequest) (*tradev1.TradeResponse, error) {
	if s == nil || s.store == nil || req == nil { return nil, status.Error(codes.InvalidArgument, "request required") }
	if strings.TrimSpace(req.InstitutionId) == "" { return nil, status.Error(codes.InvalidArgument, "institution_id required") }
	if strings.TrimSpace(req.QuoteId) == "" { return nil, status.Error(codes.InvalidArgument, "quote_id required") }
	if req.Amount != nil && req.Amount.AmountMinor <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount must be positive")
	}
	trade, err := s.store.CreateTrade(req.InstitutionId, req.QuoteId, req.IdempotencyKey, req.CorrelationId, req.BaseAmountMinor, req.BaseCurrency, req.QuoteCurrency)
	if err != nil { return nil, status.Error(codes.InvalidArgument, err.Error()) }
	if s.settlementClient != nil {
		resp, err := s.settlementClient.CreateSettlementForTrade(ctx, &settlementv1.CreateSettlementForTradeRequest{
			InstitutionId: req.InstitutionId,
			TradeId:       trade.ID,
			QuoteId:       req.QuoteId,
			IdempotencyKey: req.IdempotencyKey,
			CorrelationId: req.CorrelationId,
			Beneficiary:   "acct_" + strings.TrimPrefix(trade.ID, "trd_"),
			Amount:        &settlementv1.Money{AmountMinor: req.BaseAmountMinor, Currency: req.BaseCurrency},
			Purpose:       "trade-acceptance",
		})
		if err == nil && resp != nil && strings.TrimSpace(resp.SettlementId) != "" {
			_ = s.store.SetSettlementID(trade.ID, resp.SettlementId)
			trade.SettlementID = resp.SettlementId
		}
	}
	log.Printf("trade created: trade_id=%s quote_id=%s institution_id=%s settlement_id=%s correlation_id=%s", trade.ID, trade.QuoteID, trade.InstitutionID, trade.SettlementID, trade.CorrelationID)
	return &tradev1.TradeResponse{TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID, Status: trade.Status, BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency, QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID, SettlementId: trade.SettlementID}, nil
}

func (s *tradeServer) GetTrade(ctx context.Context, req *tradev1.GetTradeRequest) (*tradev1.TradeResponse, error) {
	if s == nil || s.store == nil || req == nil { return nil, status.Error(codes.InvalidArgument, "request required") }
	if strings.TrimSpace(req.InstitutionId) == "" { return nil, status.Error(codes.InvalidArgument, "institution_id required") }
	if strings.TrimSpace(req.TradeId) == "" { return nil, status.Error(codes.InvalidArgument, "trade_id required") }
	trade, ok := s.store.GetTrade(req.TradeId)
	if !ok { return nil, status.Error(codes.NotFound, "trade not found") }
	if trade.InstitutionID != req.InstitutionId { return nil, status.Error(codes.PermissionDenied, "forbidden") }
	return &tradev1.TradeResponse{TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID, Status: trade.Status, BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency, QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID, SettlementId: trade.SettlementID}, nil
}

func dialTradeService(grpcAddr string) (settlementv1.SettlementServiceClient, error) {
	if strings.TrimSpace(grpcAddr) == "" { return nil, nil }
	conn, err := grpc.Dial(grpcAddr, grpc.WithInsecure())
	if err != nil { return nil, err }
	log.Printf("trade orchestration connected to settlement service at %s", grpcAddr)
	return settlementv1.NewSettlementServiceClient(conn), nil
}

func startTradeServer() {
	_ = time.Now
}
