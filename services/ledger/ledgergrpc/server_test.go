package ledgergrpc

import (
	"context"
	"net"
	"testing"

	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/ledger/ledgerapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestLedgerGRPCCommandsAreIdempotentAndRejectConflicts(t *testing.T) {
	const bufferSize = 1024 * 1024
	listener := bufconn.Listen(bufferSize)
	server := grpc.NewServer()
	service := ledgerapi.NewService()
	ledgerv1.RegisterLedgerServiceServer(server, &Server{Service: service})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	connection, err := grpc.DialContext(context.Background(), "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := ledgerv1.NewLedgerServiceClient(connection)
	request := &ledgerv1.SettlementCommand{IdempotencyKey: "idem-1", SettlementInstructionId: "si_1", ExternalReference: "si_1:confirmed", Beneficiary: "acct_1", Amount: &ledgerv1.Money{AmountMinor: 100, Currency: "USD"}}
	callContext := metadata.AppendToOutgoingContext(context.Background(), "x-osai-correlation-id", "cor_metadata")
	first, err := client.ConfirmSettlement(callContext, request)
	if err != nil || first.GetJournalId() == "" || first.GetAlreadyApplied() {
		t.Fatalf("unexpected first command: %+v %v", first, err)
	}
	second, err := client.ConfirmSettlement(callContext, request)
	if err != nil || !second.GetAlreadyApplied() {
		t.Fatalf("expected idempotent replay: %+v %v", second, err)
	}
	request.Amount.AmountMinor = 101
	_, err = client.ConfirmSettlement(callContext, request)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
	if correlations := service.Correlations(); len(correlations) == 0 || correlations[0] != "cor_metadata" {
		t.Fatalf("correlation metadata was not propagated: %v", correlations)
	}
	if len(service.Journals()) != 1 {
		t.Fatalf("expected one journal, got %d", len(service.Journals()))
	}
}
