package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/adapters/simulator/simcore"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type grpcServer struct {
	quotev1.UnimplementedQuoteServiceServer
	store       *quotecore.QuoteStore
	pricing     feeScheduleStore
	tradeClient tradev1.TradeServiceClient
	liquidity   *simcore.NormalizedAdapter
}

type feeScheduleStore interface {
	GetFeeSchedule(context.Context, string, string, string) (quotecore.FeeSchedule, error)
}

func (s *grpcServer) CreateQuote(ctx context.Context, req *quotev1.CreateQuoteRequest) (*quotev1.QuoteResponse, error) {
	if s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key required")
	}
	mode := quotecore.QuoteMode(req.QuoteMode)
	if mode == "" {
		mode = quotecore.QuoteModeFixedSource
	}
	if mode != quotecore.QuoteModeFixedSource && mode != quotecore.QuoteModeFixedDestination {
		return nil, status.Error(codes.InvalidArgument, "unsupported quote mode")
	}
	request := quotecore.QuoteRequest{
		CustomerID:             req.InstitutionId,
		QuoteMode:              mode,
		BaseAmountMinor:        req.BaseAmountMinor,
		DestinationAmountMinor: req.DestinationAmountMinor,
		BaseCurrency:           strings.ToUpper(strings.TrimSpace(req.BaseCurrency)),
		QuoteCurrency:          strings.ToUpper(strings.TrimSpace(req.QuoteCurrency)),
		DestinationRail:        req.DestinationRail,
		Urgency:                req.Urgency,
		CustomerReference:      req.CustomerReference,
		IdempotencyKey:         req.IdempotencyKey,
	}
	if (mode == quotecore.QuoteModeFixedSource && (request.BaseAmountMinor <= 0 || request.DestinationAmountMinor != 0)) ||
		(mode == quotecore.QuoteModeFixedDestination && (request.BaseAmountMinor != 0 || request.DestinationAmountMinor <= 0)) {
		return nil, status.Error(codes.InvalidArgument, "quote amount does not match quote mode")
	}
	existing, found, err := s.store.FindByIdempotency(ctx, request.CustomerID, request.IdempotencyKey, quotecore.CanonicalQuoteRequestHash(request))
	if err != nil {
		if errors.Is(err, quotecore.ErrQuoteIdempotencyConflict) {
			return nil, status.Error(codes.AlreadyExists, "idempotency key reused with different quote request")
		}
		return nil, status.Error(codes.Unavailable, "quote idempotency store unavailable")
	}
	if found {
		return toQuoteResponse(existing), nil
	}
	pricingStore := s.pricing
	if pricingStore == nil {
		pricingStore = s.store
	}
	policy, err := pricingStore.GetFeeSchedule(ctx, request.CustomerID, request.BaseCurrency, request.QuoteCurrency)
	if err != nil {
		if errors.Is(err, quotecore.ErrPricingPolicyUnavailable) {
			return nil, status.Error(codes.FailedPrecondition, "approved pricing policy unavailable")
		}
		return nil, status.Error(codes.Unavailable, "pricing policy lookup failed")
	}
	probeRequest := request
	if mode == quotecore.QuoteModeFixedDestination {
		probeRequest.BaseAmountMinor = policy.MinimumSourcePrincipalMinor
	}
	probeQuote, err := s.selectProviderQuote(ctx, probeRequest)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "firm provider quote unavailable")
	}
	pricingInput := quotecore.QuotePricingInput{
		Mode:                   mode,
		SourceAmountMinor:      request.BaseAmountMinor,
		DestinationAmountMinor: request.DestinationAmountMinor,
		RateScaled:             probeQuote.RateMinor,
	}
	priced, err := quotecore.PriceQuote(policy, pricingInput)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	firmQuote := probeQuote
	if firmQuote.BaseAmountMinor != priced.SourcePrincipalMinor {
		firmRequest := request
		firmRequest.BaseAmountMinor = priced.SourcePrincipalMinor
		firmQuote, err = s.selectProviderQuote(ctx, firmRequest)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "amount-bound firm provider quote unavailable")
		}
	}
	if firmQuote.BaseAmountMinor != priced.SourcePrincipalMinor ||
		firmQuote.BaseCurrency != request.BaseCurrency ||
		firmQuote.QuoteCurrency != request.QuoteCurrency ||
		firmQuote.RateMinor <= 0 ||
		firmQuote.FeeMinor != 0 {
		return nil, status.Error(codes.FailedPrecondition, "provider quote does not match priced economics")
	}
	pricingInput.RateScaled = firmQuote.RateMinor
	priced, err = quotecore.PriceQuote(policy, pricingInput)
	if err != nil || priced.SourcePrincipalMinor != firmQuote.BaseAmountMinor {
		return nil, status.Error(codes.FailedPrecondition, "provider rate changed the approved quote economics")
	}
	expectedOutput, err := quotecore.ConvertMinorUnits(priced.SourcePrincipalMinor, firmQuote.RateMinor, policy.SourceExponent, policy.DestinationExponent)
	if err != nil || firmQuote.AmountOutMinor != expectedOutput {
		return nil, status.Error(codes.FailedPrecondition, "provider output does not match the quoted rate")
	}
	quote, err := s.store.CreatePricedFromProvider(request, firmQuote, policy, priced)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return toQuoteResponse(quote), nil
}

