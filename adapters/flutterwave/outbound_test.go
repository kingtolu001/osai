package flutterwave

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osai/osai/pkg/provider"
)

type memoryAttempts struct {
	mu     sync.Mutex
	hashes map[string]string
	refs   map[string]string
}

func (m *memoryAttempts) Claim(_ context.Context, ref, hash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hashes == nil {
		m.hashes = map[string]string{}
	}
	if prior, ok := m.hashes[ref]; ok {
		if prior != hash {
			return false, fmt.Errorf("reference conflict")
		}
		return false, nil
	}
	m.hashes[ref] = hash
	return true, nil
}
func (m *memoryAttempts) RecordProviderRef(_ context.Context, ref, providerRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refs == nil {
		m.refs = map[string]string{}
	}
	m.refs[ref] = providerRef
	return nil
}

func instruction() provider.TransferInstruction {
	return provider.TransferInstruction{ClientRef: "si_stable_1", AmountMinor: 10025, Currency: "NGN", Metadata: map[string]string{"account_bank": "044", "account_number": "0690000040"}}
}

func TestCreateIsClaimedBeforeProviderAndNeverPostedAgain(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	status := "NEW"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer local-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			posts++
			var body struct {
				Reference string `json:"reference"`
				Amount    int64  `json:"amount"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Reference != "si_stable_1" || body.Amount != 100 {
				t.Errorf("bad transfer request: %+v", body)
			}
		} else if r.URL.Path != "/v3/transfers" && r.URL.Path != "/v3/transfers/42" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		data := fmt.Sprintf(`{"id":42,"reference":"si_stable_1","amount":100,"currency":"NGN","status":%q,"created_at":"2026-01-01T00:00:00Z"}`, status)
		if r.URL.Path == "/v3/transfers" && r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"status":"success","data":[%s]}`, data)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
	}))
	defer server.Close()
	// Flutterwave's v3 NGN transfer endpoint requires whole major units.
	in := instruction()
	in.AmountMinor = 10000
	a := New(Config{BaseURL: server.URL + "/v3", SecretKey: "local-test-key", Env: "test", Enabled: true, Attempts: &memoryAttempts{}})
	ack, err := a.CreateTransfer(in)
	if err != nil || ack.ProviderRef != "42" || ack.Status != provider.TransferAccepted {
		t.Fatalf("create: %+v %v", ack, err)
	}
	mu.Lock()
	status = "SUCCESSFUL"
	mu.Unlock()
	again, err := a.CreateTransfer(in)
	if err != nil || again.ProviderRef != "42" || again.Status != provider.TransferConfirmed {
		t.Fatalf("replay query: %+v %v", again, err)
	}
	mu.Lock()
	count := posts
	mu.Unlock()
	if count != 1 {
		t.Fatalf("created %d transfers", count)
	}
}

func TestDisabledRailStillQueriesSubmittedTransfer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("disabled rail attempted money movement")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		data := `{"id":42,"reference":"si_stable_1","amount":100,"currency":"NGN","status":"SUCCESSFUL"}`
		if r.URL.Path == "/v3/transfers" {
			fmt.Fprintf(w, `{"status":"success","data":[%s]}`, data)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
	}))
	defer server.Close()
	a := New(Config{BaseURL: server.URL + "/v3", SecretKey: "local-test-key", Env: "test", Enabled: false, Attempts: &memoryAttempts{}})
	in := instruction()
	in.AmountMinor = 10000
	if _, err := a.CreateTransfer(in); err == nil {
		t.Fatal("disabled rail accepted a new transfer")
	}
	result, err := a.GetTransfer(in.ClientRef)
	if err != nil || result.Status != provider.TransferConfirmed {
		t.Fatalf("submitted transfer could not be recovered: %+v %v", result, err)
	}
}

