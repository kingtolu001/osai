package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/osai/osai/pkg/pricing"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type testCustomerClient struct {
	customerv1.CustomerServiceClient
	beneficiary *customerv1.Beneficiary
}

func TestSandboxFXRejectsUnboundBeneficiaryBeforeCreatingTrade(t *testing.T) {
	for _, tc := range []struct {
		name, requested, returned string
		code                      codes.Code
	}{
		{"empty", "", "", codes.InvalidArgument},
		{"malformed", "ben_invalid", "ben_invalid", codes.InvalidArgument},
		{"different approved beneficiary", "sbx:requested", "sbx:other", codes.FailedPrecondition},
		{"missing returned identity", "sbx:requested", "", codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tradecore.NewStore()
			server := newTradeServer(store, nil).withCustomerClient(testCustomerClient{beneficiary: &customerv1.Beneficiary{BeneficiaryId: tc.returned, InstitutionId: "sbx:institution", Status: "ACTIVE", ApprovalStatus: "APPROVED"}}).withLedgerClient(testLedgerClient{})
			_, err := server.createSandboxFXTrade(context.Background(), &tradev1.CreateTradeFromAcceptedQuoteRequest{InstitutionId: "sbx:institution", QuoteId: "sbx:quote", BeneficiaryId: tc.requested}, pricing.Snapshot{})
			if status.Code(err) != tc.code {
				t.Fatalf("expected %v, got %v", tc.code, err)
			}
			if _, exists := store.GetByQuoteID("sbx:quote"); exists {
				t.Fatal("unbound beneficiary created a trade before validation")
			}
		})
	}
}

func (c testCustomerClient) GetApprovedBeneficiary(context.Context, *customerv1.BeneficiaryRequest, ...grpc.CallOption) (*customerv1.Beneficiary, error) {
	return c.beneficiary, nil
}

type testSettlementClient struct {
	settlementv1.SettlementServiceClient
	request *settlementv1.CreateSettlementForTradeRequest
	status  string
}

type testLedgerClient struct {
	ledgerv1.LedgerServiceClient
	reject bool
	calls  *[]*ledgerv1.FundingReservationCommand
}

func (c testLedgerClient) ReserveFunding(_ context.Context, request *ledgerv1.FundingReservationCommand, _ ...grpc.CallOption) (*ledgerv1.FundingReservationResponse, error) {
	if c.calls != nil {
		copy := proto.Clone(request).(*ledgerv1.FundingReservationCommand)
		*c.calls = append(*c.calls, copy)
	}
	if c.reject {
		return nil, status.Error(codes.ResourceExhausted, "insufficient institution prefund")
	}
	return &ledgerv1.FundingReservationResponse{State: "ACTIVE", JournalId: "jt_test_hold"}, nil
}

func (c *testSettlementClient) CreateSettlementForTrade(_ context.Context, request *settlementv1.CreateSettlementForTradeRequest, _ ...grpc.CallOption) (*settlementv1.SettlementResponse, error) {
	c.request = request
	return &settlementv1.SettlementResponse{SettlementId: "set_test"}, nil
}

func (c *testSettlementClient) GetSettlement(_ context.Context, request *settlementv1.GetSettlementRequest, _ ...grpc.CallOption) (*settlementv1.SettlementResponse, error) {
	return &settlementv1.SettlementResponse{SettlementId: request.SettlementId, InstitutionId: request.InstitutionId, Status: c.status}, nil
}

