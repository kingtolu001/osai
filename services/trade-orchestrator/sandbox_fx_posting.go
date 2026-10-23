package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"github.com/osai/osai/pkg/sandboxpolicy"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
)

func startSandboxFXLedgerPosting(ctx context.Context, store *tradecore.Store, client ledgerv1.LedgerServiceClient) error {
	selected := os.Getenv("OSAI_SANDBOX_FX_LEDGER_POSTING")
	if selected == "" {
		return nil
	}
	enabled, err := sandboxpolicy.Read()
	host, _, addressErr := net.SplitHostPort(os.Getenv("OSAI_LEDGER_GRPC_ADDR"))
	if selected != "true" || err != nil || !enabled || client == nil || addressErr != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return errors.New("sandbox FX Ledger posting requires the local proof profile and explicit loopback Ledger target")
	}
	if err := store.ValidateSandboxFXRecovery(ctx); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := store.RelaySandboxFXPostings(callCtx, client, 25)
			cancel()
			if err != nil {
				log.Printf("sandbox FX Ledger posting remains unresolved: %v", err)
			}
		}
	}()
	return nil
}
