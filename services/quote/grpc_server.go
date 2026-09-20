package main

import (
	"context"
	"strings"
	"time"

	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcServer struct {
	quotev1.UnimplementedQuoteServiceServer
	store      *quotecore.QuoteStore
	tradeClient tradev1.TradeServiceClient
}

func (s *grpcServer) CreateQuote(ctx context.Context, req *quotev1.CreateQuoteRequest) (*quotev1.QuoteResponse, error) {
	if s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	quote, err := s.store.Create(quotecore.QuoteRequest{
		CustomerID:      req.InstitutionId,
		BaseAmountMinor: req.BaseAmountMinor,
		BaseCurrency:    req.BaseCurrency,
		QuoteCurrency:   req.QuoteCurrency,
		DestinationRail: req.DestinationRail,
		Urgency:         req.Urgency,
		IdempotencyKey:  req.IdempotencyKey,
	}, req.DestinationRail, 1200, 500, req.BaseAmountMinor/100, time.Now().Add(30*time.Minute))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return toQuoteResponse(quote), nil
}

func (s *grpcServer) GetQuote(ctx context.Context, req *quotev1.GetQuoteRequest) (*quotev1.QuoteResponse, error) {
	if s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	quote, ok := s.store.Get(req.QuoteId)
	if !ok {
		return nil, status.Error(codes.NotFound, "quote not found")
	}
	if quote.Request.CustomerID != req.InstitutionId {
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	return toQuoteResponse(quote), nil
}

func (s *grpcServer) AcceptQuote(ctx context.Context, req *quotev1.AcceptQuoteRequest) (*quotev1.QuoteResponse, error) {
	if s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	quote, ok := s.store.Get(req.QuoteId)
	if !ok {
		return nil, status.Error(codes.NotFound, "quote not found")
	}
	if quote.Request.CustomerID != req.InstitutionId {
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	if quote.Status == quotecore.QuoteStatusAccepted {
		resp := toQuoteResponse(quote)
		resp.TradeId = quote.AcceptedTradeID
		return resp, nil
	}
	if s.tradeClient == nil {
		return nil, status.Error(codes.Unavailable, "trade service unavailable")
	}
	tradeResp, err := s.tradeClient.CreateTradeFromAcceptedQuote(ctx, &tradev1.CreateTradeFromAcceptedQuoteRequest{
		InstitutionId: req.InstitutionId,
		QuoteId:      req.QuoteId,
		IdempotencyKey: req.IdempotencyKey,
		CorrelationId: req.CorrelationId,
		BaseAmountMinor: quote.Request.BaseAmountMinor,
		BaseCurrency: quote.Request.BaseCurrency,
		QuoteCurrency: quote.Request.QuoteCurrency,
		Amount: &tradev1.Money{AmountMinor: quote.AmountOutMinor, Currency: quote.Request.QuoteCurrency},
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	tradeID := tradeResp.GetTradeId()
	if strings.TrimSpace(tradeID) == "" {
		return nil, status.Error(codes.Internal, "trade-orchestrator did not return a trade id")
	}
	if err := quote.Accept(tradeID, req.IdempotencyKey, time.Now().UTC(), s.store); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	quote.AcceptedTradeID = tradeID
	resp := toQuoteResponse(quote)
	resp.TradeId = tradeID
	println("quote accepted: quote_id=" + req.QuoteId + " trade_id=" + tradeID + " institution_id=" + req.InstitutionId + " correlation_id=" + req.CorrelationId)
	return resp, nil
}

func toQuoteResponse(quote *quotecore.ExecutableQuote) *quotev1.QuoteResponse {
	if quote == nil { return &quotev1.QuoteResponse{} }
	return &quotev1.QuoteResponse{
		QuoteId:         quote.ID,
		TradeId:         quote.AcceptedTradeID,
		Status:          string(quote.Status),
		BaseAmountMinor: quote.Request.BaseAmountMinor,
		BaseCurrency:    quote.Request.BaseCurrency,
		QuoteCurrency:   quote.Request.QuoteCurrency,
		DestinationRail: quote.Request.DestinationRail,
		AmountOutMinor:  quote.AmountOutMinor,
		RateMinor:       quote.RateMinor,
		FeeMinor:        quote.FeeMinor,
		ExpiresAt:       quote.ExpiresAt.UTC().Format(time.RFC3339),
	}
}
