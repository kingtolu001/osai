package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/provider"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type confirmedSettlementLister interface {
	ListConfirmedSettlements(context.Context, *settlementv1.ListConfirmedSettlementsRequest, ...grpc.CallOption) (*settlementv1.ListConfirmedSettlementsResponse, error)
}

// reconcileFlutterwavePass reads only terminal settlement expectations through
// the settlement RPC, and reads provider statements through SettlementRail.
// Repeating the same window is safe because evidence and break IDs are stable.
func reconcileFlutterwavePass(ctx context.Context, svc *reconciliation.Service, client confirmedSettlementLister, rail provider.SettlementRail, token string, now time.Time, expectedFeeMinor int64) error {
	if svc == nil || client == nil || rail == nil || token == "" || expectedFeeMinor < 0 {
		return errors.New("automatic reconciliation configuration unavailable")
	}
	to := now.UTC().Add(-5 * time.Minute)
	from := to.Add(-7 * 24 * time.Hour)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-osai-reconciliation-token", token)
	var cursor string
	for page := 0; page < 1000; page++ {
		batch, err := client.ListConfirmedSettlements(ctx, &settlementv1.ListConfirmedSettlementsRequest{ProviderId: "flutterwave", FromUnix: from.Unix(), ToUnix: to.Unix(), AfterSettlementId: cursor, PageSize: 100})
		if err != nil {
			return fmt.Errorf("settlement reconciliation snapshot: %w", err)
		}
		if batch == nil {
			return errors.New("settlement reconciliation snapshot unavailable")
		}
		for _, item := range batch.Settlements {
			if item == nil || item.ProviderId != "flutterwave" || item.Status != "CONFIRMED" || item.SettlementId == "" || item.ClientRef == "" || item.ProviderRef == "" || item.AmountMinor <= 0 || item.Currency != "NGN" {
				return errors.New("settlement reconciliation snapshot incomplete")
			}
			window := flutterwave.TransactionWindow{From: from, To: now.UTC(), PageSize: 100, Reference: item.ClientRef}
			expected := reconciliation.ExpectedTransaction{ID: item.SettlementId, Provider: item.ProviderId, ClientRef: item.ClientRef, ProviderRef: item.ProviderRef, AmountMinor: item.AmountMinor, Currency: item.Currency, Beneficiary: item.Beneficiary, FeeMinor: expectedFeeMinor}
			if _, err := svc.ReconcileFlutterwaveWindow(rail, window, to, []reconciliation.ExpectedTransaction{expected}); err != nil {
				return fmt.Errorf("provider reconciliation for settlement %s: %w", item.SettlementId, err)
			}
		}
		if batch.NextSettlementId == "" {
			return nil
		}
		if batch.NextSettlementId <= cursor {
			return errors.New("settlement reconciliation cursor did not advance")
		}
		cursor = batch.NextSettlementId
	}
	return errors.New("settlement reconciliation page limit reached: " + strconv.Itoa(1000))
}
