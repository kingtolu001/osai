package ledgergrpc

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"time"

	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	Service           *ledgerapi.Service
	DB                *sql.DB
	LocalPrefundToken string
	SandboxToken      string
	SandboxAccounting SandboxAccountingWriter
}

type SandboxAccountingWriter interface {
	Apply(context.Context, sandboxaccounting.Command) (string, bool, error)
}

func (s *Server) ApplySandboxAccounting(ctx context.Context, req *ledgerv1.SandboxAccountingCommand) (*ledgerv1.SandboxAccountingResponse, error) {
	// Unlike ordinary Ledger commands, this fixture route is never available
	// when sandbox authentication is disabled.
	if s.SandboxToken == "" || s.SandboxAccounting == nil {
		return nil, status.Error(codes.PermissionDenied, "sandbox accounting unavailable")
	}
	if err := s.authorizeSandbox(ctx); err != nil {
		return nil, err
	}
	if req == nil || len(req.CommandJson) == 0 || len(req.CommandJson) > 1<<20 {
		return nil, status.Error(codes.InvalidArgument, "bounded sandbox command required")
	}
	var command sandboxaccounting.Command
	decoder := json.NewDecoder(bytes.NewBufferString(req.CommandJson))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox command")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, status.Error(codes.InvalidArgument, "one sandbox command required")
	}
	state, replay, err := s.SandboxAccounting.Apply(ctx, command)
	if err != nil {
		if errors.Is(err, ledgerapi.ErrInsufficientFunding) {
			return nil, status.Error(codes.ResourceExhausted, "insufficient sandbox institution prefund")
		}
		return nil, status.Error(codes.FailedPrecondition, "sandbox accounting command rejected")
	}
	return &ledgerv1.SandboxAccountingResponse{EventId: command.EventID, State: state, AlreadyApplied: replay, Revision: command.ExpectedRevision + 1}, nil
}

func (s *Server) PostLocalPrefund(ctx context.Context, req *ledgerv1.LocalPrefundCommand) (*ledgerv1.SettlementCommandResponse, error) {
	if s.Service == nil || s.DB == nil || s.LocalPrefundToken == "" || req == nil || req.Amount == nil || req.CorrelationId == "" {
		return nil, status.Error(codes.PermissionDenied, "local prefund fixture unavailable")
	}
	value := metadata.ValueFromIncomingContext(ctx, "x-osai-local-prefund-token")
	if len(value) != 1 || subtle.ConstantTimeCompare([]byte(value[0]), []byte(s.LocalPrefundToken)) != 1 {
		return nil, status.Error(codes.PermissionDenied, "local prefund fixture unavailable")
	}
	journalID, replay, err := s.Service.PostLocalPrefund(ctx, ledgerapi.PrefundCommand{InstitutionID: req.InstitutionId, Currency: req.Amount.Currency, AmountMinor: req.Amount.AmountMinor, EvidenceReference: req.EvidenceReference, CorrelationID: req.CorrelationId})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &ledgerv1.SettlementCommandResponse{JournalId: journalID, AlreadyApplied: replay}, nil
}

func (s *Server) ReserveFunding(ctx context.Context, req *ledgerv1.FundingReservationCommand) (*ledgerv1.FundingReservationResponse, error) {
	if err := s.authorizeSandbox(ctx); err != nil {
		return nil, err
	}
	if s.Service == nil || req == nil || req.Amount == nil {
		return nil, status.Error(codes.InvalidArgument, "funding command required")
	}
	var snapshot *pricing.Snapshot
	if req.PricingSnapshotJson != "" {
		snapshot = &pricing.Snapshot{}
		if err := json.Unmarshal([]byte(req.PricingSnapshotJson), snapshot); err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid pricing snapshot")
		}
	}
	reservation, replay, err := s.Service.ReserveFunding(ctx, ledgerapi.FundingCommand{
		InstitutionID: req.InstitutionId, TradeID: req.TradeId, Currency: req.Amount.Currency,
		AmountMinor: req.Amount.AmountMinor, SourcePrincipalMinor: req.SourcePrincipalMinor,
		FeeMinor: req.FeeMinor, PricingHash: req.PricingHash, PricingSnapshot: snapshot,
		CorrelationID: req.CorrelationId,
	})
	if err != nil {
		if errors.Is(err, ledgerapi.ErrInsufficientFunding) {
			return nil, status.Error(codes.ResourceExhausted, "insufficient institution prefund")
		}
		if errors.Is(err, ledgerapi.ErrIdempotencyConflict) {
			return nil, status.Error(codes.AlreadyExists, "funding reservation payload conflict")
		}
		log.Printf("ledger funding reservation failed: %v", err)
		return nil, status.Error(codes.Internal, "funding reservation failed")
	}
	return &ledgerv1.FundingReservationResponse{JournalId: reservation.JournalID, State: reservation.State, AlreadyApplied: replay}, nil
}

