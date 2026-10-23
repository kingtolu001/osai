package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/osai/osai/pkg/correlation"
	"github.com/osai/osai/pkg/observability"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	"google.golang.org/grpc/status"
)

func (s *grpcServer) recoverPendingAcceptances(ctx context.Context, limit int) (int, error) {
	if s == nil || s.store == nil || s.tradeClient == nil || limit < 1 || limit > 100 {
		return 0, errors.New("bounded quote acceptance recovery required")
	}
	processed := 0
	var failures []error
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return processed, errors.Join(append(failures, ctx.Err())...)
		}
		q, err := s.store.ClaimPendingAcceptance(ctx)
		if err != nil {
			return processed, errors.Join(append(failures, err)...)
		}
		if q == nil {
			break
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		call = observability.AttachOutgoingGRPCMetadata(correlation.WithContext(call, q.AcceptanceCorrelation))
		_, err = s.AcceptQuote(call, &quotev1.AcceptQuoteRequest{InstitutionId: q.Request.CustomerID, QuoteId: q.ID, IdempotencyKey: q.AcceptanceKey, BeneficiaryId: q.AcceptanceBeneficiary, CorrelationId: q.AcceptanceCorrelation})
		cancel()
		if err != nil {
			failures = append(failures, err)
		} else {
			processed++
		}
	}
	return processed, errors.Join(failures...)
}

func startQuoteAcceptanceRecovery(ctx context.Context, s *grpcServer) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := s.recoverPendingAcceptances(call, 25)
			cancel()
			if err != nil {
				// Do not expose upstream database/provider diagnostics or tokens.
				log.Printf("quote acceptance recovery remains unresolved: code=%s", status.Code(err))
			}
		}
	}()
}
