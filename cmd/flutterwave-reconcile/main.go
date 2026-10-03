package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osai/osai/adapters/flutterwave"
	"github.com/osai/osai/pkg/postgres"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// This controlled one-shot ingests a single settled transfer's provider
// statement into reconciliation storage; it never posts Ledger journals.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if os.Getenv("FLW_ENV") != "sandbox" || os.Getenv("FLW_SETTLEMENT_ENABLED") != "true" || !strings.HasPrefix(os.Getenv("FLW_SECRET_KEY"), "FLWSECK_TEST-") {
		return errors.New("Flutterwave sandbox reconciliation unavailable")
	}
	institution := os.Getenv("OSAI_FLW_TEST_INSTITUTION_ID")
	settlementID := os.Getenv("OSAI_FLW_TEST_SETTLEMENT_ID")
	from, fromErr := time.Parse("2006-01-02", os.Getenv("OSAI_FLW_RECON_FROM"))
	to, toErr := time.Parse("2006-01-02", os.Getenv("OSAI_FLW_RECON_TO"))
	fee, feeErr := strconv.ParseInt(os.Getenv("OSAI_FLW_EXPECTED_FEE_MINOR"), 10, 64)
	if institution == "" || settlementID == "" || fromErr != nil || toErr != nil || to.Before(from) || feeErr != nil || fee < 0 {
		return errors.New("institution, settlement ID, reconciliation dates and approved expected fee required")
	}
	addr := os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR")
	if addr == "" {
		addr = "127.0.0.1:50055"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "localhost" && host != "127.0.0.1" {
		return errors.New("reconciliation command requires loopback settlement address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return errors.New("settlement service unavailable")
	}
	defer conn.Close()
	settlement, err := settlementv1.NewSettlementServiceClient(conn).GetSettlement(ctx, &settlementv1.GetSettlementRequest{InstitutionId: institution, SettlementId: settlementID})
	if err != nil || settlement.ProviderId != "flutterwave" || settlement.Status != "CONFIRMED" || settlement.ClientRef == "" || settlement.ProviderRef == "" {
		return errors.New("confirmed Flutterwave settlement evidence unavailable")
	}
	db, err := postgres.Open("reconciliation")
	if err != nil {
		return errors.New("reconciliation storage unavailable")
	}
	defer db.Close()
	if err = reconciliation.EnsureSchema(db); err != nil {
		return errors.New("reconciliation schema unavailable")
	}
	service := reconciliation.NewServiceWithStore(&reconciliation.PostgresStore{DB: db})
	rail := flutterwave.New(flutterwave.Config{BaseURL: os.Getenv("FLW_BASE_URL"), SecretKey: os.Getenv("FLW_SECRET_KEY"), Env: "sandbox", Enabled: true})
	expected := reconciliation.ExpectedTransaction{ID: settlement.SettlementId, Provider: "flutterwave", ProviderRef: settlement.ProviderRef, ClientRef: settlement.ClientRef, AmountMinor: settlement.AmountMinor, Currency: settlement.Currency, Beneficiary: settlement.Beneficiary, FeeMinor: fee, Watermark: os.Getenv("OSAI_FLW_EXPECTED_WATERMARK")}
	breaks, err := service.ReconcileFlutterwaveWindow(rail, flutterwave.TransactionWindow{From: from, To: to, PageSize: 100, Reference: settlement.ClientRef}, to.Add(24*time.Hour), []reconciliation.ExpectedTransaction{expected})
	if err != nil {
		return errors.New("provider statement reconciliation unavailable")
	}
	fmt.Printf("settlement_id=%s reconciliation_breaks=%d\n", settlement.SettlementId, len(breaks))
	for _, brk := range breaks {
		fmt.Printf("break_id=%s type=%s\n", brk.ID, brk.Type)
	}
	return nil
}
