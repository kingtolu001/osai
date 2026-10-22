package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/sandboxpolicy"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type tradeServer struct {
	tradev1.UnimplementedTradeServiceServer
	store            *tradecore.Store
	settlementClient settlementv1.SettlementServiceClient
	customerClient   customerv1.CustomerServiceClient
	ledgerClient     ledgerv1.LedgerServiceClient
}

func newTradeServer(store *tradecore.Store, settlementClient settlementv1.SettlementServiceClient) *tradeServer {
	if store == nil {
		store = tradecore.NewStore()
	}
	return &tradeServer{store: store, settlementClient: settlementClient}
}

func (s *tradeServer) withCustomerClient(client customerv1.CustomerServiceClient) *tradeServer {
	s.customerClient = client
	return s
}

func (s *tradeServer) withLedgerClient(client ledgerv1.LedgerServiceClient) *tradeServer {
	s.ledgerClient = client
	return s
}

func (s *tradeServer) validateApprovedBeneficiary(ctx context.Context, institutionID, beneficiaryID string) error {
	if s == nil || s.customerClient == nil || strings.TrimSpace(beneficiaryID) == "" {
		return nil
	}
	resp, err := s.customerClient.GetApprovedBeneficiary(ctx, &customerv1.BeneficiaryRequest{InstitutionId: institutionID, BeneficiaryId: beneficiaryID})
	if err != nil {
		return status.Error(codes.FailedPrecondition, "beneficiary validation failed")
	}
	if resp == nil || strings.TrimSpace(resp.InstitutionId) == "" || resp.InstitutionId != institutionID {
		return status.Error(codes.FailedPrecondition, "beneficiary institution mismatch")
	}
	if strings.EqualFold(resp.Status, "DISABLED") || !strings.EqualFold(resp.ApprovalStatus, "APPROVED") || !strings.EqualFold(resp.Status, "ACTIVE") {
		return status.Error(codes.FailedPrecondition, "beneficiary not approved for settlement")
	}
	return nil
}

func (s *tradeServer) reserveFunding(ctx context.Context, institutionID, tradeID, currency string, amountMinor int64, snapshot *pricing.Snapshot) error {
	if s == nil || s.ledgerClient == nil {
		return nil
	}
	if amountMinor <= 0 {
		return status.Error(codes.InvalidArgument, "amount must be positive")
	}
	command := &ledgerv1.FundingReservationCommand{
		InstitutionId: institutionID, TradeId: tradeID,
		Amount:        &ledgerv1.Money{AmountMinor: amountMinor, Currency: currency},
		CorrelationId: tradeID, SourcePrincipalMinor: amountMinor,
	}
	if snapshot != nil {
		snapshotJSON, err := json.Marshal(snapshot)
		if err != nil {
			return status.Error(codes.Internal, "pricing snapshot serialization failed")
		}
		command.SourcePrincipalMinor = snapshot.SourcePrincipalMinor
		command.FeeMinor = snapshot.FeeMinor
		command.PricingHash = snapshot.Hash
		command.PricingSnapshotJson = string(snapshotJSON)
	}
	resp, err := s.ledgerClient.ReserveFunding(sandboxpolicy.Outgoing(ctx), command)
	if err != nil {
		return err
	}
	if resp == nil || !strings.EqualFold(resp.State, "ACTIVE") {
		return status.Error(codes.FailedPrecondition, "active funding reservation required")
	}
	return nil
}

