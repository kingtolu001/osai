package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osai/osai/adapters/firstpair"
	"github.com/osai/osai/pkg/pricing"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type staticFeeScheduleStore struct {
	policy quotecore.FeeSchedule
	err    error
}

func (s staticFeeScheduleStore) GetFeeSchedule(_ context.Context, institutionID, sourceCurrency, destinationCurrency string) (quotecore.FeeSchedule, error) {
	if s.err != nil {
		return quotecore.FeeSchedule{}, s.err
	}
	if s.policy.InstitutionID != institutionID || s.policy.SourceCurrency != sourceCurrency || s.policy.DestinationCurrency != destinationCurrency {
		return quotecore.FeeSchedule{}, quotecore.ErrPricingPolicyUnavailable
	}
	return s.policy, nil
}

func quoteTestPolicy(institutionID string) quotecore.FeeSchedule {
	return quotecore.FeeSchedule{
		ID:                          "fee_test",
		Version:                     1,
		InstitutionID:               institutionID,
		SourceCurrency:              "USD",
		DestinationCurrency:         "NGN",
		SourceExponent:              2,
		DestinationExponent:         2,
		MinimumSourcePrincipalMinor: 100,
		MaximumSourcePrincipalMinor: 1_000_000,
		MinimumTotalDebitMinor:      100,
		MaximumTotalDebitMinor:      2_000_000,
		MinimumDestinationMinor:     100,
		MaximumDestinationMinor:     2_000_000_000,
		FixedFeeMinor:               500,
	}
}