func TestTimeoutAfterProviderAcceptedQueriesFirst(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodPost {
			posts++
			mu.Unlock()
			time.Sleep(80 * time.Millisecond)
			return
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		data := `{"id":42,"reference":"si_stable_1","amount":100,"currency":"NGN","status":"SUCCESSFUL","created_at":"2026-01-01T00:00:00Z"}`
		if r.URL.Path == "/v3/transfers" {
			fmt.Fprintf(w, `{"status":"success","data":[%s]}`, data)
		} else {
			fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
		}
	}))
	defer server.Close()
	in := instruction()
	in.AmountMinor = 10000
	a := New(Config{BaseURL: server.URL + "/v3", SecretKey: "local-test-key", Env: "test", Enabled: true, Attempts: &memoryAttempts{}, HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}})
	_, err := a.CreateTransfer(in)
	if err == nil {
		t.Fatal("timeout must be ambiguous")
	}
	providerErr, ok := err.(*provider.Error)
	if !ok || providerErr.Class != provider.ErrorProviderTimeout {
		t.Fatalf("unexpected error %v", err)
	}
	result, err := a.GetTransfer(in.ClientRef)
	if err != nil || result.Status != provider.TransferConfirmed {
		t.Fatalf("query first: %+v %v", result, err)
	}
	_, err = a.CreateTransfer(in)
	if err != nil {
		t.Fatalf("replay query: %v", err)
	}
	mu.Lock()
	count := posts
	mu.Unlock()
	if count != 1 {
		t.Fatalf("timeout caused %d creates", count)
	}
}

func TestProviderStatusAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		code         int
		want         provider.TransferStatus
		class        provider.ErrorClass
	}{
		{"processing", "PENDING", 200, provider.TransferProcessing, ""},
		{"failed", "FAILED", 200, provider.TransferFailed, ""},
		{"auth", "", 401, "", provider.ErrorAuthFailure},
		{"rate", "", 429, "", provider.ErrorRateLimited},
		{"server", "", 503, "", provider.ErrorProviderUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.code != 200 {
					w.WriteHeader(tc.code)
					return
				}
				data := fmt.Sprintf(`{"id":42,"reference":"si_stable_1","amount":100,"currency":"NGN","status":%q,"created_at":"2026-01-01T00:00:00Z"}`, tc.status)
				if strings.HasSuffix(r.URL.Path, "/transfers") {
					fmt.Fprintf(w, `{"status":"success","data":[%s]}`, data)
				} else {
					fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
				}
			}))
			defer server.Close()
			a := New(Config{BaseURL: server.URL + "/v3", SecretKey: "local-test-key", Env: "test", Enabled: true})
			result, err := a.GetTransfer("si_stable_1")
			if tc.class != "" {
				pe, ok := err.(*provider.Error)
				if !ok || pe.Class != tc.class {
					t.Fatalf("error %v", err)
				}
				if (tc.class == provider.ErrorAuthFailure || tc.code >= 500) && a.Health().Capabilities["transfer"] {
					t.Fatal("provider health did not degrade")
				}
				return
			}
			if err != nil || result.Status != tc.want {
				t.Fatalf("status: %+v %v", result, err)
			}
		})
	}
}

func TestListTransactionsPreservesDistinctEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
			t.Error("statement window missing")
		}
		_, _ = w.Write([]byte(`{"status":"success","data":[{"id":42,"reference":"si_1","amount":100,"fee":0,"currency":"NGN","account_number":"0690000040","status":"SUCCESSFUL","created_at":"2026-09-25T10:00:00Z"},{"id":43,"reference":"si_2","amount":200,"fee":1.5,"currency":"NGN","account_number":"0690000041","status":"FAILED","created_at":"2026-09-25T11:00:00Z"}]}`))
	}))
	defer server.Close()
	a := New(Config{BaseURL: server.URL + "/v3", SecretKey: "local-test-key", Env: "test", Enabled: true, Attempts: &memoryAttempts{}})
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	items, err := a.ListTransactions(TransactionWindow{From: from, To: from.Add(24 * time.Hour), PageSize: 10})
	if err != nil || len(items) != 2 {
		t.Fatalf("statement: %+v %v", items, err)
	}
	if items[0].ClientRef != "si_1" || items[0].Beneficiary != "0690000040" || items[0].FeeMinor != 0 || items[1].FeeMinor != 150 || items[0].Evidence.RawPayloadHash == items[1].Evidence.RawPayloadHash {
		t.Fatalf("statement normalization lost distinct evidence: %+v", items)
	}
}
