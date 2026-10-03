package main

import (
	"context"
	"testing"

	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/trade-orchestrator/tradecore"
	"google.golang.org/grpc"
)

type testCustomerClient struct {
	customerv1.CustomerServiceClient
	beneficiary *customerv1.Beneficiary
}

func (c testCustomerClient) GetApprovedBeneficiary(context.Context, *customerv1.BeneficiaryRequest, ...grpc.CallOption) (*customerv1.Beneficiary, error) {
	return c.beneficiary, nil
}

type testSettlementClient struct {
	settlementv1.SettlementServiceClient
	request *settlementv1.CreateSettlementForTradeRequest
}

func (c *testSettlementClient) CreateSettlementForTrade(_ context.Context, request *settlementv1.CreateSettlementForTradeRequest, _ ...grpc.CallOption) (*settlementv1.SettlementResponse, error) {
	c.request = request
	return &settlementv1.SettlementResponse{SettlementId: "set_test"}, nil
}

func TestApprovedInstitutionBeneficiaryFeedsSettlement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		beneficiary *customerv1.Beneficiary
		allowed     bool
	}{
		{"approved", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, true},
		{"another institution", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_2", Status: "ACTIVE", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, false},
		{"disabled", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "DISABLED", ApprovalStatus: "APPROVED", BankCode: "044", AccountNumber: "0690000040"}, false},
		{"unapproved", &customerv1.Beneficiary{BeneficiaryId: "ben_1", InstitutionId: "inst_1", Status: "ACTIVE", ApprovalStatus: "PENDING", BankCode: "044", AccountNumber: "0690000040"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OSAI_FLW_TRADE_ROUTING_ENABLED", "true")
			settlement := &testSettlementClient{}
			server := newTradeServer(tradecore.NewStore(), settlement).withCustomerClient(testCustomerClient{beneficiary: tc.beneficiary})
			_, err := server.CreateTradeFromAcceptedQuote(context.Background(), &tradev1.CreateTradeFromAcceptedQuoteRequest{InstitutionId: "inst_1", QuoteId: "quo_1", IdempotencyKey: "key_1", BaseAmountMinor: 10000, BaseCurrency: "USD", QuoteCurrency: "NGN", Amount: &tradev1.Money{AmountMinor: 100000, Currency: "NGN"}, BeneficiaryId: "ben_1", SettlementProviderId: "flutterwave"})
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if settlement.request == nil || settlement.request.ProviderId != "flutterwave" || settlement.request.BeneficiaryId != "ben_1" || settlement.request.BankCode != "" || settlement.request.AccountNumber != "" || settlement.request.Amount.AmountMinor != 100000 || settlement.request.Amount.Currency != "NGN" {
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
