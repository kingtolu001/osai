package main

import (
	"context"
	"testing"
	"time"

	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type lostTradeReply struct {
	tradev1.TradeServiceClient
	expires time.Time
	first   *tradev1.CreateTradeFromAcceptedQuoteRequest
	calls   int
}

func (c *lostTradeReply) CreateTradeFromAcceptedQuote(_ context.Context, req *tradev1.CreateTradeFromAcceptedQuoteRequest, _ ...grpc.CallOption) (*tradev1.TradeResponse, error) {
	c.calls++
	if c.first == nil {
		c.first = proto.Clone(req).(*tradev1.CreateTradeFromAcceptedQuoteRequest)
		// The Trade owner committed before expiry, but its reply was lost.
		<-time.After(time.Until(c.expires.Add(time.Millisecond)))
		return nil, status.Error(codes.Unavailable, "reply lost after commit")
	}
	if !proto.Equal(c.first, req) {
		return nil, status.Error(codes.AlreadyExists, "request changed")
	}
	return &tradev1.TradeResponse{TradeId: "committed-trade"}, nil
}

func TestAcceptQuoteRecoversLostTradeReplyAfterExpiry(t *testing.T) {
	store := quotecore.NewQuoteStore()
	policy := quoteTestPolicy("inst-recovery")
	policy.SourceCurrency, policy.DestinationCurrency = "NGN", "NGN"
	request := quotecore.QuoteRequest{CustomerID: policy.InstitutionID, QuoteMode: quotecore.QuoteModeFixedSource, BaseAmountMinor: 10000, BaseCurrency: "NGN", QuoteCurrency: "NGN", DestinationRail: "sim_lp_1", IdempotencyKey: "quote-recovery"}
	priced, err := quotecore.PriceQuote(policy, quotecore.QuotePricingInput{Mode: request.QuoteMode, SourceAmountMinor: request.BaseAmountMinor, RateScaled: quotecore.RateScale})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(100 * time.Millisecond)
	q, err := store.CreatePricedFromProvider(request, quotecore.ProviderQuote{ProviderID: "sim_lp_1", BaseCurrency: "NGN", QuoteCurrency: "NGN", BaseAmountMinor: priced.SourcePrincipalMinor, RateMinor: quotecore.RateScale, AmountOutMinor: priced.DestinationMinor, ExpiresAt: expires}, policy, priced)
	if err != nil {
		t.Fatal(err)
	}
	client := &lostTradeReply{expires: expires}
	server := &grpcServer{store: store, tradeClient: client}
	accept := &quotev1.AcceptQuoteRequest{InstitutionId: policy.InstitutionID, QuoteId: q.ID, IdempotencyKey: "accept-recovery", BeneficiaryId: "beneficiary", CorrelationId: "first-correlation"}
	if _, err = server.AcceptQuote(context.Background(), accept); err == nil {
		t.Fatal("lost response unexpectedly succeeded")
	}
	conflict := proto.Clone(accept).(*quotev1.AcceptQuoteRequest)
	conflict.BeneficiaryId = "different-beneficiary"
	if _, err = server.AcceptQuote(context.Background(), conflict); err == nil || client.calls != 1 {
		t.Fatal("conflicting selection reached Trade")
	}
	accept.CorrelationId = "retry-correlation"
	response, err := server.AcceptQuote(context.Background(), accept)
	if err != nil || response.TradeId != "committed-trade" || client.calls != 2 {
		t.Fatal("ambiguous acceptance could not resume after expiry", err)
	}
	stored, _ := store.Get(q.ID)
	if stored.Status != quotecore.QuoteStatusAccepted || stored.AcceptanceCorrelation != "first-correlation" {
		t.Fatal("acceptance state or original correlation lost")
	}
}