func (s *Server) GetFundingReservation(ctx context.Context, req *ledgerv1.FundingReservationCommand) (*ledgerv1.FundingReservationResponse, error) {
	if err := s.authorizeSandbox(ctx); err != nil {
		return nil, err
	}
	if s.Service == nil || req == nil || req.Amount == nil {
		return nil, status.Error(codes.InvalidArgument, "funding command required")
	}
	reservation, err := s.Service.GetFundingReservation(ctx, ledgerapi.FundingCommand{InstitutionID: req.InstitutionId, TradeID: req.TradeId, Currency: req.Amount.Currency, AmountMinor: req.Amount.AmountMinor, CorrelationID: req.CorrelationId})
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "funding reservation unavailable")
	}
	return &ledgerv1.FundingReservationResponse{JournalId: reservation.JournalID, State: reservation.State}, nil
}

func (s *Server) GetCustomerBalances(ctx context.Context, req *ledgerv1.CustomerReadRequest) (*ledgerv1.CustomerBalanceResponse, error) {
	if req == nil || req.InstitutionId == "" || s.DB == nil {
		return nil, status.Error(codes.InvalidArgument, "institution and ledger storage required")
	}
	fixture := s.SandboxToken != "" && s.SandboxAccounting != nil && strings.HasPrefix(req.InstitutionId, "sbx:")
	rows, err := s.DB.QueryContext(ctx, `SELECT currency,
 SUM(CASE WHEN account_code='2000' OR ($2 AND account_code='sbx:'||currency||':available') THEN credit-debit ELSE 0 END),
 SUM(CASE WHEN account_code='2100' OR ($2 AND account_code='sbx:'||currency||':held') THEN credit-debit ELSE 0 END),
 SUM(CASE WHEN $2 AND account_code='sbx:'||currency||':restricted' THEN credit-debit ELSE 0 END)
 FROM ledger_entries WHERE owner=$1 AND (account_code IN ('2000','2100') OR ($2 AND account_code IN ('sbx:'||currency||':available','sbx:'||currency||':held','sbx:'||currency||':restricted')))
 GROUP BY currency ORDER BY currency`, req.InstitutionId, fixture)
	if err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	defer rows.Close()
	result := &ledgerv1.CustomerBalanceResponse{}
	for rows.Next() {
		var b ledgerv1.CustomerBalance
		if err = rows.Scan(&b.Currency, &b.AvailableMinor, &b.HeldMinor, &b.RestrictedMinor); err != nil {
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
	fixture := s.SandboxToken != "" && s.SandboxAccounting != nil && strings.HasPrefix(req.InstitutionId, "sbx:")
	if err := s.DB.QueryRowContext(ctx, customerTransactionRead+` SELECT count(*) FROM visible`, req.InstitutionId, fixture).Scan(&count); err != nil {
		return nil, status.Error(codes.Internal, "ledger read failed")
	}
	rows, err := s.DB.QueryContext(ctx, customerTransactionRead+` SELECT journal_id,trade_id,currency,amount_minor,created_at FROM visible ORDER BY created_at DESC,journal_id LIMIT $3 OFFSET $4`, req.InstitutionId, fixture, pageSize, (page-1)*pageSize)
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
	if err := s.authorizeSandbox(ctx); err != nil {
		return nil, err
	}
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
		IdempotencyKey: request.GetIdempotencyKey(), CorrelationID: request.GetCorrelationId(), InstitutionID: request.GetInstitutionId(), SettlementID: request.GetSettlementInstructionId(), TradeID: request.GetTradeId(), ObligationID: request.GetObligationId(), ProviderReference: request.GetProviderReference(), ExternalReference: request.GetExternalReference(), Beneficiary: request.GetBeneficiary(), AmountMinor: amount.GetAmountMinor(), Currency: amount.GetCurrency(), Purpose: request.GetPurpose(),
	})
	if err != nil {
		if errors.Is(err, ledgerapi.ErrIdempotencyConflict) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &ledgerv1.SettlementCommandResponse{JournalId: result, AlreadyApplied: replayed}, nil
}

func (s *Server) authorizeSandbox(ctx context.Context) error {
	if s.SandboxToken == "" {
		return nil
	}
	values := metadata.ValueFromIncomingContext(ctx, sandboxpolicy.TokenHeader)
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(s.SandboxToken)) != 1 {
		return status.Error(codes.PermissionDenied, "sandbox Ledger service authentication required")
	}
	return nil
}