func TestCreateQuoteUsesApprovedSimulatorRoute(t *testing.T) {
	server := &grpcServer{store: quotecore.NewQuoteStore(), pricing: staticFeeScheduleStore{policy: quoteTestPolicy("inst_test")}, liquidity: firstpair.New().Liquidity}
	request := &quotev1.CreateQuoteRequest{InstitutionId: "inst_test", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1", IdempotencyKey: "quote-1"}
	response, err := server.CreateQuote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	providerQuote, ok := server.store.Get(response.QuoteId)
	if !ok || providerQuote.ProviderID != "sim_lp_1" || len(providerQuote.RouteCandidates) != 1 || providerQuote.RouteCandidates[0].SettlementProviderID != "sim_lp_1" || providerQuote.ProviderEvidence["method"] != "getQuote" || response.AmountOutMinor != providerQuote.AmountOutMinor || response.RateMinor != providerQuote.RateMinor || response.FeeMinor != 500 || providerQuote.Pricing == nil || providerQuote.Pricing.SourcePrincipalMinor != 9500 || providerQuote.Pricing.TotalDebitMinor != 10000 || providerQuote.Pricing.FeeSchedule.ID != "fee_test" {
		t.Fatalf("simulator economics or evidence missing: %+v", providerQuote)
	}
	if response.TotalCostMinor != response.TotalDebitMinor {
		t.Fatalf("total_cost_minor must equal all-in source debit: %+v", response)
	}
	replay, err := server.CreateQuote(context.Background(), request)
	if err != nil || replay.QuoteId != response.QuoteId {
		t.Fatalf("quote replay changed route: %+v %v", replay, err)
	}
	server.pricing = staticFeeScheduleStore{err: errors.New("pricing store unavailable")}
	server.liquidity = nil
	replay, err = server.CreateQuote(context.Background(), request)
	if err != nil || replay.QuoteId != response.QuoteId || replay.FeeScheduleId != response.FeeScheduleId {
		t.Fatalf("quote replay depended on current pricing/provider availability: %+v %v", replay, err)
	}
	conflictingRequest := proto.Clone(request).(*quotev1.CreateQuoteRequest)
	conflictingRequest.BaseAmountMinor++
	if _, err := server.CreateQuote(context.Background(), conflictingRequest); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("same quote idempotency key accepted changed request: %v", err)
	}
	server.pricing = staticFeeScheduleStore{policy: quoteTestPolicy("inst_test")}
	server.liquidity = firstpair.New().Liquidity
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

func TestProviderStatusReturnsNormalizedCustomerRoutes(t *testing.T) {
	t.Setenv("OSAI_SIMULATOR_ROUTING_ENABLED", "true")
	t.Setenv("OSAI_SIMULATOR_KILL_SWITCH", "false")
	t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "false")
	t.Setenv("FLW_SETTLEMENT_ENABLED", "false")
	t.Setenv("FLW_KILL_SWITCH", "false")
	server := &grpcServer{liquidity: firstpair.New().Liquidity}
	response, err := server.ListProviderStatuses(context.Background(), &quotev1.ListProviderStatusesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 ||
		response.Items[0].RouteId != "simulator" || response.Items[0].Status != "AVAILABLE" ||
		response.Items[1].RouteId != "ngn-bank-transfer" || response.Items[1].Status != "UNAVAILABLE" {
		t.Fatalf("unexpected customer route status: %+v", response.Items)
	}
	for _, item := range response.Items {
		if item.RouteId == "sim_lp_1" || item.RouteId == "flutterwave" {
			t.Fatalf("provider identity exposed instead of route ID: %+v", item)
		}
	}
	t.Setenv("OSAI_SIMULATOR_KILL_SWITCH", "true")
	response, err = server.ListProviderStatuses(context.Background(), &quotev1.ListProviderStatusesRequest{})
	if err != nil || response.Items[0].Status != "UNAVAILABLE" {
		t.Fatalf("simulator kill switch did not affect route status: %+v %v", response, err)
	}
}

func TestCreateQuoteFailsClosedWithoutInstitutionPricing(t *testing.T) {
	server := &grpcServer{store: quotecore.NewQuoteStore(), liquidity: firstpair.New().Liquidity}
	request := &quotev1.CreateQuoteRequest{InstitutionId: "inst_test", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1", IdempotencyKey: "quote-no-policy"}
	if _, err := server.CreateQuote(context.Background(), request); err == nil {
		t.Fatal("quote created without institution pricing policy")
	}
}

func TestCreateFixedDestinationQuoteRequiresExactFirmProceeds(t *testing.T) {
	policy := quoteTestPolicy("inst_test")
	server := &grpcServer{store: quotecore.NewQuoteStore(), pricing: staticFeeScheduleStore{policy: policy}, liquidity: firstpair.New().Liquidity}
	request := &quotev1.CreateQuoteRequest{
		InstitutionId: "inst_test", QuoteMode: string(quotecore.QuoteModeFixedDestination),
		DestinationAmountMinor: 15_000_000, BaseCurrency: "USD", QuoteCurrency: "NGN",
		DestinationRail: "sim_lp_1", IdempotencyKey: "fixed-destination-exact",
	}
	response, err := server.CreateQuote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.SourcePrincipalMinor != 10_000 || response.TotalDebitMinor != 10_500 ||
		response.DestinationTargetMinor != 15_000_000 || response.DestinationSurplusMinor != 0 ||
		response.RateDirection != "NGN_PER_USD" || response.RateScale != quotecore.RateScale {
		t.Fatalf("unexpected fixed-destination quote snapshot: %+v", response)
	}
	request.DestinationAmountMinor++
	request.IdempotencyKey = "fixed-destination-surplus"
	surplusQuote, err := server.CreateQuote(context.Background(), request)
	if err != nil {
		t.Fatalf("fixed-destination quote with separately exposed surplus failed: %v", err)
	}
	if surplusQuote.AmountOutMinor != request.DestinationAmountMinor ||
		surplusQuote.DestinationSurplusMinor <= 0 ||
		surplusQuote.BaseAmountMinor != surplusQuote.SourcePrincipalMinor ||
		surplusQuote.TotalDebitMinor != surplusQuote.SourcePrincipalMinor+surplusQuote.FeeMinor {
		t.Fatalf("fixed-destination amount/fee semantics were not preserved: %+v", surplusQuote)
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
	server := &grpcServer{store: quotecore.NewQuoteStore(), pricing: staticFeeScheduleStore{policy: quoteTestPolicy("inst_test")}, liquidity: firstpair.New().Liquidity}
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

func TestCreateQuoteDoesNotExposePricingStoreFailure(t *testing.T) {
	server := &grpcServer{
		store: quotecore.NewQuoteStore(), pricing: staticFeeScheduleStore{err: errors.New("database credentials leaked")},
		liquidity: firstpair.New().Liquidity,
	}
	request := &quotev1.CreateQuoteRequest{InstitutionId: "inst_test", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1", IdempotencyKey: "quote-db-failure"}
	_, err := server.CreateQuote(context.Background(), request)
	if err == nil || err.Error() == "database credentials leaked" {
		t.Fatalf("pricing store failure was not safely mapped: %v", err)
	}
}

func TestAcceptQuoteBlocksUntilTradeLedgerSupportsPricedFX(t *testing.T) {
	server := &grpcServer{store: quotecore.NewQuoteStore(), pricing: staticFeeScheduleStore{policy: quoteTestPolicy("inst_test")}, liquidity: firstpair.New().Liquidity}
	created, err := server.CreateQuote(context.Background(), &quotev1.CreateQuoteRequest{
		InstitutionId: "inst_test", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN",
		DestinationRail: "sim_lp_1", IdempotencyKey: "accept-priced-fx",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = server.AcceptQuote(context.Background(), &quotev1.AcceptQuoteRequest{
		InstitutionId: "inst_test", QuoteId: created.QuoteId, IdempotencyKey: "accept-priced-fx-trade",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("priced FX accepted without multi-currency Ledger support: %v", err)
	}
}

func TestAcceptedTradeRequestCarriesVerifiedPricingSnapshot(t *testing.T) {
	policy := quoteTestPolicy("inst_same_currency")
	policy.SourceCurrency = "NGN"
	policy.DestinationCurrency = "NGN"
	policy.FixedFeeMinor = 500
	policy.MinimumFeeMinor = 500
	policy.MaximumFeeMinor = 500
	server := &grpcServer{
		store: quotecore.NewQuoteStore(), pricing: staticFeeScheduleStore{policy: policy},
		liquidity: firstpair.New().Liquidity,
	}
	quote, err := server.CreateQuote(context.Background(), &quotev1.CreateQuoteRequest{
		InstitutionId: "inst_same_currency", BaseAmountMinor: 10_000,
		BaseCurrency: "NGN", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1",
		IdempotencyKey: "fee-bearing-quote",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := server.store.Get(quote.QuoteId)
	if !ok {
		t.Fatal("created quote missing from owner store")
	}
	request, err := tradeRequestFromQuote(&quotev1.AcceptQuoteRequest{
		InstitutionId: "inst_same_currency", QuoteId: quote.QuoteId, IdempotencyKey: "accept-fee",
	}, stored)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pricing.Snapshot{
		Mode:                    request.PricingSnapshot.GetQuoteMode(),
		RequestedAmountMinor:    request.PricingSnapshot.GetRequestedAmountMinor(),
		SourcePrincipalMinor:    request.PricingSnapshot.GetSourcePrincipalMinor(),
		FeeMinor:                request.PricingSnapshot.GetFeeMinor(),
		TotalDebitMinor:         request.PricingSnapshot.GetTotalDebitMinor(),
		DestinationAmountMinor:  request.PricingSnapshot.GetDestinationAmountMinor(),
		DestinationTargetMinor:  request.PricingSnapshot.GetDestinationTargetMinor(),
		DestinationSurplusMinor: request.PricingSnapshot.GetDestinationSurplusMinor(),
		SourceCurrency:          request.PricingSnapshot.GetSourceCurrency(),
		DestinationCurrency:     request.PricingSnapshot.GetDestinationCurrency(),
		RateScaled:              request.PricingSnapshot.GetRateScaled(),
		RateDirection:           request.PricingSnapshot.GetRateDirection(),
		RateScale:               request.PricingSnapshot.GetRateScale(),
		SourceExponent:          request.PricingSnapshot.GetSourceMinorExponent(),
		DestinationExponent:     request.PricingSnapshot.GetDestinationMinorExponent(),
		FeeScheduleID:           request.PricingSnapshot.GetFeeScheduleId(),
		FeeScheduleVersion:      request.PricingSnapshot.GetFeeScheduleVersion(),
		FixedFeeMinor:           request.PricingSnapshot.GetFixedFeeMinor(),
		FeeBasisPoints:          request.PricingSnapshot.GetFeeBasisPoints(),
		MinimumFeeMinor:         request.PricingSnapshot.GetMinimumFeeMinor(),
		MaximumFeeMinor:         request.PricingSnapshot.GetMaximumFeeMinor(),
		MinimumPrincipalMinor:   request.PricingSnapshot.GetMinimumSourcePrincipalMinor(),
		MaximumPrincipalMinor:   request.PricingSnapshot.GetMaximumSourcePrincipalMinor(),
		MinimumDebitMinor:       request.PricingSnapshot.GetMinimumTotalDebitMinor(),
		MaximumDebitMinor:       request.PricingSnapshot.GetMaximumTotalDebitMinor(),
		MinimumDestinationMinor: request.PricingSnapshot.GetMinimumDestinationMinor(),
		MaximumDestinationMinor: request.PricingSnapshot.GetMaximumDestinationMinor(),
		ExpiresAt:               request.PricingSnapshot.GetExpiresAt(),
		Hash:                    request.PricingSnapshot.GetSnapshotHash(),
	}
	if err := pricing.Validate(snapshot); err != nil {
		t.Fatalf("trade request does not carry valid immutable economics: %v; %+v", err, request.PricingSnapshot)
	}
	if request.BaseAmountMinor != snapshot.SourcePrincipalMinor ||
		request.Amount.AmountMinor != snapshot.DestinationAmountMinor ||
		snapshot.TotalDebitMinor != 10_000 || snapshot.FeeMinor != 500 {
		t.Fatalf("trade request amount fields diverge from the immutable snapshot: request=%+v", request)
	}
}