func (s *tradeServer) CreateTradeFromAcceptedQuote(ctx context.Context, req *tradev1.CreateTradeFromAcceptedQuoteRequest) (*tradev1.TradeResponse, error) {
	if s == nil || s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	if strings.TrimSpace(req.QuoteId) == "" {
		return nil, status.Error(codes.InvalidArgument, "quote_id required")
	}
	if req.BaseAmountMinor <= 0 || strings.TrimSpace(req.BaseCurrency) == "" || strings.TrimSpace(req.QuoteCurrency) == "" {
		return nil, status.Error(codes.InvalidArgument, "positive source amount and currencies required")
	}
	if req.Amount != nil && req.Amount.AmountMinor <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount must be positive")
	}
	currency := strings.ToUpper(strings.TrimSpace(req.BaseCurrency))
	payoutAmountMinor := req.BaseAmountMinor
	reservationAmountMinor := req.BaseAmountMinor
	var pricingSnapshot *pricing.Snapshot
	if req.PricingSnapshot != nil {
		converted := pricingSnapshotFromProto(req.PricingSnapshot)
		if err := pricing.Validate(converted); err != nil {
			return nil, status.Error(codes.InvalidArgument, "pricing snapshot is invalid")
		}
		if converted.SourceCurrency != strings.ToUpper(strings.TrimSpace(req.BaseCurrency)) ||
			converted.DestinationCurrency != strings.ToUpper(strings.TrimSpace(req.QuoteCurrency)) ||
			converted.SourcePrincipalMinor != req.BaseAmountMinor ||
			req.Amount == nil || converted.DestinationAmountMinor != req.Amount.AmountMinor ||
			!strings.EqualFold(req.Amount.Currency, converted.DestinationCurrency) {
			return nil, status.Error(codes.FailedPrecondition, "trade amount does not match immutable pricing snapshot")
		}
		if _, exists := s.store.GetByQuoteID(req.QuoteId); !exists {
			expiresAt, _ := time.Parse(time.RFC3339Nano, converted.ExpiresAt)
			if !expiresAt.After(time.Now()) {
				return nil, status.Error(codes.FailedPrecondition, "quote expired before trade acceptance")
			}
		}
		if converted.SourceCurrency != converted.DestinationCurrency {
			return nil, status.Error(codes.FailedPrecondition, "cross-currency funding requires verified FX execution and Ledger posting")
		}
		if s.ledgerClient == nil {
			return nil, status.Error(codes.Unavailable, "durable Ledger funding is required")
		}
		pricingSnapshot = &converted
		payoutAmountMinor = converted.DestinationAmountMinor
		reservationAmountMinor = converted.TotalDebitMinor
	} else {
		amountMinor := req.BaseAmountMinor
		if req.Amount != nil {
			amountMinor = req.Amount.AmountMinor
			if strings.TrimSpace(req.Amount.Currency) != "" {
				currency = strings.ToUpper(strings.TrimSpace(req.Amount.Currency))
			}
		}
		if !strings.EqualFold(req.BaseCurrency, req.QuoteCurrency) ||
			!strings.EqualFold(currency, req.BaseCurrency) ||
			amountMinor != req.BaseAmountMinor {
			return nil, status.Error(codes.FailedPrecondition, "cross-currency or fee-bearing funding requires an immutable pricing snapshot")
		}
		payoutAmountMinor = amountMinor
		reservationAmountMinor = amountMinor
	}
	if strings.EqualFold(req.SettlementProviderId, "flutterwave") && os.Getenv("OSAI_FLW_TRADE_ROUTING_ENABLED") != "true" {
		return nil, status.Error(codes.FailedPrecondition, "flutterwave trade routing is disabled")
	}
	if err := s.validateApprovedBeneficiary(ctx, req.InstitutionId, req.BeneficiaryId); err != nil {
		return nil, err
	}
	var trade *tradecore.Trade
	var err error
	if pricingSnapshot != nil {
		trade, err = s.store.CreateTradeWithPricing(req.InstitutionId, req.QuoteId, req.IdempotencyKey, req.CorrelationId,
			pricingSnapshot.SourcePrincipalMinor, req.BaseCurrency, req.QuoteCurrency, *pricingSnapshot,
			req.BeneficiaryId, req.SettlementProviderId, req.CustomerReference)
	} else {
		trade, err = s.store.CreateTrade(req.InstitutionId, req.QuoteId, req.IdempotencyKey, req.CorrelationId,
			req.BaseAmountMinor, req.BaseCurrency, req.QuoteCurrency, req.BeneficiaryId, req.SettlementProviderId, req.CustomerReference)
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if trade.State != tradecore.StateFunded && trade.State != tradecore.StateSettling &&
		trade.State != tradecore.StateFeeAccountingRequired {
		if err := s.store.SetStatus(trade.ID, tradecore.StateFundingRequired); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		trade.State = tradecore.StateFundingRequired
		trade.Status = string(tradecore.StateFundingRequired)
	}
	if err := s.reserveFunding(ctx, req.InstitutionId, trade.ID, currency, reservationAmountMinor, pricingSnapshot); err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			return tradeResponse(trade, string(tradecore.StateFundingRequired)), nil
		}
		if setErr := s.store.SetStatus(trade.ID, tradecore.StateFundingRequired); setErr != nil {
			log.Printf("funding gate rejected but trade status could not be updated: %v", setErr)
		}
		return nil, err
	}
	if trade.State != tradecore.StateFunded && trade.State != tradecore.StateSettling {
		if err := s.store.SetStatus(trade.ID, tradecore.StateFunded); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		trade.State = tradecore.StateFunded
		trade.Status = string(tradecore.StateFunded)
	}
	sandboxFees, policyErr := sandboxpolicy.Read()
	if policyErr != nil {
		return nil, status.Error(codes.FailedPrecondition, "invalid sandbox accounting configuration")
	}
	if pricingSnapshot != nil && pricingSnapshot.FeeMinor > 0 && !sandboxFees {
		if trade.State != tradecore.StateFeeAccountingRequired {
			if err := s.store.SetStatus(trade.ID, tradecore.StateFeeAccountingRequired); err != nil {
				return nil, status.Error(codes.Internal, "fee accounting state persistence failed")
			}
			trade.State = tradecore.StateFeeAccountingRequired
			trade.Status = string(tradecore.StateFeeAccountingRequired)
		}
		return tradeResponse(trade, trade.Status), nil
	}
	if s.settlementClient != nil {
		resp, err := s.settlementClient.CreateSettlementForTrade(ctx, &settlementv1.CreateSettlementForTradeRequest{
			InstitutionId:  req.InstitutionId,
			TradeId:        trade.ID,
			QuoteId:        req.QuoteId,
			IdempotencyKey: req.IdempotencyKey,
			CorrelationId:  req.CorrelationId,
			BeneficiaryId:  req.BeneficiaryId,
			ProviderId:     req.SettlementProviderId,
			Amount:         &settlementv1.Money{AmountMinor: payoutAmountMinor, Currency: currency},
			Purpose:        "trade-acceptance",
		})
		if err != nil {
			log.Printf("settlement creation failed for trade %s: %v", trade.ID, err)
			return nil, status.Error(codes.Unavailable, "settlement creation unavailable")
		}
		if resp == nil || strings.TrimSpace(resp.SettlementId) == "" {
			return nil, status.Error(codes.Unavailable, "settlement service returned no settlement id")
		}
		if err := s.store.SetSettlementID(trade.ID, resp.SettlementId); err != nil {
			log.Printf("persist settlement id for trade %s: %v", trade.ID, err)
			return nil, status.Error(codes.Internal, "settlement link persistence failed")
		}
		trade.SettlementID = resp.SettlementId
	}
	if trade.SettlementID != "" {
		if err := s.store.SetStatus(trade.ID, tradecore.StateSettling); err != nil {
			log.Printf("persist settling status for trade %s: %v", trade.ID, err)
			return nil, status.Error(codes.Internal, "trade settling state persistence failed")
		}
		trade.State = tradecore.StateSettling
		trade.Status = string(tradecore.StateSettling)
	}
	log.Printf("trade created: trade_id=%s quote_id=%s institution_id=%s settlement_id=%s correlation_id=%s", trade.ID, trade.QuoteID, trade.InstitutionID, trade.SettlementID, trade.CorrelationID)
	return tradeResponse(trade, trade.Status), nil
}

