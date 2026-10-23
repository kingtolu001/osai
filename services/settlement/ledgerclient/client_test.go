package ledgerclient

import (
	"context"
	"strings"
	"testing"

	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type commandCapture struct {
	method   string
	request  *ledgerv1.SettlementCommand
	metadata metadata.MD
}

func (c *commandCapture) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	c.method = method
	c.request = args.(*ledgerv1.SettlementCommand)
	c.metadata, _ = metadata.FromOutgoingContext(ctx)
	return nil
}

func (*commandCapture) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	panic("unexpected stream")
}

func TestFinalityCommandsPreserveAuthorityAndIdentity(t *testing.T) {
	for key, value := range map[string]string{
		"OSAI_SANDBOX_ACCOUNTING_POLICY": sandboxpolicy.Version,
		"OSAI_LOCAL_PROOF_MODE":          "true", "OSAI_API_ENVIRONMENT": "sandbox",
		"FLW_ENV": "test", "FLW_BASE_URL": "http://127.0.0.1:1234/v3",
		"FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": strings.Repeat("x", 32),
		"OSAI_POSTGRES_DSN":       "postgres://127.0.0.1:5433/osai_proof_client?sslmode=disable",
		"OSAI_TRADE_POSTGRES_DSN": "", "OSAI_SETTLEMENT_POSTGRES_DSN": "",
		"OSAI_LEDGER_POSTGRES_DSN": "", "OSAI_QUOTE_POSTGRES_DSN": "",
	} {
		t.Setenv(key, value)
	}
	capture := &commandCapture{}
	client := New(capture)
	instruction := settlementcore.Instruction{ID: "si-fixture", InstitutionID: "sbx:institution", TradeID: "sbx:trade", ClientRef: "stable-ref", CorrelationID: "correlation", ProviderRef: "observed-provider-ref", AmountMinor: 123, Currency: "USD"}
	for _, tc := range []struct {
		effect, method string
		call           func(settlementcore.Instruction) error
	}{
		{"confirmed", "ConfirmSettlement", client.ConfirmSettlement},
		{"failed", "FailSettlement", client.FailSettlement},
		{"reversed", "ReverseSettlement", client.ReverseSettlement},
	} {
		t.Run(tc.effect, func(t *testing.T) {
			if err := tc.call(instruction); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(capture.method, "/"+tc.method) {
				t.Fatal("wrong finality RPC")
			}
			if got := capture.metadata.Get(sandboxpolicy.TokenHeader); len(got) != 1 || got[0] != strings.Repeat("x", 32) {
				t.Fatal("sandbox authority missing")
			}
			r := capture.request
			if r.IdempotencyKey != "si:stable-ref:"+tc.effect || r.TradeId != instruction.TradeID || r.SettlementInstructionId != instruction.ID || r.ProviderReference != instruction.ProviderRef || r.CorrelationId != instruction.CorrelationID || r.Amount.AmountMinor != 123 || r.Amount.Currency != "USD" {
				t.Fatal("finality identity or economics changed")
			}
		})
	}
	// A disabled profile must not attach local authority to ordinary RPCs.
	t.Setenv("OSAI_SANDBOX_ACCOUNTING_POLICY", "")
	if err := client.ReverseSettlement(instruction); err != nil {
		t.Fatal(err)
	}
	if len(capture.metadata.Get(sandboxpolicy.TokenHeader)) != 0 {
		t.Fatal("fixture authority leaked outside profile")
	}
}
