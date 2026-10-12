package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

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

func (s *tradeServer) reserveFunding(ctx context.Context, institutionID, tradeID, currency string, amountMinor int64) error {
	if s == nil || s.ledgerClient == nil {
		return nil
	}
	if amountMinor <= 0 {
		return status.Error(codes.InvalidArgument, "amount must be positive")
	}
	resp, err := s.ledgerClient.ReserveFunding(ctx, &ledgerv1.FundingReservationCommand{InstitutionId: institutionID, TradeId: tradeID, Amount: &ledgerv1.Money{AmountMinor: amountMinor, Currency: currency}, CorrelationId: tradeID})
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
	if req.Amount != nil && req.Amount.AmountMinor <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount must be positive")
	}
	amountMinor := req.BaseAmountMinor
	currency := req.BaseCurrency
	if req.Amount != nil {
		amountMinor = req.Amount.AmountMinor
		if strings.TrimSpace(req.Amount.Currency) != "" {
			currency = req.Amount.Currency
		}
	}
	if strings.TrimSpace(currency) == "" {
		return nil, status.Error(codes.InvalidArgument, "currency required")
	}
	if strings.EqualFold(req.SettlementProviderId, "flutterwave") && os.Getenv("OSAI_FLW_TRADE_ROUTING_ENABLED") != "true" {
		return nil, status.Error(codes.FailedPrecondition, "flutterwave trade routing is disabled")
	}
	if err := s.validateApprovedBeneficiary(ctx, req.InstitutionId, req.BeneficiaryId); err != nil {
		return nil, err
	}
	trade, err := s.store.CreateTrade(req.InstitutionId, req.QuoteId, req.IdempotencyKey, req.CorrelationId, req.BaseAmountMinor, req.BaseCurrency, req.QuoteCurrency, req.BeneficiaryId, req.SettlementProviderId, req.CustomerReference)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.store.SetStatus(trade.ID, tradecore.StateFundingRequired); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	trade.State = tradecore.StateFundingRequired
	trade.Status = string(tradecore.StateFundingRequired)
	if err := s.reserveFunding(ctx, req.InstitutionId, trade.ID, currency, amountMinor); err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			return &tradev1.TradeResponse{TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID, Status: string(tradecore.StateFundingRequired), BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency, QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID, BeneficiaryId: req.BeneficiaryId, SettlementProviderId: req.SettlementProviderId, CustomerReference: trade.CustomerReference}, nil
		}
		if setErr := s.store.SetStatus(trade.ID, tradecore.StateFundingRequired); setErr != nil {
			log.Printf("funding gate rejected but trade status could not be updated: %v", setErr)
		}
		return nil, err
	}
	if err := s.store.SetStatus(trade.ID, tradecore.StateFunded); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	trade.State = tradecore.StateFunded
	trade.Status = string(tradecore.StateFunded)
	if s.settlementClient != nil {
		resp, err := s.settlementClient.CreateSettlementForTrade(ctx, &settlementv1.CreateSettlementForTradeRequest{
			InstitutionId:  req.InstitutionId,
			TradeId:        trade.ID,
			QuoteId:        req.QuoteId,
			IdempotencyKey: req.IdempotencyKey,
			CorrelationId:  req.CorrelationId,
			BeneficiaryId:  req.BeneficiaryId,
			ProviderId:     req.SettlementProviderId,
			Amount:         &settlementv1.Money{AmountMinor: amountMinor, Currency: currency},
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
	return &tradev1.TradeResponse{TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID, Status: trade.Status, BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency, QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID, SettlementId: trade.SettlementID, BeneficiaryId: req.BeneficiaryId, SettlementProviderId: req.SettlementProviderId, CustomerReference: trade.CustomerReference}, nil
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
	return &tradev1.TradeResponse{TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID, Status: trade.Status, BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency, QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID, SettlementId: trade.SettlementID, BeneficiaryId: trade.BeneficiaryID, SettlementProviderId: trade.SettlementProviderID, CustomerReference: trade.CustomerReference}, nil
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
		response.Items = append(response.Items, &tradev1.TradeResponse{
			TradeId: trade.ID, InstitutionId: trade.InstitutionID, QuoteId: trade.QuoteID,
			Status: trade.Status, BaseAmountMinor: trade.BaseAmountMinor, BaseCurrency: trade.BaseCurrency,
			QuoteCurrency: trade.QuoteCurrency, CorrelationId: trade.CorrelationID,
			SettlementId: trade.SettlementID, BeneficiaryId: trade.BeneficiaryID,
			SettlementProviderId: trade.SettlementProviderID, CustomerReference: trade.CustomerReference,
		})
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
