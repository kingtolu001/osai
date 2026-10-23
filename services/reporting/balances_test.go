package main

import (
	"context"
	"math"
	"testing"

	ledgerv1 "github.com/osai/osai/proto/osai/ledger/v1"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type balanceLedgerFixture struct {
	ledgerv1.LedgerServiceClient
	response *ledgerv1.CustomerBalanceResponse
}

func (c balanceLedgerFixture) GetCustomerBalances(context.Context, *ledgerv1.CustomerReadRequest, ...grpc.CallOption) (*ledgerv1.CustomerBalanceResponse, error) {
	return c.response, nil
}

func TestReportingBalancesIncludeRestrictedFundsAndRejectOverflow(t *testing.T) {
	s := &grpcServer{ledgerClient: balanceLedgerFixture{response: &ledgerv1.CustomerBalanceResponse{Balances: []*ledgerv1.CustomerBalance{{Currency: "USD", AvailableMinor: 30, HeldMinor: 20, RestrictedMinor: 95}}}}}
	response, err := s.GetBalances(context.Background(), &reportingv1.GetBalancesRequest{InstitutionId: "sbx:institution"})
	if err != nil || len(response.Balances) != 1 || response.Balances[0].TotalMinor != 145 || response.Balances[0].RestrictedMinor != 95 || response.Balances[0].AvailableMinor != 30 {
		t.Fatalf("restricted balance lost or made spendable: %v %v", response, err)
	}
	for _, invalid := range []*ledgerv1.CustomerBalanceResponse{nil, {Balances: []*ledgerv1.CustomerBalance{nil}}, {Balances: []*ledgerv1.CustomerBalance{{AvailableMinor: math.MaxInt64, RestrictedMinor: 1}}}} {
		s.ledgerClient = balanceLedgerFixture{response: invalid}
		if _, err = s.GetBalances(context.Background(), &reportingv1.GetBalancesRequest{InstitutionId: "sbx:institution"}); status.Code(err) != codes.Internal {
			t.Fatalf("invalid ledger balance accepted: %v", err)
		}
	}
}
