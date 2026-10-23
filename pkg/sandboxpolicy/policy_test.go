package sandboxpolicy

import (
	"context"
	"google.golang.org/grpc/metadata"
	"strings"
	"testing"
)

func TestSandboxPolicyRejectsActualProviderAndProduction(t *testing.T) {
	settings := map[string]string{"OSAI_SANDBOX_ACCOUNTING_POLICY": Version, "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": "http://127.0.0.1:1234/v3", "FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": strings.Repeat("x", 32), "OSAI_POSTGRES_DSN": "postgres://127.0.0.1:5433/osai_proof_policy?sslmode=disable"}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	if enabled, err := Read(); err != nil || !enabled {
		t.Fatal("valid proof profile rejected")
	}
	outgoing, _ := metadata.FromOutgoingContext(Outgoing(context.Background()))
	if len(outgoing.Get(TokenHeader)) != 1 {
		t.Fatal("authenticated metadata absent")
	}
	for _, tc := range []struct{ key, value string }{{"FLW_ENV", "sandbox"}, {"FLW_BASE_URL", "https://api.flutterwave.com/v3"}, {"FLW_SECRET_KEY", "FLWSECK_TEST-not-a-secret"}, {"OSAI_POSTGRES_DSN", "postgres://127.0.0.1/osai"}, {"OSAI_POSTGRES_DSN", "postgres://127.0.0.1/osai_proof_policy?host=remote"}, {"OSAI_API_ENVIRONMENT", "production"}, {"OSAI_SANDBOX_LEDGER_TOKEN", ""}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if enabled, err := Read(); enabled || err == nil {
				t.Fatal("unsafe sandbox configuration accepted")
			}
		})
	}
	t.Setenv("OSAI_SANDBOX_ACCOUNTING_POLICY", "")
	if enabled, err := Read(); enabled || err != nil {
		t.Fatal("unset profile did not preserve default refusal")
	}
}

func TestSandboxFXAcceptanceRequiresCompleteFixtureProfile(t *testing.T) {
	settings := map[string]string{"OSAI_SANDBOX_ACCOUNTING_POLICY": Version, "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": "http://127.0.0.1:1234/v3", "FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": strings.Repeat("x", 32), "OSAI_POSTGRES_DSN": "postgres://127.0.0.1:5433/osai_proof_policy?sslmode=disable", "OSAI_SANDBOX_FX_ACCEPTANCE": "true", "OSAI_SANDBOX_FX_LEDGER_POSTING": "true", "OSAI_SANDBOX_FX_BASE_URL": "http://127.0.0.1:1235", "OSAI_SANDBOX_FX_EXECUTOR_ID": "sbx:executor", "OSAI_SANDBOX_FX_SIGNING_KEY": "local-" + strings.Repeat("x", 32)}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	if allowed, err := FXTradeAllowed("sbx:institution", "NGN", "USD", "sbx:provider"); err != nil || !allowed {
		t.Fatalf("fixture rejected: %v", err)
	}
	for _, args := range [][4]string{{"real-institution", "NGN", "USD", "sbx:provider"}, {"sbx:institution", "NGN", "USD", "flutterwave"}, {"sbx:institution", "USD", "NGN", "sbx:provider"}} {
		if allowed, _ := FXTradeAllowed(args[0], args[1], args[2], args[3]); allowed {
			t.Fatal("unsupported real-money path enabled")
		}
	}
	for _, tc := range []struct{ key, value string }{{"FLW_ENV", "sandbox"}, {"OSAI_SANDBOX_FX_LEDGER_POSTING", ""}, {"OSAI_SANDBOX_FX_BASE_URL", "http://remote.example"}, {"OSAI_SANDBOX_FX_BASE_URL", "http://127.0.0.1?host=remote"}, {"OSAI_SANDBOX_FX_SIGNING_KEY", ""}, {"OSAI_SANDBOX_FX_EXECUTOR_ID", "real-executor"}} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if enabled, err := FXAcceptance(); enabled || err == nil {
				t.Fatal("incomplete or unsafe FX profile accepted")
			}
		})
	}
	t.Setenv("OSAI_SANDBOX_FX_ACCEPTANCE", "")
	if enabled, err := FXAcceptance(); enabled || err != nil {
		t.Fatal("default refusal changed")
	}
}
