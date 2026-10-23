package main

import (
	"context"

	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/sandboxaccounting"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *tradeServer) createSandboxFXTrade(ctx context.Context, req *tradev1.CreateTradeFromAcceptedQuoteRequest, snapshot pricing.Snapshot) (*tradev1.TradeResponse, error) {
	if s.customerClient == nil || s.ledgerClient == nil {
		return nil, status.Error(codes.Unavailable, "sandbox FX requires Customer and Ledger services")
	}
	if !sandboxaccounting.ValidBeneficiaryID(req.BeneficiaryId) {
		return nil, status.Error(codes.InvalidArgument, "valid sandbox FX beneficiary required")
	}
	if err := s.validateApprovedBeneficiary(ctx, req.InstitutionId, req.BeneficiaryId); err != nil {
		return nil, err
	}
	trade, err := s.store.CreateTradeWithPricing(req.InstitutionId, req.QuoteId, req.IdempotencyKey, req.CorrelationId, snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot, req.BeneficiaryId, req.SettlementProviderId, req.CustomerReference)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox FX trade conflicts with accepted pricing")
	}
	if _, err := s.store.ReserveSandboxFX(ctx, trade.ID, s.ledgerClient); err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			if err := s.store.SetSandboxFundingRequired(trade.ID); err != nil {
				return nil, status.Error(codes.Internal, "sandbox funding state persistence failed")
			}
			current, found := s.store.GetTrade(trade.ID)
			if !found {
				return nil, status.Error(codes.Internal, "sandbox funding state unavailable")
			}
			return tradeResponse(current, current.Status), nil
		}
		return nil, status.Error(codes.FailedPrecondition, "sandbox FX source reservation unresolved")
	}
	trade, found := s.store.GetTrade(trade.ID)
	if !found {
		return nil, status.Error(codes.Internal, "sandbox FX trade state unavailable")
	}
	// The optional destination relay waits for the verified Ledger FX posting.
	// Never infer USD custody from an FX acknowledgement or submit the NGN debit
	// as the destination payout amount.
	return tradeResponse(trade, trade.Status), nil
}
