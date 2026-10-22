package ledgergrpc

import (
	"context"
	"testing"

	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAdjustmentRPCFailsClosedWithoutApprovedTarget(t *testing.T) {
	service := ledgerapi.NewService()
	server := &Server{Service: service}
	response, err := server.PostReconciliationAdjustment(context.Background(), &ledgerv1.ReconciliationAdjustmentCommand{
		IdempotencyKey: "fixture-key", CorrelationId: "fixture-correlation", BreakId: "fixture-break",
		Maker: "maker", Checker: "checker", Currency: "NGN", AmountMinor: 100, Reason: "fixture reason",
	})
	if status.Code(err) != codes.FailedPrecondition || response != nil {
		t.Fatalf("adjustment RPC did not fail closed: %v %v", response, err)
	}
	if len(service.Journals()) != 0 {
		t.Fatal("RPC mutated financial journals")
	}
}