func pricingSnapshotFromProto(snapshot *tradev1.PricingSnapshot) pricing.Snapshot {
	return pricing.Snapshot{
		Mode: snapshot.GetQuoteMode(), RequestedAmountMinor: snapshot.GetRequestedAmountMinor(),
		SourcePrincipalMinor: snapshot.GetSourcePrincipalMinor(), FeeMinor: snapshot.GetFeeMinor(),
		TotalDebitMinor: snapshot.GetTotalDebitMinor(), DestinationAmountMinor: snapshot.GetDestinationAmountMinor(),
		DestinationTargetMinor: snapshot.GetDestinationTargetMinor(), DestinationSurplusMinor: snapshot.GetDestinationSurplusMinor(),
		SourceCurrency: snapshot.GetSourceCurrency(), DestinationCurrency: snapshot.GetDestinationCurrency(),
		RateScaled: snapshot.GetRateScaled(), RateDirection: snapshot.GetRateDirection(), RateScale: snapshot.GetRateScale(),
		SourceExponent: snapshot.GetSourceMinorExponent(), DestinationExponent: snapshot.GetDestinationMinorExponent(),
		FeeScheduleID: snapshot.GetFeeScheduleId(), FeeScheduleVersion: snapshot.GetFeeScheduleVersion(),
		FixedFeeMinor: snapshot.GetFixedFeeMinor(), FeeBasisPoints: snapshot.GetFeeBasisPoints(),
		MinimumFeeMinor: snapshot.GetMinimumFeeMinor(), MaximumFeeMinor: snapshot.GetMaximumFeeMinor(),
		MinimumPrincipalMinor: snapshot.GetMinimumSourcePrincipalMinor(), MaximumPrincipalMinor: snapshot.GetMaximumSourcePrincipalMinor(),
		MinimumDebitMinor: snapshot.GetMinimumTotalDebitMinor(), MaximumDebitMinor: snapshot.GetMaximumTotalDebitMinor(),
		MinimumDestinationMinor: snapshot.GetMinimumDestinationMinor(), MaximumDestinationMinor: snapshot.GetMaximumDestinationMinor(),
		ExpiresAt: snapshot.GetExpiresAt(), Hash: snapshot.GetSnapshotHash(),
	}
}