func (s *grpcServer) selectProviderQuote(ctx context.Context, req quotecore.QuoteRequest) (quotecore.ProviderQuote, error) {
	if s.liquidity == nil || req.BaseAmountMinor <= 0 || req.BaseCurrency == "" || req.QuoteCurrency == "" {
		return quotecore.ProviderQuote{}, errors.New("liquidity route unavailable")
	}
	if req.DestinationRail != "sim_lp_1" && req.DestinationRail != "flutterwave" {
		return quotecore.ProviderQuote{}, errors.New("unsupported settlement route")
	}
	if req.DestinationRail == "sim_lp_1" && (os.Getenv("OSAI_SIMULATOR_ROUTING_ENABLED") == "false" || os.Getenv("OSAI_SIMULATOR_KILL_SWITCH") == "true") {
		return quotecore.ProviderQuote{}, errors.New("simulator route disabled")
	}
	if req.DestinationRail == "flutterwave" && (os.Getenv("OSAI_FLW_TRADE_ROUTING_ENABLED") != "true" || os.Getenv("FLW_SETTLEMENT_ENABLED") != "true" || os.Getenv("FLW_KILL_SWITCH") == "true" || req.QuoteCurrency != "NGN") {
		return quotecore.ProviderQuote{}, errors.New("Flutterwave customer route unavailable")
	}
	if req.DestinationRail == "flutterwave" {
		rail := flutterwave.New(flutterwave.Config{BaseURL: os.Getenv("FLW_BASE_URL"), SecretKey: os.Getenv("FLW_SECRET_KEY"), SecretHash: os.Getenv("FLW_SECRET_HASH"), Env: os.Getenv("FLW_ENV"), Enabled: true, Timeout: 3 * time.Second})
		if err := rail.ProbeTransferHealth(ctx); err != nil || !rail.Health().Capabilities["webhook"] || !rail.Health().Capabilities["get_transfer"] {
			return quotecore.ProviderQuote{}, errors.New("Flutterwave route health unavailable")
		}
	}
	health := s.liquidity.Health()
	if !health.Up || !health.Capabilities["quotes"] || (req.BaseCurrency != req.QuoteCurrency && !health.Capabilities["fx"]) || !health.Capabilities["payout"] {
		return quotecore.ProviderQuote{}, errors.New("liquidity capability unavailable")
	}
	quote, err := s.liquidity.GetQuote(req)
	if err != nil || quote.ProviderID != "sim_lp_1" {
		return quotecore.ProviderQuote{}, errors.New("liquidity quote unavailable")
	}
	return quote, nil
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
	if quote.Pricing != nil &&
		(quote.Request.BaseCurrency != quote.Request.QuoteCurrency ||
			quote.FeeMinor != 0 ||
			quote.Pricing.TotalDebitMinor != quote.AmountOutMinor) {
		return nil, status.Error(codes.FailedPrecondition, "trade funding does not yet support this priced FX or fee flow")
	}
	if quote.Status == quotecore.QuoteStatusAccepted {
		var trade *tradev1.TradeResponse
		if req.BeneficiaryId != "" {
			if s.tradeClient == nil {
				return nil, status.Error(codes.Unavailable, "trade service unavailable")
			}
			var err error
			trade, err = s.tradeClient.GetTrade(ctx, &tradev1.GetTradeRequest{InstitutionId: req.InstitutionId, TradeId: quote.AcceptedTradeID})
			if err != nil || trade.BeneficiaryId != req.BeneficiaryId {
				return nil, status.Error(codes.AlreadyExists, "quote beneficiary selection conflict")
			}
		}
		if trade != nil && (trade.Status == "FUNDING_REQUIRED" || trade.Status == "FUNDED") {
			_, err := s.tradeClient.CreateTradeFromAcceptedQuote(ctx, &tradev1.CreateTradeFromAcceptedQuoteRequest{InstitutionId: req.InstitutionId, QuoteId: req.QuoteId, IdempotencyKey: req.IdempotencyKey, CorrelationId: req.CorrelationId, BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, Amount: &tradev1.Money{AmountMinor: quote.AmountOutMinor, Currency: quote.Request.QuoteCurrency}, BeneficiaryId: req.BeneficiaryId, SettlementProviderId: quote.Request.DestinationRail, CustomerReference: quote.Request.CustomerReference})
			if err != nil {
				return nil, status.Error(codes.Unavailable, "trade funding resume unavailable")
			}
		}
		resp := toQuoteResponse(quote)
		resp.TradeId = quote.AcceptedTradeID
		return resp, nil
	}
	if s.tradeClient == nil {
		return nil, status.Error(codes.Unavailable, "trade service unavailable")
	}
	if quote.IsExpiredAt(time.Now()) || quote.Status != quotecore.QuoteStatusQuoted {
		return nil, status.Error(codes.FailedPrecondition, "quote is not executable")
	}
	tradeResp, err := s.tradeClient.CreateTradeFromAcceptedQuote(ctx, &tradev1.CreateTradeFromAcceptedQuoteRequest{
		InstitutionId:        req.InstitutionId,
		QuoteId:              req.QuoteId,
		IdempotencyKey:       req.IdempotencyKey,
		CorrelationId:        req.CorrelationId,
		BaseAmountMinor:      quote.Request.BaseAmountMinor,
		BaseCurrency:         quote.Request.BaseCurrency,
		QuoteCurrency:        quote.Request.QuoteCurrency,
		Amount:               &tradev1.Money{AmountMinor: quote.AmountOutMinor, Currency: quote.Request.QuoteCurrency},
		BeneficiaryId:        req.BeneficiaryId,
		SettlementProviderId: quote.Request.DestinationRail,
		CustomerReference:    quote.Request.CustomerReference,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	tradeID := tradeResp.GetTradeId()
	if strings.TrimSpace(tradeID) == "" {
		return nil, status.Error(codes.Internal, "trade-orchestrator did not return a trade id")
	}
	quote, err = s.store.AcceptWithCorrelation(quote.ID, req.IdempotencyKey, tradeID, req.CorrelationId, time.Now().UTC())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	quote.AcceptedTradeID = tradeID
	resp := toQuoteResponse(quote)
	resp.TradeId = tradeID
	println("quote accepted: quote_id=" + req.QuoteId + " trade_id=" + tradeID + " institution_id=" + req.InstitutionId + " correlation_id=" + req.CorrelationId)
	return resp, nil
}

func toQuoteResponse(quote *quotecore.ExecutableQuote) *quotev1.QuoteResponse {
	if quote == nil {
		return &quotev1.QuoteResponse{}
	}
	baseAmountMinor := quote.Request.BaseAmountMinor
	amountOutMinor := quote.AmountOutMinor
	response := &quotev1.QuoteResponse{
		QuoteId:         quote.ID,
		TradeId:         quote.AcceptedTradeID,
		Status:          string(quote.Status),
		BaseAmountMinor: baseAmountMinor,
		BaseCurrency:    quote.Request.BaseCurrency,
		QuoteCurrency:   quote.Request.QuoteCurrency,
		DestinationRail: quote.Request.DestinationRail,
		AmountOutMinor:  quote.AmountOutMinor,
		RateMinor:       quote.RateMinor,
		FeeMinor:        quote.FeeMinor,
		ExpiresAt:       quote.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if quote.Pricing != nil {
		baseAmountMinor = quote.Pricing.SourcePrincipalMinor
		if quote.Pricing.Mode == quotecore.QuoteModeFixedDestination {
			amountOutMinor = quote.Pricing.DestinationTargetMinor
		}
		response.BaseAmountMinor = baseAmountMinor
		response.AmountOutMinor = amountOutMinor
		response.QuoteMode = string(quote.Pricing.Mode)
		response.SourcePrincipalMinor = quote.Pricing.SourcePrincipalMinor
		response.TotalDebitMinor = quote.Pricing.TotalDebitMinor
		response.DestinationTargetMinor = quote.Pricing.DestinationTargetMinor
		response.DestinationSurplusMinor = quote.Pricing.DestinationSurplusMinor
		response.RateDirection = quote.Pricing.RateDirection
		response.RateScale = quote.Pricing.RateScale
		response.FeeScheduleId = quote.Pricing.FeeSchedule.ID
		response.FeeScheduleVersion = quote.Pricing.FeeSchedule.Version
		response.SourceMinorExponent = uint32(quote.Pricing.SourceExponent)
		response.DestinationMinorExponent = uint32(quote.Pricing.DestinationExponent)
	}
	return response
}
