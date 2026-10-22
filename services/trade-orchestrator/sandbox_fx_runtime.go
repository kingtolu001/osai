package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/osai/osai/pkg/sandboxpolicy"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
)

func startSandboxFXRecovery(ctx context.Context, store *tradecore.Store) error {
	endpoint, key, id := os.Getenv("OSAI_SANDBOX_FX_BASE_URL"), os.Getenv("OSAI_SANDBOX_FX_SIGNING_KEY"), os.Getenv("OSAI_SANDBOX_FX_EXECUTOR_ID")
	if endpoint == "" && key == "" && id == "" {
		return nil
	}
	enabled, err := sandboxpolicy.Read()
	if err != nil || !enabled {
		return errors.New("sandbox FX recovery requires the explicit local accounting profile")
	}
	client, err := tradecore.NewSandboxFXClient(endpoint, id, key)
	if err != nil {
		return err
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
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := store.RecoverSandboxFXBatch(callCtx, client, 25)
			cancel()
			if err != nil {
				log.Printf("sandbox FX recovery remains unresolved: %v", err)
			}
		}
	}()
	return nil
}
