// Package sandboxpolicy selects the local accounting fixture profile. It is
// deliberately incompatible with actual-provider and production configuration.
package sandboxpolicy

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"

	"google.golang.org/grpc/metadata"
)

const Version = "osai-sandbox-accounting/v1"
const TokenHeader = "x-osai-sandbox-ledger-token"

// FXAcceptance enables only the local NGN/USD fixture lifecycle. It is not an
// approval to route cross-currency transactions to a real provider.
func FXAcceptance() (bool, error) {
	selected := os.Getenv("OSAI_SANDBOX_FX_ACCEPTANCE")
	if selected == "" {
		return false, nil
	}
	enabled, err := Read()
	base, parseErr := url.Parse(os.Getenv("OSAI_SANDBOX_FX_BASE_URL"))
	local := parseErr == nil && base != nil && base.Scheme == "http" && base.User == nil && base.RawQuery == "" && base.Fragment == "" && (base.Hostname() == "127.0.0.1" || base.Hostname() == "localhost" || base.Hostname() == "::1") && (base.Path == "" || base.Path == "/sandbox-fx")
	if selected != "true" || err != nil || !enabled || os.Getenv("OSAI_SANDBOX_FX_LEDGER_POSTING") != "true" || !local || !strings.HasPrefix(os.Getenv("OSAI_SANDBOX_FX_EXECUTOR_ID"), "sbx:") || !strings.HasPrefix(os.Getenv("OSAI_SANDBOX_FX_SIGNING_KEY"), "local-") || len(os.Getenv("OSAI_SANDBOX_FX_SIGNING_KEY")) < 32 {
		return false, errors.New("sandbox FX acceptance requires the complete local executor and Ledger posting profile")
	}
	return true, nil
}

func FXTradeAllowed(institution, source, destination, provider string) (bool, error) {
	enabled, err := FXAcceptance()
	return enabled && strings.HasPrefix(institution, "sbx:") && source == "NGN" && destination == "USD" && provider == "sbx:provider", err
}

// FXPayout cannot enable a real-provider route or a non-proof database.
func FXPayout() (bool, error) {
	selected := os.Getenv("OSAI_SANDBOX_FX_PAYOUT")
	if selected == "" {
		return false, nil
	}
	enabled, err := FXAcceptance()
	if selected != "true" || err != nil || !enabled {
		return false, errors.New("sandbox FX payout requires the complete local FX acceptance profile")
	}
	return true, nil
}

func Read() (bool, error) {
	policy := os.Getenv("OSAI_SANDBOX_ACCOUNTING_POLICY")
	if policy == "" {
		return false, nil
	}
	provider, providerErr := url.Parse(os.Getenv("FLW_BASE_URL"))
	localProvider := providerErr == nil && provider != nil && provider.Scheme == "http" && provider.User == nil && provider.RawQuery == "" && provider.Fragment == "" && provider.Path == "/v3" && (provider.Hostname() == "127.0.0.1" || provider.Hostname() == "localhost" || provider.Hostname() == "::1")
	if policy != Version || os.Getenv("OSAI_LOCAL_PROOF_MODE") != "true" || os.Getenv("OSAI_API_ENVIRONMENT") != "sandbox" || os.Getenv("FLW_ENV") != "test" || !localProvider || !strings.HasPrefix(os.Getenv("FLW_SECRET_KEY"), "local-") || len(os.Getenv("OSAI_SANDBOX_LEDGER_TOKEN")) < 32 || !LocalProofDSN(os.Getenv("OSAI_POSTGRES_DSN")) {
		return false, errors.New("sandbox accounting requires the local proof profile, loopback proof database and service token")
	}
	for _, key := range []string{"OSAI_TRADE_POSTGRES_DSN", "OSAI_SETTLEMENT_POSTGRES_DSN", "OSAI_LEDGER_POSTGRES_DSN", "OSAI_QUOTE_POSTGRES_DSN"} {
		if dsn := os.Getenv(key); dsn != "" && !LocalProofDSN(dsn) {
			return false, errors.New("sandbox service database override must remain a local proof database")
		}
	}
	return true, nil
}
func LocalProofDSN(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil || u == nil {
		return false
	}
	if (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") || !strings.HasPrefix(u.Path, "/osai_proof_") || u.Fragment != "" {
		return false
	}
	allowed := map[string]bool{"sslmode": true, "search_path": true, "application_name": true, "connect_timeout": true}
	for key := range u.Query() {
		if !allowed[key] {
			return false
		}
	}
	return true
}
func Outgoing(ctx context.Context) context.Context {
	enabled, err := Read()
	if err != nil || !enabled {
		return ctx
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(TokenHeader, os.Getenv("OSAI_SANDBOX_LEDGER_TOKEN"))
	return metadata.NewOutgoingContext(ctx, md)
}
