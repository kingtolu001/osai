package ledgergrpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/osai/osai/pkg/sandboxaccounting"
	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type positionReaderFixture struct {
	accountingWriterFixture
	reads int
}

func (r *positionReaderFixture) Position(_ context.Context, institution, trade string) (sandboxaccounting.Position, error) {
	r.reads++
	if institution != "sbx:institution" {
		return sandboxaccounting.Position{}, errors.New("private SQL diagnostic")
	}
	return sandboxaccounting.Position{Terms: sandboxaccounting.Terms{TradeID: trade, InstitutionID: institution}, State: "FX_HELD", Revision: 3}, nil
}

func TestSandboxPositionRPCRequiresAuthenticationAndSanitizesFailures(t *testing.T) {
	reader := &positionReaderFixture{}
	s := &Server{SandboxToken: "local-position-token", SandboxAccounting: reader}
	req := &ledgerv1.SandboxPositionRequest{InstitutionId: "sbx:institution", TradeId: "sbx:trade"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, s.SandboxToken))
	for _, server := range []*Server{{}, {SandboxToken: s.SandboxToken}, {SandboxAccounting: reader}} {
		if _, err := server.GetSandboxPosition(ctx, req); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("disabled snapshot route allowed: %v", err)
		}
	}
	if _, err := s.GetSandboxPosition(context.Background(), req); status.Code(err) != codes.PermissionDenied || reader.reads != 0 {
		t.Fatal("unauthenticated snapshot reached storage")
	}
	if _, err := s.GetSandboxPosition(ctx, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatal("nil snapshot identity accepted")
	}
	response, err := s.GetSandboxPosition(ctx, req)
	if err != nil || !strings.Contains(response.PositionJson, `"Revision":3`) || reader.reads != 1 {
		t.Fatalf("snapshot: %v %v", response, err)
	}
	req.InstitutionId = "sbx:other"
	if _, err := s.GetSandboxPosition(ctx, req); status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe failure response: %v", err)
	}
}
