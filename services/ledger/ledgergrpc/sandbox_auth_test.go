package ledgergrpc

import (
	"context"
	"github.com/osai/osai/pkg/sandboxpolicy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"testing"
)

func TestSandboxMoneyCommandsRequireServiceAuthentication(t *testing.T) {
	server := &Server{SandboxToken: "fixture-service-token"}
	for _, ctx := range []context.Context{context.Background(), metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, "wrong")), metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, "fixture-service-token", sandboxpolicy.TokenHeader, "fixture-service-token"))} {
		if _, err := server.ReserveFunding(ctx, nil); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("reserve authentication bypass: %v", err)
		}
		if _, err := server.GetFundingReservation(ctx, nil); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("verify authentication bypass: %v", err)
		}
		if _, err := server.ConfirmSettlement(ctx, nil); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("finality authentication bypass: %v", err)
		}
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, "fixture-service-token"))
	if _, err := server.ReserveFunding(ctx, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("valid service token rejected: %v", err)
	}
}