func TestApprovedInstitutionBeneficiaryFeedsSettlement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		beneficiary *customerv1.Beneficiary
		allowed     bool
	}{
		{"approved", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, true},
		{"another institution", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_2", Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, false},
		{"another beneficiary", &customerv1.Beneficiary{BeneficiaryId: "ben_2", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "APPROVED"}, false},
		{"missing beneficiary identity", &customerv1.Beneficiary{InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "APPROVED"}, false},
		{"disabled", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "DISABLED", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, false},
		{"unapproved", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "PENDING", BankCode: "044", AccountNumber: "0690000040"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "true")
			settlement := &testSettlementClient{}
			server := newTradeServer(tradecore.NewStore(), settlement).withCustomerClient(testCustomerClient{beneficiary: tc.beneficiary}).withLedgerClient(testLedgerClient{})
			_, err := server.CreateTradeFromAcceptedQuote(context.Background(), &tradev1.CreateTradeFromAcceptedQuoteRequest{InstitutionId: "inst_1", QuoteId: "quo_1", IdempotencyKey: "key_1", BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "NGN", Amount: &tradev1.Money{AmountMinor: 10000, Currency: "NGN"}, BeneficiaryId: "ben_1", SettlementProviderId: "flutterwave"})
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if settlement.request == nil || settlement.request.ProviderId != "flutterwave" || settlement.request.BeneficiaryId != "ben_1" || settlement.request.BankCode != "" || settlement.request.AccountNumber != "" || settlement.request.Amount.AmountMinor != 10000 || settlement.request.Amount.Currency != "NGN" {
					t.Fatalf("wrong transfer instruction: %+v", settlement.request)
				}
			} else if err == nil || settlement.request != nil {
				t.Fatalf("unapproved account reached settlement: %v %+v", err, settlement.request)
			}
		})
	}
}

func TestCustomerFlutterwaveRoutingDefaultsClosed(t *testing.T) {
	t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "false")
	settlement := &testSettlementClient{}
	server := newTradeServer(tradecore.NewStore(), settlement).withCustomerClient(testCustomerClient{beneficiary: &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}})
	_, err := server.CreateTradeFromAcceptedQuote(context.Background(), &tradev1.CreateTradeFromAcceptedQuoteRequest{InstitutionId: "inst_1", QuoteId: "quo_1", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", Amount: &tradev1.Money{AmountMinor: 100000, Currency: "NGN"}, BeneficiaryId: "ben_1", SettlementProviderId: "flutterwave"})
	if err == nil || settlement.request != nil {
		t.Fatalf("default-off route admitted a trade: %v", err)
	}
}

func TestTradeRejectsUnsupportedFXOrFeeFunding(t *testing.T) {
	t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "true")
	for _, test := range []struct {
		name  string
		quote *tradev1.CreateTradeFromAcceptedQuoteRequest
	}{
		{
			name: "cross currency",
			quote: &tradev1.CreateTradeFromAcceptedQuoteRequest{
				InstitutionId: "inst_1", QuoteId: "quo_fx", BaseAmountMinor: 10000,
				BaseCurrency: "NGN", QuoteCurrency: "USD",
				Amount: &tradev1.Money{AmountMinor: 1000, Currency: "USD"},
			},
		},
		{
			name: "amount does not include source fee",
			quote: &tradev1.CreateTradeFromAcceptedQuoteRequest{
				InstitutionId: "inst_1", QuoteId: "quo_fee", BaseAmountMinor: 10000,
				BaseCurrency: "NGN", QuoteCurrency: "NGN",
				Amount: &tradev1.Money{AmountMinor: 9500, Currency: "NGN"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := tradecore.NewStore()
			settlement := &testSettlementClient{}
			server := newTradeServer(store, settlement).withLedgerClient(testLedgerClient{})
			_, err := server.CreateTradeFromAcceptedQuote(context.Background(), test.quote)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("unsupported funding should fail closed, got %v", err)
			}
			if _, exists := store.GetByQuoteID(test.quote.QuoteId); exists || settlement.request != nil {
				t.Fatalf("unsupported funding created trade or settlement: trade_exists=%v settlement=%+v", exists, settlement.request)
			}
		})
	}
}

