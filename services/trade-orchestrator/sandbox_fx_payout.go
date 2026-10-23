package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"github.com/osai/osai/pkg/sandboxpolicy"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
)

func startSandboxFXPayout(ctx context.Context, store *tradecore.Store, client settlementv1.SettlementServiceClient) error {
	enabled, err := sandboxpolicy.FXPayout()
	if err != nil || !enabled {
		return err
	}
	host, _, addressErr := net.SplitHostPort(os.Getenv("OSAI_SETTLEMENT_GRPC_ADDR"))
	if client == nil || addressErr != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return errors.New("sandbox payout relay requires explicit loopback Settlement target")
	}
	if err = store.ValidateSandboxFXRecovery(ctx); err != nil {
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
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := store.RelaySandboxFXPayouts(call, client, 25)
			cancel()
			if err != nil {
				log.Printf("sandbox destination payout remains unresolved")
			}
		}
	}()
	return nil
}
