package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/pricing"
	"github.com/osai/osai/pkg/sandboxpolicy"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
)

func TestSandboxFXRuntimeRequiresExplicitProfile(t *testing.T) {
	for _, key := range []string{"OSAI_SANDBOX_FX_BASE_URL", "OSAI_SANDBOX_FX_SIGNING_KEY", "OSAI_SANDBOX_FX_EXECUTOR_ID", "OSAI_SANDBOX_ACCOUNTING_POLICY"} {
		t.Setenv(key, "")
	}
	if err := startSandboxFXRecovery(context.Background(), nil); err != nil {
		t.Fatal("disabled runtime altered ordinary startup")
	}
	t.Setenv("OSAI_SANDBOX_FX_BASE_URL", "http://127.0.0.1:1234")
	if err := startSandboxFXRecovery(context.Background(), nil); err == nil {
		t.Fatal("FX worker started without explicit proof profile")
	}
}

func TestSandboxFXBackgroundRuntimeQueriesWithoutResubmitting(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof database required")
	}
	if !sandboxpolicy.LocalProofDSN(dsn) {
		t.Fatal("dedicated loopback proof database required")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "fx_worker_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); admin.Close() })
	store, err := tradecore.NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pricedFeeSnapshot(t, time.Now().Add(time.Hour))
	snapshot.DestinationCurrency = "USD"
	snapshot.DestinationAmountMinor = 95
	snapshot.RateScaled = 1000000
	snapshot.RateDirection = "USD_PER_NGN"
	snapshot.Hash, err = pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	trade, err := store.CreateTradeWithPricing("sbx:institution", "sbx:quote", "accept", "sbx:correlation", snapshot.SourcePrincipalMinor, "NGN", "USD", snapshot, "sbx:beneficiary", "sbx:fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetStatus(trade.ID, tradecore.StateFunded); err != nil {
		t.Fatal(err)
	}
	id := "sbx:background-fixture"
	if _, err = store.CreateFXExecution(trade.ID, "sbx:intent", id); err != nil {
		t.Fatal(err)
	}
	if _, err = store.MarkFXExecutionSubmitted(trade.ID); err != nil {
		t.Fatal(err)
	}
	key := "local-" + strings.Repeat("x", 40)
	var mu sync.Mutex
	queries, submissions := 0, 0
	mac := func(body []byte) string {
		h := hmac.New(sha256.New, []byte(key))
		h.Write(body)
		return hex.EncodeToString(h.Sum(nil))
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		defer r.Body.Close()
		if !hmac.Equal([]byte(r.Header.Get("X-Osai-Sandbox-FX-Signature")), []byte(mac(raw))) {
			w.WriteHeader(401)
			return
		}
		var req tradecore.SandboxFXRequest
		if json.Unmarshal(raw, &req) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		if req.Operation == "query" {
			queries++
		} else {
			submissions++
		}
		mu.Unlock()
		e := req.Execution
		payload := tradecore.SandboxFXPayload{Status: tradecore.FXQueryNotExecuted, ExecutorID: e.ExecutorID, RequestReference: e.RequestReference, RequestHash: e.RequestHash, PricingSnapshotHash: e.PricingSnapshotHash, RecoveryGeneration: e.RecoveryGeneration, QueryReference: req.Nonce, ObservedAt: time.Now().UTC()}
		body, _ := json.Marshal(payload)
		json.NewEncoder(w).Encode(tradecore.SandboxFXEnvelope{Payload: body, Signature: mac(body)})
	}))
	defer fake.Close()
	settings := map[string]string{"OSAI_POSTGRES_DSN": u.String(), "OSAI_TRADE_POSTGRES_DSN": "", "OSAI_SETTLEMENT_POSTGRES_DSN": "", "OSAI_LEDGER_POSTGRES_DSN": "", "OSAI_SANDBOX_ACCOUNTING_POLICY": sandboxpolicy.Version, "OSAI_LOCAL_PROOF_MODE": "true", "OSAI_API_ENVIRONMENT": "sandbox", "FLW_ENV": "test", "FLW_BASE_URL": fake.URL + "/v3", "FLW_SECRET_KEY": "local-fixture", "OSAI_SANDBOX_LEDGER_TOKEN": strings.Repeat("t", 32), "OSAI_SANDBOX_FX_BASE_URL": fake.URL, "OSAI_SANDBOX_FX_SIGNING_KEY": key, "OSAI_SANDBOX_FX_EXECUTOR_ID": id}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = startSandboxFXRecovery(ctx, store); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	resolved := false
	for time.Now().Before(deadline) {
		e, found, err := store.GetFXExecution(trade.ID)
		if err != nil {
			t.Fatal(err)
		}
		if found && e.Status == tradecore.FXExecutionReady {
			resolved = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if !resolved {
		t.Fatal("configured background recovery did not query persisted instruction")
	}
	mu.Lock()
	defer mu.Unlock()
	if queries != 1 || submissions != 0 {
		t.Fatalf("background recovery queried=%d submitted=%d", queries, submissions)
	}
}
