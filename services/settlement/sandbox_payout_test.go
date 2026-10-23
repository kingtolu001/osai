package main

import (
	"context"
	"testing"

	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"github.com/osai/osai/services/settlement/settlementstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSandboxPayoutDisabledUnlessCompleteProfileSelected(t *testing.T) {
	t.Setenv("OSAI_SANDBOX_FX_PAYOUT", "")
	runtime, err := startSandboxPayout(context.Background(), settlementstore.Postgres{})
	if runtime != nil || err != nil {
		t.Fatal("disabled payout profile touched runtime dependencies")
	}
	for _, selected := range []string{"true", "false", "production"} {
		t.Setenv("OSAI_SANDBOX_FX_PAYOUT", selected)
		t.Setenv("OSAI_SANDBOX_FX_ACCEPTANCE", "")
		if _, err := startSandboxPayout(context.Background(), settlementstore.Postgres{}); err == nil {
			t.Fatal("incomplete payout profile accepted")
		}
	}
}

func TestSandboxPayoutCreationUnavailableWithoutAuthenticatedRuntime(t *testing.T) {
	s := &settlementGRPCServer{store: settlementcore.NewStore(nil)}
	req := &settlementv1.CreateSettlementForTradeRequest{InstitutionId: "sbx:institution", TradeId: "trd_fixture", QuoteId: "sbx:quote", ProviderId: "sbx:provider", BeneficiaryId: "sbx:beneficiary", Amount: &settlementv1.Money{AmountMinor: 95, Currency: "USD"}}
	if _, err := s.CreateSettlementForTrade(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unconfigured fixture route allowed: %v", err)
	}
}