func TestTradeCreationPersistsFundingRequiredWithoutSettlement(t *testing.T) {
	t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "true")
	settlement := &testSettlementClient{}
	ledger := &testLedgerClient{reject: true}
	store := tradecore.NewStore()
	server := newTradeServer(store, settlement).withCustomerClient(testCustomerClient{beneficiary: &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}}).withLedgerClient(ledger)

	response, err := server.CreateTradeFromAcceptedQuote(context.Background(), &tradev1.CreateTradeFromAcceptedQuoteRequest{
		InstitutionId:        "inst_1",
		QuoteId:              "quo_unfunded",
		IdempotencyKey:       "key_unfunded",
		BaseAmountMinor:      10000,
		BaseCurrency:         "NGN",
		QuoteCurrency:        "NGN",
		Amount:               &tradev1.Money{AmountMinor: 10000, Currency: "NGN"},
		BeneficiaryId:        "ben_1",
		SettlementProviderId: "flutterwave",
	})
	if err != nil {
		t.Fatalf("unfunded trade should remain recoverable: %v", err)
	}
	if response.Status != string(tradecore.StateFundingRequired) {
		t.Fatalf("expected FUNDING_REQUIRED, got %s", response.Status)
	}
	if trade, ok := store.GetTrade(response.TradeId); !ok || trade.State != tradecore.StateFundingRequired {
		t.Fatalf("expected persisted FUNDING_REQUIRED trade, got %+v (exists=%v)", trade, ok)
	}
	if settlement.request != nil {
		t.Fatalf("unfunded trade still reached settlement: %+v", settlement.request)
	}
}

func TestPricedFeeTradeReservesExactDebitAndWithholdsPayout(t *testing.T) {
	t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "true")
	snapshot := pricedFeeSnapshot(t, time.Now().UTC().Add(time.Minute))
	protoSnapshot := pricingSnapshotToProto(snapshot)
	var commands []*ledgerv1.FundingReservationCommand
	settlement := &testSettlementClient{}
	store := tradecore.NewStore()
	server := newTradeServer(store, settlement).withLedgerClient(testLedgerClient{calls: &commands})
	request := &tradev1.CreateTradeFromAcceptedQuoteRequest{
		InstitutionId: "inst_fee", QuoteId: "quo_fee", IdempotencyKey: "accept-fee",
		BaseAmountMinor: snapshot.SourcePrincipalMinor, BaseCurrency: snapshot.SourceCurrency,
		QuoteCurrency:        snapshot.DestinationCurrency,
		Amount:               &tradev1.Money{AmountMinor: snapshot.DestinationAmountMinor, Currency: snapshot.DestinationCurrency},
		SettlementProviderId: "sim_lp_1", PricingSnapshot: protoSnapshot,
	}
	response, err := server.CreateTradeFromAcceptedQuote(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != string(tradecore.StateFeeAccountingRequired) ||
		response.BaseAmountMinor != snapshot.SourcePrincipalMinor ||
		response.PricingSnapshot.GetTotalDebitMinor() != snapshot.TotalDebitMinor {
		t.Fatalf("fee-bearing trade did not retain its immutable terms: %+v", response)
	}
	if len(commands) != 1 || commands[0].Amount.AmountMinor != 10_000 ||
		commands[0].SourcePrincipalMinor != 9_500 || commands[0].FeeMinor != 500 ||
		commands[0].PricingHash != snapshot.Hash {
		t.Fatalf("Ledger did not receive exact all-in reservation terms: %+v", commands)
	}
	var ledgerSnapshot pricing.Snapshot
	if err := json.Unmarshal([]byte(commands[0].PricingSnapshotJson), &ledgerSnapshot); err != nil ||
		ledgerSnapshot != snapshot {
		t.Fatalf("Ledger did not receive the complete immutable pricing snapshot: snapshot=%+v err=%v", ledgerSnapshot, err)
	}
	if settlement.request != nil {
		t.Fatalf("fee-bearing trade reached payout before Ledger fee accounting exists: %+v", settlement.request)
	}

	replay, err := server.CreateTradeFromAcceptedQuote(context.Background(), request)
	if err != nil || replay.TradeId != response.TradeId || replay.Status != response.Status {
		t.Fatalf("idempotent fee acceptance replay changed trade: response=%+v err=%v", replay, err)
	}
	if len(commands) != 2 || settlement.request != nil {
		t.Fatalf("funding replay or payout behavior incorrect: ledger_calls=%d settlement=%+v", len(commands), settlement.request)
	}
	if trades, total, err := store.ListTrades("inst_fee", tradecore.TradeListFilter{Page: 1, PageSize: 10}); err != nil || total != 1 || len(trades) != 1 {
		t.Fatalf("duplicate acceptance created another trade: total=%d err=%v", total, err)
	}

	altered := proto.Clone(request).(*tradev1.CreateTradeFromAcceptedQuoteRequest)
	altered.PricingSnapshot = pricingSnapshotToProto(snapshot)
	altered.PricingSnapshot.ExpiresAt = time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano)
	alteredSnapshot := pricingSnapshotFromProto(altered.PricingSnapshot)
	alteredSnapshot.Hash, _ = pricing.Hash(alteredSnapshot)
	altered.PricingSnapshot = pricingSnapshotToProto(alteredSnapshot)
	if _, err = server.CreateTradeFromAcceptedQuote(context.Background(), altered); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("altered quote snapshot replay was accepted: %v", err)
	}

	expired := pricedFeeSnapshot(t, time.Now().UTC().Add(-time.Minute))
	expiredRequest := &tradev1.CreateTradeFromAcceptedQuoteRequest{
		InstitutionId: "inst_fee", QuoteId: "quo_expired", IdempotencyKey: "accept-expired",
		BaseAmountMinor: expired.SourcePrincipalMinor, BaseCurrency: expired.SourceCurrency,
		QuoteCurrency:   expired.DestinationCurrency,
		Amount:          &tradev1.Money{AmountMinor: expired.DestinationAmountMinor, Currency: expired.DestinationCurrency},
		PricingSnapshot: pricingSnapshotToProto(expired),
	}
	if _, err = server.CreateTradeFromAcceptedQuote(context.Background(), expiredRequest); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expired quote was accepted: %v", err)
	}
	if _, exists := store.GetByQuoteID(expiredRequest.QuoteId); exists {
		t.Fatal("expired quote created a trade")
	}
}

