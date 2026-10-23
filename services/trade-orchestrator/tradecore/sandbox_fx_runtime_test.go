package tradecore

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
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/fxevidence"
)

func signedFXFixture(t *testing.T, key string, alter func(*SandboxFXPayload), posts *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		defer r.Body.Close()
		if !hmac.Equal([]byte(r.Header.Get("X-Osai-Sandbox-FX-Signature")), []byte(sandboxFXMAC([]byte(key), raw))) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request SandboxFXRequest
		if json.Unmarshal(raw, &request) != nil {
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/submit" && request.Operation == "submit" {
			mu.Lock()
			*posts++
			mu.Unlock()
			w.WriteHeader(503)
			return
		}
		if r.URL.Path != "/query" || request.Operation != "query" {
			w.WriteHeader(400)
			return
		}
		e := request.Execution
		execution := &fxevidence.FXExecutionEvidence{TradeID: e.TradeID, InstitutionID: e.InstitutionID, QuoteID: e.QuoteID, Executor: e.ExecutorID, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: e.SourcePrincipalMinor, DestinationCurrency: "USD", DestinationExponent: 2, DestinationProceedsMinor: 95, AppliedRate: "0.01", RateDirection: fxevidence.DestinationPerSource, ExecutionReference: "sbx:execution", SourceEvidenceReference: "sbx:source", DestinationEvidenceRef: "sbx:destination", OccurredAt: time.Now().UTC()}
		payload := SandboxFXPayload{Status: FXQueryExecuted, ExecutorID: e.ExecutorID, RequestReference: e.RequestReference, RequestHash: e.RequestHash, PricingSnapshotHash: e.PricingSnapshotHash, RecoveryGeneration: e.RecoveryGeneration, ProviderRef: execution.ExecutionReference, QueryReference: request.Nonce, ObservedAt: time.Now().UTC(), Execution: execution}
		if alter != nil {
			alter(&payload)
		}
		body, _ := json.Marshal(payload)
		_ = json.NewEncoder(w).Encode(SandboxFXEnvelope{Payload: body, Signature: sandboxFXMAC([]byte(key), body)})
	}))
}

