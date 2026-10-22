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
	for _, key := range []string{"OSAI_TRADE_POSTGRES_DSN", "OSAI_SETTLEMENT_POSTGRES_DSN", "OSAI_LEDGER_POSTGRES_DSN"} {
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