func tradeResponse(trade *tradecore.Trade, statusValue string) *tradev1.TradeResponse {
	response := &tradev1.TradeResponse{
		TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID,
		Status: statusValue, BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency,
		QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID, SettlementId: trade.SettlementID,
		BeneficiaryId: trade.BeneficiaryID, SettlementProviderId: trade.SettlementProviderID,
		CustomerReference: trade.CustomerReference, FxExecutionId: trade.FXExecutionID,
		FxExecutorId: trade.FXExecutorID, FxExecutionReference: trade.FXExecutionReference,
		FxExecutionStatus: trade.FXExecutionStatus,
	}
	if trade.PricingSnapshot != nil {
		response.PricingSnapshot = pricingSnapshotToProto(*trade.PricingSnapshot)
	}
	return response
}

func pricingSnapshotToProto(snapshot pricing.Snapshot) *tradev1.PricingSnapshot {
	return &tradev1.PricingSnapshot{
		QuoteMode: snapshot.Mode, RequestedAmountMinor: snapshot.RequestedAmountMinor,
		SourcePrincipalMinor: snapshot.SourcePrincipalMinor, FeeMinor: snapshot.FeeMinor,
		TotalDebitMinor: snapshot.TotalDebitMinor, DestinationAmountMinor: snapshot.DestinationAmountMinor,
		DestinationTargetMinor: snapshot.DestinationTargetMinor, DestinationSurplusMinor: snapshot.DestinationSurplusMinor,
		SourceCurrency: snapshot.SourceCurrency, DestinationCurrency: snapshot.DestinationCurrency,
		RateScaled: snapshot.RateScaled, RateDirection: snapshot.RateDirection, RateScale: snapshot.RateScale,
		FeeScheduleId: snapshot.FeeScheduleID, FeeScheduleVersion: snapshot.FeeScheduleVersion,
		SourceMinorExponent: snapshot.SourceExponent, DestinationMinorExponent: snapshot.DestinationExponent,
		FixedFeeMinor: snapshot.FixedFeeMinor, FeeBasisPoints: snapshot.FeeBasisPoints,
		MinimumFeeMinor: snapshot.MinimumFeeMinor, MaximumFeeMinor: snapshot.MaximumFeeMinor,
		MinimumSourcePrincipalMinor: snapshot.MinimumPrincipalMinor, MaximumSourcePrincipalMinor: snapshot.MaximumPrincipalMinor,
		MinimumTotalDebitMinor: snapshot.MinimumDebitMinor, MaximumTotalDebitMinor: snapshot.MaximumDebitMinor,
		MinimumDestinationMinor: snapshot.MinimumDestinationMinor, MaximumDestinationMinor: snapshot.MaximumDestinationMinor,
		ExpiresAt: snapshot.ExpiresAt, SnapshotHash: snapshot.Hash,
	}
}