func TestSandboxFXResponseAuthenticationAndEconomicsBinding(t *testing.T) {
	key := "local-" + strings.Repeat("x", 40)
	var mu sync.Mutex
	posts := 0
	execution := FXExecution{TradeID: "sbx:trade", InstitutionID: "sbx:institution", QuoteID: "sbx:quote", ExecutorID: "sbx:executor", RequestReference: "sbx:request", RequestHash: "hash", PricingSnapshotHash: "pricing", RecoveryGeneration: 1, SourceCurrency: "NGN", SourceExponent: 2, SourcePrincipalMinor: 9500, DestinationCurrency: "USD", DestinationExponent: 2, QuotedDestinationMinor: 95, Status: FXExecutionSubmitted}
	server := signedFXFixture(t, key, nil, &posts, &mu)
	defer server.Close()
	client, err := NewSandboxFXClient(server.URL, execution.ExecutorID, key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.QueryFX(context.Background(), execution)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.VerifyFXQuery(context.Background(), execution, result); err != nil {
		t.Fatal(err)
	}
	var envelope SandboxFXEnvelope
	if err := json.Unmarshal(result.QueryEvidence.RawPayload, &envelope); err != nil {
		t.Fatal(err)
	}
	t.Run("signed payload has a second JSON value", func(t *testing.T) {
		ambiguous := envelope
		ambiguous.Payload = append(append([]byte(nil), envelope.Payload...), []byte(` {"status":"NOT_EXECUTED"}`)...)
		ambiguous.Signature = sandboxFXMAC([]byte(key), ambiguous.Payload)
		// Construct the raw envelope directly: json.Marshal correctly refuses a
		// RawMessage containing multiple JSON values.
		raw := []byte(`{"payload":` + string(ambiguous.Payload) + `,"signature":"` + ambiguous.Signature + `"}`)
		if _, err := client.decode(execution, raw); err == nil {
			t.Fatal("ambiguous signed response accepted")
		}
		var payload SandboxFXPayload
		if err := decodeSandboxFXJSON(ambiguous.Payload, &payload); err == nil {
			t.Fatal("second signed payload value accepted")
		}
	})
	t.Run("undeclared envelope field", func(t *testing.T) {
		raw, err := json.Marshal(map[string]any{"payload": envelope.Payload, "signature": envelope.Signature, "override_status": "NOT_EXECUTED"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.decode(execution, raw); err == nil {
			t.Fatal("undeclared response field accepted")
		}
	})
	envelope.Signature = strings.Repeat("0", 64)
	tampered := result
	tampered.QueryEvidence.RawPayload, _ = json.Marshal(envelope)
	digest := sha256.Sum256(tampered.QueryEvidence.RawPayload)
	tampered.QueryEvidence.PayloadHash = hex.EncodeToString(digest[:])
	if client.VerifyFXQuery(context.Background(), execution, tampered) == nil {
		t.Fatal("unsigned response accepted even after its payload hash was recomputed")
	}
	changed := result
	changed.Status = FXQueryNotExecuted
	if client.VerifyFXQuery(context.Background(), execution, changed) == nil {
		t.Fatal("status not bound to signed bytes")
	}
	result.Execution.DestinationProceedsMinor++
	if client.VerifyFXQuery(context.Background(), execution, result) == nil {
		t.Fatal("economics not bound to signed bytes")
	}
	wrongKey, _ := NewSandboxFXClient(server.URL, execution.ExecutorID, "local-"+strings.Repeat("y", 40))
	if _, err = wrongKey.QueryFX(context.Background(), execution); err == nil {
		t.Fatal("unauthenticated request accepted")
	}
	for _, change := range []struct {
		name  string
		alter func(*SandboxFXPayload)
	}{
		{"generation", func(p *SandboxFXPayload) { p.RecoveryGeneration++ }},
		{"request hash", func(p *SandboxFXPayload) { p.RequestHash = "different" }},
		{"pricing hash", func(p *SandboxFXPayload) { p.PricingSnapshotHash = "different" }},
		{"nonce", func(p *SandboxFXPayload) { p.QueryReference = "old-query" }},
		{"old response", func(p *SandboxFXPayload) { p.ObservedAt = time.Now().Add(-time.Hour) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			fake := signedFXFixture(t, key, change.alter, &posts, &mu)
			defer fake.Close()
			rail, _ := NewSandboxFXClient(fake.URL, execution.ExecutorID, key)
			if _, err := rail.QueryFX(context.Background(), execution); err == nil {
				t.Fatal("unbound signed observation accepted")
			}
		})
	}
	for _, endpoint := range []string{"https://api.flutterwave.com", "http://remote.example", "http://127.0.0.1?redirect=remote", "http://user:secret@127.0.0.1", "http://127.0.0.1/../external"} {
		if _, err := NewSandboxFXClient(endpoint, execution.ExecutorID, key); err == nil {
			t.Fatal("non-fixture endpoint accepted")
		}
	}
	forwarded := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); forwarded++; mu.Unlock(); w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	redirectClient, _ := NewSandboxFXClient(redirect.URL, execution.ExecutorID, key)
	if _, err := redirectClient.QueryFX(context.Background(), execution); err == nil {
		t.Fatal("redirect accepted")
	}
	mu.Lock()
	count := forwarded
	mu.Unlock()
	if count != 0 {
		t.Fatal("signed fixture request forwarded to redirect target")
	}
}

func TestSandboxFXRecoveryQueriesAfterSubmissionTimeoutAndRestart(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof database required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateSandboxFXRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := crossCurrencyFXSnapshot(t)
	trade, err := store.CreateTradeWithPricing("sbx:"+uuid.NewString(), "sbx:"+uuid.NewString(), "accept", "sbx:correlation", snapshot.SourcePrincipalMinor, snapshot.SourceCurrency, snapshot.DestinationCurrency, snapshot, "sbx:beneficiary", "sbx:fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		db.Exec(`DELETE FROM trade_fx_executions WHERE trade_id=$1`, trade.ID)
		db.Exec(`DELETE FROM trade_outbox WHERE trade_id=$1`, trade.ID)
		db.Exec(`DELETE FROM trade_transitions WHERE trade_id=$1`, trade.ID)
		db.Exec(`DELETE FROM trades WHERE id=$1`, trade.ID)
	}()
	if err = store.SetStatus(trade.ID, StateFunded); err != nil {
		t.Fatal(err)
	}
	id := "sbx:" + uuid.NewString()
	_, err = store.CreateFXExecution(trade.ID, "fx-intent", id)
	if err != nil {
		t.Fatal(err)
	}
	key := "local-" + strings.Repeat("z", 40)
	var mu sync.Mutex
	posts := 0
	fake := signedFXFixture(t, key, nil, &posts, &mu)
	defer fake.Close()
	rail, _ := NewSandboxFXClient(fake.URL, id, key)
	unknown, err := store.SubmitFXExecution(context.Background(), trade.ID, rail, rail)
	if err == nil || unknown == nil || unknown.Status != FXExecutionUnknown {
		t.Fatal("submission ambiguity did not retain UNKNOWN")
	}
	restarted, err := NewPostgresStore(db)
	if err != nil {
		t.Fatal(err)
	}
	count, err := restarted.RecoverSandboxFXBatch(context.Background(), rail, 10)
	if err != nil || count != 1 {
		t.Fatalf("query recovery failed count=%d err=%v", count, err)
	}
	final, found, err := restarted.GetFXExecution(trade.ID)
	if err != nil || !found || final.Status != FXExecutionComplete || final.SubmissionCount != 1 {
		t.Fatal("restart recovery did not preserve one submission")
	}
	if count, err = restarted.RecoverSandboxFXBatch(context.Background(), rail, 10); err != nil || count != 0 {
		t.Fatal("terminal execution returned to recovery")
	}
	mu.Lock()
	actualPosts := posts
	mu.Unlock()
	if actualPosts != 1 {
		t.Fatalf("create was retried %d times", actualPosts)
	}
	var events int
	if err = db.QueryRow(`SELECT count(*) FROM trade_outbox WHERE trade_id=$1 AND event_type='trade.fx_executed'`, trade.ID).Scan(&events); err != nil || events != 1 {
		t.Fatal("FX finality event was not unique")
	}
}
