package main

import (
	"context"
	"testing"
)

func TestSandboxFXLedgerWorkerDisabledUnlessExplicitlyConfigured(t *testing.T) {
	t.Setenv("OSAI_SANDBOX_FX_LEDGER_POSTING", "")
	if err := startSandboxFXLedgerPosting(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{"true", "false", "production"} {
		t.Setenv("OSAI_SANDBOX_FX_LEDGER_POSTING", selected)
		if err := startSandboxFXLedgerPosting(context.Background(), nil, nil); err == nil {
			t.Fatal("worker started without an authenticated local proof Ledger")
		}
	}
}
