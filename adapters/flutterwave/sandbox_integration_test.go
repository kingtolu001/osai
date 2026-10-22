package flutterwave

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
)

// This opt-in calls the actual provider with test credentials and documented
// Flutterwave test accounts. It does not post Ledger journals or use real money.
func TestFlutterwaveActualSandboxTransferContract(t *testing.T) {
	if os.Getenv("OSAI_FLW_SANDBOX_VERIFY") != "true" {
		t.Skip("explicit real sandbox verification required")
	}
	key, base, dsn := os.Getenv("FLW_SECRET_KEY"), os.Getenv("FLW_BASE_URL"), os.Getenv("OSAI_POSTGRES_DSN")
	u, err := url.Parse(base)
	if os.Getenv("FLW_ENV") != "sandbox" || !strings.HasPrefix(key, "FLWSECK_TEST-") || err != nil || u.Scheme != "https" || u.Host != "api.flutterwave.com" || u.Path != "/v3" || u.User != nil || u.RawQuery != "" {
		t.Fatal("actual provider verification requires the approved v3 sandbox endpoint and test credential")
	}
	dbURL, err := url.Parse(dsn)
	if err != nil || dbURL == nil || (dbURL.Hostname() != "127.0.0.1" && dbURL.Hostname() != "localhost") || !strings.HasPrefix(dbURL.Path, "/osai_proof_") {
		t.Fatal("dedicated local proof database required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("open proof database")
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal("proof database unavailable")
	}
	attempts := PostgresAttempts{DB: db}
	if err := attempts.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	prefix := os.Getenv("OSAI_FLW_SANDBOX_REFERENCE")
	if !validReference(prefix) {
		t.Fatal("explicit stable sandbox reference prefix required")
	}
	for _, scenario := range []struct {
		name, suffix string
		want         provider.TransferStatus
	}{
		{"success", "_success_PMCKDU_1", provider.TransferConfirmed},
		{"failure", "_failure_PMCK_ST_FDU_1", provider.TransferFailed},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ref := prefix + scenario.suffix
			rail := New(Config{BaseURL: base, Env: "sandbox", SecretKey: key, SecretHash: os.Getenv("FLW_SECRET_HASH"), Enabled: true, Attempts: attempts, HTTPClient: &http.Client{Transport: sandboxDiagnosticTransport{t}, Timeout: 15 * time.Second}})
			if err := rail.ProbeTransferHealth(context.Background()); err != nil {
				t.Fatal(err)
			}
			instruction := provider.TransferInstruction{ClientRef: ref, Beneficiary: "0690000040", AmountMinor: 10000, Currency: "NGN", Purpose: "sandbox contract verification", Metadata: map[string]string{"account_bank": "044", "account_number": "0690000040"}}
			ack, err := rail.CreateTransfer(instruction)
			if err != nil {
				t.Fatalf("sandbox create/query failed; retain the stable reference and resolve before retry: %v", err)
			}
			t.Logf("SANDBOX_ACCEPTED reference=%s provider_ref=%s state=%s", ref, ack.ProviderRef, ack.Status)
			deadline := time.Now().Add(3 * time.Minute)
			for time.Now().Before(deadline) {
				result, err := rail.GetTransfer(ref)
				if err == nil && (result.Status == provider.TransferConfirmed || result.Status == provider.TransferFailed) {
					if result.Status != scenario.want || !result.Evidence.IsCompleteFor(ref) || result.AmountMinor != instruction.AmountMinor || result.Currency != "NGN" {
						t.Fatalf("unexpected provider result: state=%s amount=%d currency=%s", result.Status, result.AmountMinor, result.Currency)
					}
					// Reconstruct the adapter and replay the identical create. The durable
					// attempt claim must query the same transfer, never send a second POST.
					restarted := New(rail.config)
					replayed, err := restarted.CreateTransfer(instruction)
					if err != nil || replayed.ProviderRef != result.ProviderRef {
						t.Fatalf("sandbox replay failed: %v", err)
					}
					t.Logf("SANDBOX_FINAL reference=%s provider_ref=%s state=%s payload_hash=%s durable_replay=same_provider_reference", ref, result.ProviderRef, result.Status, result.Evidence.RawPayloadHash)
					return
				}
				if err != nil {
					t.Logf("query unresolved: %v", err)
				}
				time.Sleep(5 * time.Second)
			}
			t.Fatal("sandbox outcome remains unresolved; no terminal result or financial posting claimed")
		})
	}
}

type sandboxDiagnosticTransport struct{ t *testing.T }

func (s sandboxDiagnosticTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		response.Body = io.NopCloser(bytes.NewReader(raw))
		var envelope struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &envelope)
		category := "unclassified_rejection"
		message := strings.ToLower(envelope.Message)
		for _, term := range []string{"whitelist", "insufficient", "balance", "disabled", "approval", "beneficiary", "ip address", "unauthorized"} {
			if strings.Contains(message, term) {
				category = term
				break
			}
		}
		s.t.Logf("SANDBOX_HTTP method=%s path=%s status=%d rejection_category=%s", request.Method, request.URL.Path, response.StatusCode, category)
	}
	return response, nil
}
