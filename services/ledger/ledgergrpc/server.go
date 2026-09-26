package ledgergrpc

import (
	"context"
	"database/sql"
	"errors"
	"time"

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
	DB      *sql.DB
}

func (s *Server) GetCustomerBalances(ctx context.Context, req *ledgerv1.CustomerReadRequest) (*ledgerv1.CustomerBalanceResponse, error) {
	if req == nil || req.InstitutionId == "" || s.DB == nil {
		return nil, status.Error(codes.InvalidArgument, "institution and ledger storage required")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,SUM(CASE WHEN account_code='2000' THEN credit-debit ELSE 0 END),SUM(CASE WHEN account_code='2100' THEN credit-debit ELSE 0 END) FROM ledger_entries WHERE owner=$1 AND account_code IN ('2000','2100') GROUP BY currency ORDER BY currency`, req.InstitutionId)
	if err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	defer rows.Close()
	result := &ledgerv1.CustomerBalanceResponse{}
	for rows.Next() {
		var b ledgerv1.CustomerBalance
		if err = rows.Scan(&b.Currency, &b.AvailableMinor, &b.HeldMinor); err != nil {
			return nil, status.Error(codes.Internal, "ledger read failed")
		}
		result.Balances = append(result.Balances, &b)
	}
	if err = rows.Err(); err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	if len(result.Balances) == 0 {
		result.Balances = []*ledgerv1.CustomerBalance{{Currency: "NGN"}}
	}
	return result, nil
}
func (s *Server) ListCustomerTransactions(ctx context.Context, req *ledgerv1.CustomerReadRequest) (*ledgerv1.CustomerTransactionResponse, error) {
	if req == nil || req.InstitutionId == "" || s.DB == nil {
		return nil, status.Error(codes.InvalidArgument, "institution and ledger storage required")
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 25
	}
	var count int32
	if err := s.DB.QueryRowContext(ctx, `SELECT count(DISTINCT journal_id) FROM ledger_entries WHERE owner=$1`, req.InstitutionId).Scan(&count); err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT e.journal_id,j.trade_id,e.currency,SUM(e.credit-e.debit),j.created_at FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id WHERE e.owner=$1 GROUP BY e.journal_id,j.trade_id,e.currency,j.created_at ORDER BY j.created_at DESC,e.journal_id LIMIT $2 OFFSET $3`, req.InstitutionId, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	defer rows.Close()
	result := &ledgerv1.CustomerTransactionResponse{Total: count}
	for rows.Next() {
		var item ledgerv1.CustomerTransaction
		var occurred time.Time
		if err = rows.Scan(&item.JournalId, &item.TradeId, &item.Currency, &item.AmountMinor, &occurred); err != nil {
			return nil, status.Error(codes.Internal, "ledger read failed")
		}
		item.OccurredAt = occurred.UTC().Format(time.RFC3339)
		result.Items = append(result.Items, &item)
	}
	if err = rows.Err(); err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	return result, nil
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
