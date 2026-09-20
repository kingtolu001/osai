package ledgergrpc

import (
	"context"
	"errors"

	"github.com/osai/osai/pkg/observability"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	Service *ledgerapi.Service
}

func (s *Server) ConfirmSettlement(ctx context.Context, request *ledgerv1.SettlementCommand) (*ledgerv1.SettlementCommandResponse, error) {
	return s.apply(ctx, request, s.Service.ConfirmCommand)
}

func (s *Server) FailSettlement(ctx context.Context, request *ledgerv1.SettlementCommand) (*ledgerv1.SettlementCommandResponse, error) {
	return s.apply(ctx, request, s.Service.FailCommand)
}

func (s *Server) ReverseSettlement(ctx context.Context, request *ledgerv1.SettlementCommand) (*ledgerv1.SettlementCommandResponse, error) {
	return s.apply(ctx, request, s.Service.ReverseCommand)
}

func (s *Server) PostReconciliationAdjustment(ctx context.Context, request *ledgerv1.ReconciliationAdjustmentCommand) (*ledgerv1.SettlementCommandResponse, error) {
	ctx, span := observability.Start(ctx, "osai/ledger", "reconciliation.adjustment")
	defer span.End()
	if values := metadata.ValueFromIncomingContext(ctx, "x-osai-correlation-id"); len(values) > 0 && request != nil && request.GetCorrelationId() == "" {
		request.CorrelationId = values[0]
	}
	if s.Service == nil || request == nil {
		return nil, status.Error(codes.InvalidArgument, "reconciliation adjustment command is required")
	}
	if request.GetCorrelationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "correlation_id is required")
	}
	result, replayed, err := s.Service.PostReconciliationAdjustment(ledgerapi.ReconciliationAdjustmentCommand{
		IdempotencyKey: request.GetIdempotencyKey(),
		CorrelationID:  request.GetCorrelationId(),
		BreakID:        request.GetBreakId(),
		Maker:          request.GetMaker(),
		Checker:        request.GetChecker(),
		Currency:       request.GetCurrency(),
		AmountMinor:    request.GetAmountMinor(),
		Reason:         request.GetReason(),
		JournalTag:     request.GetJournalTag(),
	})
	if err != nil {
		if errors.Is(err, ledgerapi.ErrIdempotencyConflict) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &ledgerv1.SettlementCommandResponse{JournalId: result, AlreadyApplied: replayed}, nil
}

func (s *Server) apply(ctx context.Context, request *ledgerv1.SettlementCommand, command func(ledgerapi.Command) (string, bool, error)) (*ledgerv1.SettlementCommandResponse, error) {
	ctx, span := observability.Start(ctx, "osai/ledger", "ledger.command")
	defer span.End()
	if values := metadata.ValueFromIncomingContext(ctx, "x-osai-correlation-id"); len(values) > 0 && request != nil && request.GetCorrelationId() == "" {
		request.CorrelationId = values[0]
	}
	if s.Service == nil || request == nil || request.GetAmount() == nil {
		return nil, status.Error(codes.InvalidArgument, "ledger command and amount are required")
	}
	if request.GetCorrelationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "correlation_id is required")
	}
	amount := request.GetAmount()
	result, replayed, err := command(ledgerapi.Command{
		IdempotencyKey: request.GetIdempotencyKey(), CorrelationID: request.GetCorrelationId(), SettlementID: request.GetSettlementInstructionId(), TradeID: request.GetTradeId(), ObligationID: request.GetObligationId(), ProviderReference: request.GetProviderReference(), ExternalReference: request.GetExternalReference(), Beneficiary: request.GetBeneficiary(), AmountMinor: amount.GetAmountMinor(), Currency: amount.GetCurrency(), Purpose: request.GetPurpose(),
	})
	if err != nil {
		if errors.Is(err, ledgerapi.ErrIdempotencyConflict) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &ledgerv1.SettlementCommandResponse{JournalId: result, AlreadyApplied: replayed}, nil
}
