package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osai/osai/adapters/firstpair"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	"github.com/osai/osai/services/quote/quotecore"
)

func TestCreateQuoteUsesApprovedSimulatorRoute(t *testing.T) {
	server := &grpcServer{store: quotecore.NewQuoteStore(), liquidity: firstpair.New().Liquidity}
	request := &quotev1.CreateQuoteRequest{InstitutionId: "inst_test", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1", IdempotencyKey: "quote-1"}
	response, err := server.CreateQuote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	providerQuote, ok := server.store.Get(response.QuoteId)
	if !ok || providerQuote.ProviderID != "sim_lp_1" || providerQuote.ProviderEvidence["method"] != "getQuote" || response.AmountOutMinor != providerQuote.AmountOutMinor || response.RateMinor != providerQuote.RateMinor || response.FeeMinor != providerQuote.FeeMinor {
		t.Fatalf("simulator economics or evidence missing: %+v", providerQuote)
	}
	replay, err := server.CreateQuote(context.Background(), request)
	if err != nil || replay.QuoteId != response.QuoteId {
		t.Fatalf("quote replay changed route: %+v %v", replay, err)
	}
	request.DestinationRail = "unknown"
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("unsupported route admitted")
	}
	request.DestinationRail = "flutterwave"
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("default-off Flutterwave customer route admitted")
	}
	request.DestinationRail = "sim_lp_1"
	request.IdempotencyKey = "disabled-simulator"
	t.Setenv("OSAI_SIMULATOR_ROUTING_ENABLED", "false")
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("disabled simulator admitted")
	}
	t.Setenv("OSAI_SIMULATOR_ROUTING_ENABLED", "true")
	t.Setenv("OSAI_SIMULATOR_KILL_SWITCH", "true")
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("simulator kill switch ignored")
	}
}

func TestFlutterwaveQuoteRouteRequiresProviderHealth(t *testing.T) {
	t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "true")
	t.Setenv("FLW_SETTLEMENT_ENABLED", "true")
	t.Setenv("FLW_KILL_SWITCH", "false")
	t.Setenv("FLW_ENV", "test")
	t.Setenv("FLW_SECRET_KEY", "local-only-test-key")
	t.Setenv("FLW_SECRET_HASH", "local-only-test-hash")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transfers" || r.Method != http.MethodGet {
			t.Errorf("unexpected health probe %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	}))
	defer fake.Close()
	t.Setenv("FLW_BASE_URL", fake.URL)
	server := &grpcServer{store: quotecore.NewQuoteStore(), liquidity: firstpair.New().Liquidity}
	request := &quotev1.CreateQuoteRequest{InstitutionId: "inst_test", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", DestinationRail: "flutterwave", IdempotencyKey: "flw-quote-1"}
	if _, err := server.CreateQuote(context.Background(), request); err != nil {
		t.Fatalf("healthy fake provider excluded: %v", err)
	}
	t.Setenv("FLW_KILL_SWITCH", "true")
	request.IdempotencyKey = "flw-killed"
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("Flutterwave kill switch ignored")
	}
	t.Setenv("FLW_KILL_SWITCH", "false")
	fake.Close()
	request.IdempotencyKey = "flw-quote-2"
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("unhealthy provider admitted")
	}
}
