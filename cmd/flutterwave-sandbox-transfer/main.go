package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// This operator command creates one explicitly configured sandbox SI through
// the existing settlement service. It never holds Flutterwave credentials.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if os.Getenv("FLW_ENV") != "sandbox" || os.Getenv("FLW_SETTLEMENT_ENABLED") != "true" {
		return errors.New("Flutterwave sandbox settlement is disabled")
	}
	institution := os.Getenv("OSAI_FLW_TEST_INSTITUTION_ID")
	trade := os.Getenv("OSAI_FLW_TEST_TRADE_ID")
	quote := os.Getenv("OSAI_FLW_TEST_QUOTE_ID")
	beneficiaryID := os.Getenv("OSAI_FLW_TEST_BENEFICIARY_ID")
	amount, err := strconv.ParseInt(os.Getenv("OSAI_FLW_TEST_AMOUNT_MINOR"), 10, 64)
	if institution == "" || trade == "" || quote == "" || beneficiaryID == "" || err != nil || amount <= 0 || amount%100 != 0 {
		return errors.New("institution, stable trade/quote IDs, approved beneficiary ID and whole-NGN amount required")
	}
	addr := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if addr == "" {
		addr = "127.0.0.1:50055"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "localhost" && host != "127.0.0.1" {
		return errors.New("sandbox command requires a loopback settlement service address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return errors.New("settlement service unavailable")
	}
	defer connection.Close()
	response, err := settlementv1.NewSettlementServiceClient(connection).CreateSettlementForTrade(ctx, &settlementv1.CreateSettlementForTradeRequest{
		InstitutionId: institution, TradeId: trade, QuoteId: quote, IdempotencyKey: trade, CorrelationId: "corr_" + trade, ProviderId: "flutterwave", BeneficiaryId: beneficiaryID, Amount: &settlementv1.Money{AmountMinor: amount, Currency: "NGN"}, Purpose: "controlled sandbox transfer",
	})
	if err != nil {
		return errors.New("sandbox settlement request failed")
	}
	fmt.Printf("settlement_id=%s status=%s provider=%s\n", response.SettlementId, response.Status, response.ProviderId)
	return nil
}
