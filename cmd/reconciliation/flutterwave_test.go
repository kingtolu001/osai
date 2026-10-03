package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osai/osai/adapters/flutterwave"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	reconciliation "github.com/osai/osai/services/reconciliation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type fakeConfirmedLister struct{ t *testing.T }

func (f fakeConfirmedLister) ListConfirmedSettlements(ctx context.Context, req *settlementv1.ListConfirmedSettlementsRequest, _ ...grpc.CallOption) (*settlementv1.ListConfirmedSettlementsResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	if req.ProviderId != "flutterwave" || len(md.Get("x-osai-reconciliation-token")) != 1 || md.Get("x-osai-reconciliation-token")[0] != "local-test-token" {
		f.t.Fatal("reconciliation snapshot was not scoped and authenticated")
	}
	return &settlementv1.ListConfirmedSettlementsResponse{Settlements: []*settlementv1.SettlementResponse{{SettlementId: "set_auto_1", ProviderId: "flutterwave", Status: "CONFIRMED", ClientRef: "si_auto_1", ProviderRef: "42", AmountMinor: 10000, Currency: "NGN", Beneficiary: "0690000040"}}}, nil
}

func TestAutomaticFlutterwaveIngestionAndMismatchReplay(t *testing.T) {
	var amount atomic.Int64
	amount.Store(100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("reference") != "si_auto_1" {
			t.Errorf("unexpected provider request: %s", r.URL.String())
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":[{"id":42,"reference":"si_auto_1","amount":%d,"currency":"NGN","status":"SUCCESSFUL","account_number":"0690000040","created_at":"2026-09-27T00:00:00Z"}]}`, amount.Load())
	}))
	defer server.Close()
	rail := flutterwave.New(flutterwave.Config{BaseURL: server.URL + "/v3", SecretKey: "local-test-key", Env: "test", Enabled: false})
	store := reconciliation.NewMemoryStore()
	svc := reconciliation.NewServiceWithStore(store)
	now := time.Date(2026, 9, 27, 2, 0, 0, 0, time.UTC)
	run := func() {
		t.Helper()
		if err := reconcileFlutterwavePass(context.Background(), svc, fakeConfirmedLister{t}, rail, "local-test-token", now, 0); err != nil {
			t.Fatal(err)
		}
	}
	run()
	breaks, err := store.LoadBreaks()
	if err != nil || len(breaks) != 0 {
		t.Fatalf("matching provider evidence created a break: %+v %v", breaks, err)
	}
	amount.Store(90)
	run()
	svc = reconciliation.NewServiceWithStore(store)
	run()
	breaks, err = store.LoadBreaks()
	if err != nil || len(breaks) != 1 || breaks[0].Type != reconciliation.BreakTypeAmountMismatch {
		t.Fatalf("mismatch replay should create one break: %+v %v", breaks, err)
	}
	evidence, err := store.LoadEvidence()
	if err != nil || len(evidence) != 2 {
		t.Fatalf("changed provider observation should retain both evidence versions: %d %v", len(evidence), err)
	}
}