func (s *tradeServer) GetTrade(ctx context.Context, req *tradev1.GetTradeRequest) (*tradev1.TradeResponse, error) {
	if s == nil || s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution_id required")
	}
	if strings.TrimSpace(req.TradeId) == "" {
		return nil, status.Error(codes.InvalidArgument, "trade_id required")
	}
	trade, ok := s.store.GetTrade(req.TradeId)
	if !ok {
		return nil, status.Error(codes.NotFound, "trade not found")
	}
	if trade.InstitutionID != req.InstitutionId {
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	return tradeResponse(trade, trade.Status), nil
}

func (s *tradeServer) ListTrades(ctx context.Context, req *tradev1.ListTradesRequest) (*tradev1.ListTradesResponse, error) {
	if s == nil || s.store == nil || req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if strings.TrimSpace(req.InstitutionId) == "" || req.Page < 1 || req.PageSize < 1 || req.PageSize > 100 {
		return nil, status.Error(codes.InvalidArgument, "valid institution and pagination required")
	}
	filter := tradecore.TradeListFilter{
		Page:              int(req.Page),
		PageSize:          int(req.PageSize),
		Currency:          strings.TrimSpace(req.Currency),
		Status:            strings.TrimSpace(req.Status),
		CustomerReference: strings.TrimSpace(req.CustomerReference),
	}
	var err error
	if strings.TrimSpace(req.From) != "" {
		filter.From, err = time.Parse(time.RFC3339, req.From)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "from must be RFC3339")
		}
	}
	if strings.TrimSpace(req.To) != "" {
		filter.To, err = time.Parse(time.RFC3339, req.To)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "to must be RFC3339")
		}
	}
	trades, total, err := s.store.ListTrades(req.InstitutionId, filter)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid trade list query")
	}
	response := &tradev1.ListTradesResponse{Items: make([]*tradev1.TradeResponse, 0, len(trades)), Page: req.Page, PageSize: req.PageSize, Total: int32(total)}
	for _, trade := range trades {
		response.Items = append(response.Items, tradeResponse(&trade, trade.Status))
	}
	return response, nil
}

func consumeSettlementFinality(ctx context.Context, client settlementv1.SettlementServiceClient, store *tradecore.Store, token string) error {
	if client == nil || store == nil || strings.TrimSpace(token) == "" {
		return status.Error(codes.FailedPrecondition, "settlement finality consumer is not configured")
	}
	callCtx := metadata.AppendToOutgoingContext(ctx, "x-osai-trade-settlement-token", token)
	response, err := client.ListPendingSettlementFinalityEvents(callCtx, &settlementv1.ListPendingSettlementFinalityEventsRequest{ConsumerId: "trade-orchestrator", PageSize: 50})
	if err != nil {
		return err
	}
	for _, event := range response.Events {
		if event == nil {
			return status.Error(codes.DataLoss, "settlement service returned an empty finality event")
		}
		state := tradecore.TradeState("")
		switch event.Status {
		case "CONFIRMED":
			state = tradecore.StateSettled
		case "FAILED":
			state = tradecore.StateFailed
		default:
			return status.Error(codes.DataLoss, "settlement service returned a non-final status")
		}
		if err := store.ApplySettlementFinality(tradecore.FinalityEvent{
			EventID: event.EventId, InstitutionID: event.InstitutionId, SettlementID: event.SettlementId,
			TradeID: event.TradeId, Status: state, CorrelationID: event.CorrelationId,
		}); err != nil {
			return err
		}
		ackCtx := metadata.AppendToOutgoingContext(ctx, "x-osai-trade-settlement-token", token)
		ack, err := client.AcknowledgeSettlementFinalityEvent(ackCtx, &settlementv1.AcknowledgeSettlementFinalityEventRequest{ConsumerId: "trade-orchestrator", EventId: event.EventId})
		if err != nil {
			return err
		}
		if ack == nil || !ack.Acknowledged {
			return status.Error(codes.Unavailable, "settlement finality event was not acknowledged")
		}
	}
	return nil
}

func dialTradeService(grpcAddr string) (settlementv1.SettlementServiceClient, error) {
	if strings.TrimSpace(grpcAddr) == "" {
		return nil, nil
	}
	conn, err := grpc.Dial(grpcAddr, grpc.WithInsecure(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		return nil, err
	}
	log.Printf("trade orchestration connected to settlement service at %s", grpcAddr)
	return settlementv1.NewSettlementServiceClient(conn), nil
}

func startTradeServer() {
	_ = time.Now
}
