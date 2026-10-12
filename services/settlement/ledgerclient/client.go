package ledgerclient

import (
	"context"
	"fmt"

	"github.com/osai/osai/pkg/correlation"
	"github.com/osai/osai/pkg/observability"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/settlement/settlementcore"
	"google.golang.org/grpc"
)

type Client struct {
	client ledgerv1.LedgerServiceClient
}

func New(connection grpc.ClientConnInterface) *Client {
	return &Client{client: ledgerv1.NewLedgerServiceClient(connection)}
}

func (c *Client) VerifyFunding(ctx context.Context, institutionID, tradeID string, amountMinor int64, currency string) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("ledger funding client unavailable")
	}
	response, err := c.client.GetFundingReservation(ctx, &ledgerv1.FundingReservationCommand{InstitutionId: institutionID, TradeId: tradeID, Amount: &ledgerv1.Money{AmountMinor: amountMinor, Currency: currency}})
	if err != nil {
		return err
	}
	if response.State != "ACTIVE" {
		return fmt.Errorf("active funding reservation required")
	}
	return nil
}

func (c *Client) ConfirmSettlement(instruction settlementcore.Instruction) error {
	ctx, span := observability.Start(correlation.WithContext(context.Background(), instruction.CorrelationID), "osai/settlement", "ledger.confirm")
	defer span.End()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	_, err := c.client.ConfirmSettlement(ctx, request(instruction, "confirmed"))
	return err
}

func (c *Client) FailSettlement(instruction settlementcore.Instruction) error {
	ctx, span := observability.Start(correlation.WithContext(context.Background(), instruction.CorrelationID), "osai/settlement", "ledger.fail")
	defer span.End()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
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
		CorrelationId:           instruction.CorrelationID,
		InstitutionId:           instruction.InstitutionID,
		SettlementInstructionId: instruction.ID,
		TradeId:                 instruction.TradeID,
		ProviderReference:       instruction.ProviderRef,
		ExternalReference:       instruction.ClientRef + ":" + effect,
		Beneficiary:             instruction.Beneficiary,
		Amount:                  &ledgerv1.Money{AmountMinor: instruction.AmountMinor, Currency: instruction.Currency},
		Purpose:                 instruction.Purpose,
	}
}

var _ settlementcore.LedgerCommand = (*Client)(nil)

func (c *Client) String() string { return fmt.Sprintf("ledger-client/%T", c.client) }
