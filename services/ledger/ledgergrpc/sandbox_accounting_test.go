package ledgergrpc

import (
	"context"
	"encoding/json"
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

type accountingWriterFixture struct {
	calls int
	fail  bool
}

func (w *accountingWriterFixture) Apply(_ context.Context, c sandboxaccounting.Command) (string, bool, error) {
	w.calls++
	if w.fail {
		return "", false, errors.New("private database diagnostic")
	}
	return "FX_HELD", true, nil
}

func TestSandboxAccountingRPCRequiresExplicitProfileAndAuthentication(t *testing.T) {
	writer := &accountingWriterFixture{}
	server := &Server{SandboxToken: "local-fixture-token", SandboxAccounting: writer}
	command, _ := json.Marshal(sandboxaccounting.Command{EventID: "sbx:execution"})
	request := &ledgerv1.SandboxAccountingCommand{CommandJson: string(command)}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, server.SandboxToken))
	for _, disabled := range []*Server{{}, {SandboxToken: server.SandboxToken}, {SandboxAccounting: writer}} {
		if _, err := disabled.ApplySandboxAccounting(ctx, request); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("disabled route allowed: %v", err)
		}
	}
	for _, denied := range []context.Context{context.Background(), metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, "wrong")), metadata.NewIncomingContext(context.Background(), metadata.Pairs(sandboxpolicy.TokenHeader, server.SandboxToken, sandboxpolicy.TokenHeader, server.SandboxToken))} {
		if _, err := server.ApplySandboxAccounting(denied, request); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("authentication bypass: %v", err)
		}
	}
	for _, raw := range []string{"", "{} {}", `{"unrecognised":true}`, strings.Repeat("x", (1<<20)+1)} {
		if _, err := server.ApplySandboxAccounting(ctx, &ledgerv1.SandboxAccountingCommand{CommandJson: raw}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("malformed command accepted: %v", err)
		}
	}
	if writer.calls != 0 {
		t.Fatal("invalid or unauthenticated request reached accounting")
	}
	response, err := server.ApplySandboxAccounting(ctx, request)
	if err != nil || response.EventId != "sbx:execution" || response.State != "FX_HELD" || !response.AlreadyApplied || writer.calls != 1 {
		t.Fatalf("receipt: %v %v", response, err)
	}
	writer.fail = true
	if _, err := server.ApplySandboxAccounting(ctx, request); status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe failure response: %v", err)
	}
}
