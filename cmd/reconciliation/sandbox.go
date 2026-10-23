// The sandbox mode runs one bounded, read-only financial comparison for the
// explicit local proof profile. Only reconciliation evidence/breaks are written.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/osai/osai/pkg/postgres"
	"github.com/osai/osai/pkg/sandboxpolicy"
	"github.com/osai/osai/pkg/sandboxrecon"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"os"
	"time"
)

func runSandboxReconciliation() error {
	enabled, err := sandboxpolicy.Read()
	if err != nil || !enabled {
		return errors.New("explicit proof profile required")
	}
	address := os.Getenv("OSAI_LEDGER_GRPC_ADDR")
	host, _, err := net.SplitHostPort(address)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return errors.New("loopback Ledger required")
	}
	client, err := sandboxrecon.New(os.Getenv("OSAI_SANDBOX_RECONCILIATION_URL"), os.Getenv("OSAI_SANDBOX_RECONCILIATION_KEY"))
	if err != nil {
		return err
	}
	institution, trade := os.Getenv("OSAI_SANDBOX_RECONCILIATION_INSTITUTION"), os.Getenv("OSAI_SANDBOX_RECONCILIATION_TRADE_ID")
	if dsn := os.Getenv("OSAI_RECONCILIATION_POSTGRES_DSN"); dsn != "" && !sandboxpolicy.LocalProofDSN(dsn) {
		return errors.New("local proof reconciliation database required")
	}
	db, err := postgres.Open("reconciliation")
	if err != nil {
		return err
	}
	defer db.Close()
	if err = reconciliation.EnsureSchema(db); err != nil {
		return err
	}
	service, err := reconciliation.NewServiceWithStore(&reconciliation.PostgresStore{DB: db})
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := ledgerv1.NewLedgerServiceClient(conn).GetSandboxPosition(sandboxpolicy.Outgoing(ctx), &ledgerv1.SandboxPositionRequest{InstitutionId: institution, TradeId: trade, Reconciliation: true})
	if err != nil {
		return err
	}
	var snapshot sandboxrecon.Snapshot
	if response == nil || json.Unmarshal([]byte(response.ReconciliationJson), &snapshot) != nil || snapshot.InstitutionID != institution || snapshot.TradeID != trade {
		return errors.New("owner reconciliation snapshot unavailable")
	}
	statement, hash, err := client.Read(ctx, institution, trade)
	if err != nil {
		return err
	}
	breaks, err := service.ReconcileSandboxSnapshot(snapshot, statement, hash)
	if err != nil {
		return err
	}
	if len(breaks) > 0 {
		return errors.New("unresolved reconciliation breaks")
	}
	fmt.Println("sandbox reconciliation PASS: independently signed FX/payout/return movements and custody closings match posted Ledger entries")
	return nil
}
