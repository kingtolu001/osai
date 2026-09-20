package ledgerclient

import (
	"context"
	"fmt"

	"github.com/osai/osai/pkg/observability"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type Client struct {
	client ledgerv1.LedgerServiceClient
}

func New(connection grpc.ClientConnInterface) *Client {
	return &Client{client: ledgerv1.NewLedgerServiceClient(connection)}
}

func (c *Client) ConfirmSettlement(instruction settlementcore.Instruction) error {
	ctx, span := observability.Start(context.Background(), "osai/settlement", "ledger.confirm")
	defer span.End()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-osai-correlation-id", "cor_"+instruction.ClientRef)
	_, err := c.client.ConfirmSettlement(ctx, request(instruction, "confirmed"))
	return err
}

func (c *Client) FailSettlement(instruction settlementcore.Instruction) error {
	ctx, span := observability.Start(context.Background(), "osai/settlement", "ledger.fail")
	defer span.End()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-osai-correlation-id", "cor_"+instruction.ClientRef)
	_, err := c.client.FailSettlement(ctx, request(instruction, "failed"))
	return err
}

func (c *Client) ReverseSettlement(instruction settlementcore.Instruction) error {
	_, err := c.client.ReverseSettlement(context.Background(), request(instruction, "reversed"))
	return err
}

func request(instruction settlementcore.Instruction, effect string) *ledgerv1.SettlementCommand {
	return &ledgerv1.SettlementCommand{
		IdempotencyKey:          "si:" + instruction.ClientRef + ":" + effect,
		CorrelationId:           "cor_" + instruction.ClientRef,
		SettlementInstructionId: instruction.ID,
		ProviderReference:       instruction.ProviderRef,
		ExternalReference:       instruction.ClientRef + ":" + effect,
		Beneficiary:             instruction.Beneficiary,
		Amount:                  &ledgerv1.Money{AmountMinor: instruction.AmountMinor, Currency: instruction.Currency},
		Purpose:                 instruction.Purpose,
	}
}

var _ settlementcore.LedgerCommand = (*Client)(nil)

func (c *Client) String() string { return fmt.Sprintf("ledger-client/%T", c.client) }