func TestPricedFeeTradeInsufficientFundsRemainsRecoverable(t *testing.T) {
	snapshot := pricedFeeSnapshot(t, time.Now().UTC().Add(time.Minute))
	store := tradecore.NewStore()
	server := newTradeServer(store, &testSettlementClient{}).withLedgerClient(testLedgerClient{reject: true})
	response, err := server.CreateTradeFromAcceptedQuote(context.Background(), &tradev1.CreateTradeFromAcceptedQuoteRequest{
		InstitutionId: "inst_fee_short", QuoteId: "quo_fee_short", IdempotencyKey: "accept-fee-short",
		BaseAmountMinor: snapshot.SourcePrincipalMinor, BaseCurrency: snapshot.SourceCurrency,
		QuoteCurrency:   snapshot.DestinationCurrency,
		Amount:          &tradev1.Money{AmountMinor: snapshot.DestinationAmountMinor, Currency: snapshot.DestinationCurrency},
		PricingSnapshot: pricingSnapshotToProto(snapshot),
	})
	if err != nil || response.Status != string(tradecore.StateFundingRequired) {
		t.Fatalf("insufficient funds should leave a recoverable trade: response=%+v err=%v", response, err)
	}
	if trade, ok := store.GetTrade(response.TradeId); !ok || trade.PricingSnapshot == nil ||
		trade.PricingSnapshot.TotalDebitMinor != snapshot.TotalDebitMinor || trade.State != tradecore.StateFundingRequired {
		t.Fatalf("funding-required trade lost the immutable quote terms: trade=%+v exists=%v", trade, ok)
	}
}

func pricedFeeSnapshot(t *testing.T, expires time.Time) pricing.Snapshot {
	t.Helper()
	snapshot := pricing.Snapshot{
		Mode: "FIXED_SOURCE", RequestedAmountMinor: 10_000, SourcePrincipalMinor: 9_500,
		FeeMinor: 500, TotalDebitMinor: 10_000, DestinationAmountMinor: 9_500,
		SourceCurrency: "NGN", DestinationCurrency: "NGN", RateScaled: pricing.RateScale,
		RateDirection: "NGN_PER_NGN", RateScale: pricing.RateScale,
		SourceExponent: 2, DestinationExponent: 2, FeeScheduleID: "fee_test", FeeScheduleVersion: 1,
		FixedFeeMinor: 500, MinimumFeeMinor: 500, MaximumFeeMinor: 500,
		MinimumPrincipalMinor: 1, MaximumPrincipalMinor: 1_000_000,
		MinimumDebitMinor: 1, MaximumDebitMinor: 2_000_000,
		MinimumDestinationMinor: 1, MaximumDestinationMinor: 2_000_000,
		ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	}
	hash, err := pricing.Hash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Hash = hash
	if err := pricing.Validate(snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestTradeReadReflectsDeduplicatedSettlementFinality(t *testing.T) {
	for _, tc := range []struct {
		settlementStatus string
		tradeStatus      tradecore.TradeState
	}{
		{"CONFIRMED", tradecore.StateSettled},
		{"FAILED", tradecore.StateFailed},
	} {
		t.Run(tc.settlementStatus, func(t *testing.T) {
			store := tradecore.NewStore()
			trade, err := store.CreateTrade("inst_1", "quo_1", "key_1", "corr_1", 10000, "NGN", "NGN", "ben_1", "flutterwave")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetSettlementID(trade.ID, "set_1"); err != nil {
				t.Fatal(err)
			}
			if err := store.SetStatus(trade.ID, tradecore.StateSettling); err != nil {
				t.Fatal(err)
			}
			event := tradecore.FinalityEvent{EventID: "evt_set_1_" + tc.settlementStatus, InstitutionID: "inst_1", SettlementID: "set_1", TradeID: trade.ID, Status: tc.tradeStatus, CorrelationID: "corr_1"}
			if err := store.ApplySettlementFinality(event); err != nil {
				t.Fatal(err)
			}
			if err := store.ApplySettlementFinality(event); err != nil {
				t.Fatalf("duplicate finality event was not idempotent: %v", err)
			}
			server := newTradeServer(store, &testSettlementClient{status: tc.settlementStatus})
			response, err := server.GetTrade(context.Background(), &tradev1.GetTradeRequest{InstitutionId: "inst_1", TradeId: trade.ID})
			if err != nil {
				t.Fatal(err)
			}
			if response.Status != string(tc.tradeStatus) {
				t.Fatalf("expected trade status %s, got %s", tc.tradeStatus, response.Status)
			}
			persisted, ok := store.GetTrade(trade.ID)
			if !ok || persisted.Status != string(tc.tradeStatus) {
				t.Fatalf("expected owner store status %s, got %+v (exists=%v)", tc.tradeStatus, persisted, ok)
			}
		})
	}
}

func TestTradeRejectsSettlementFinalityFromAnotherInstitution(t *testing.T) {
	store := tradecore.NewStore()
	trade, err := store.CreateTrade("inst_1", "quo_1", "key_1", "corr_1", 10000, "NGN", "NGN")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSettlementID(trade.ID, "set_1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetStatus(trade.ID, tradecore.StateSettling); err != nil {
		t.Fatal(err)
	}
	err = store.ApplySettlementFinality(tradecore.FinalityEvent{EventID: "evt_wrong_inst", InstitutionID: "inst_2", SettlementID: "set_1", TradeID: trade.ID, Status: tradecore.StateSettled, CorrelationID: "corr_1"})
	if err == nil {
		t.Fatal("cross-institution settlement finality was accepted")
	}
}
