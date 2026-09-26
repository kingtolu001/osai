package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osai/osai/pkg/correlation"
	"github.com/osai/osai/pkg/observability"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"google.golang.org/grpc"
)

type grpcCustomerClient struct {
	client customerv1.CustomerServiceClient
}

type grpcQuoteClient struct{ client quotev1.QuoteServiceClient }

type grpcTradeClient struct{ client tradev1.TradeServiceClient }

type grpcSettlementClient struct {
	client settlementv1.SettlementServiceClient
}

type grpcReportingClient struct {
	client reportingv1.ReportingServiceClient
}

func NewCustomerGRPCClient(conn grpc.ClientConnInterface) CustomerClient {
	return &grpcCustomerClient{client: customerv1.NewCustomerServiceClient(conn)}
}

func NewQuoteGRPCClient(conn grpc.ClientConnInterface) QuoteClient {
	return &grpcQuoteClient{client: quotev1.NewQuoteServiceClient(conn)}
}

func NewTradeGRPCClient(conn grpc.ClientConnInterface) TradeClient {
	return &grpcTradeClient{client: tradev1.NewTradeServiceClient(conn)}
}

func NewSettlementGRPCClient(conn grpc.ClientConnInterface) SettlementClient {
	return &grpcSettlementClient{client: settlementv1.NewSettlementServiceClient(conn)}
}

func NewReportingGRPCClient(conn grpc.ClientConnInterface) ReadModelClient {
	return &grpcReportingClient{client: reportingv1.NewReportingServiceClient(conn)}
}

func (c *grpcCustomerClient) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	resp, err := c.client.AuthenticateAPIClient(context.Background(), &customerv1.CredentialAuthRequest{PublicId: publicID, Secret: secret})
	if err != nil {
		return CustomerAuthContext{}, err
	}
	return CustomerAuthContext{InstitutionID: resp.InstitutionId, InstitutionStatus: resp.InstitutionStatus, KYBStatus: resp.KybStatus, APIAccessStatus: resp.ApiAccessStatus, APIEnabled: resp.ApiEnabled, Entitlements: resp.Entitlements}, nil
}

func (c *grpcCustomerClient) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	resp, err := c.client.GetInstitutionAccess(context.Background(), &customerv1.InstitutionRequest{InstitutionId: instID})
	if err != nil {
		return CustomerAuthContext{}, err
	}
	return CustomerAuthContext{InstitutionID: resp.InstitutionId, InstitutionStatus: resp.InstitutionStatus, KYBStatus: resp.KybStatus, APIAccessStatus: resp.ApiAccessStatus, APIEnabled: resp.ApiEnabled, Entitlements: resp.Entitlements}, nil
}

func (c *grpcCustomerClient) GetInstitution(instID string) (CustomerInstitution, error) {
	resp, err := c.client.GetInstitution(context.Background(), &customerv1.InstitutionRequest{InstitutionId: instID})
	if err != nil {
		return CustomerInstitution{}, err
	}
	return CustomerInstitution{ID: resp.InstitutionId, Name: resp.Name, Status: resp.Status, KYBStatus: resp.KybStatus, Country: resp.Country, APIEnabled: resp.ApiEnabled}, nil
}

func (c *grpcCustomerClient) GetWebhookConfiguration(instID string) (WebhookConfig, error) {
	resp, err := c.client.GetWebhookConfiguration(context.Background(), &customerv1.InstitutionRequest{InstitutionId: instID})
	if err != nil {
		return WebhookConfig{}, err
	}
	return WebhookConfig{InstitutionID: resp.InstitutionId, WebhookURL: resp.WebhookUrl, Enabled: resp.Enabled, Status: resp.Status}, nil
}

func (q *grpcQuoteClient) CreateQuote(req QuoteRequest) (QuoteResponse, error) {
	ctx := correlation.WithContext(context.Background(), req.CorrelationID)
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := q.client.CreateQuote(ctx, &quotev1.CreateQuoteRequest{InstitutionId: req.InstitutionID, IdempotencyKey: req.IdempotencyKey, CorrelationId: req.CorrelationID, BaseAmountMinor: req.BaseAmountMinor, BaseCurrency: req.BaseCurrency, QuoteCurrency: req.QuoteCurrency, DestinationRail: req.DestinationRail, Urgency: req.Urgency})
	if err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: resp.QuoteId, Status: resp.Status, BaseAmountMinor: resp.BaseAmountMinor, BaseCurrency: resp.BaseCurrency, QuoteCurrency: resp.QuoteCurrency, DestinationRail: resp.DestinationRail, AmountOutMinor: resp.AmountOutMinor, RateMinor: resp.RateMinor, FeeMinor: resp.FeeMinor, ExpiresAt: parseExpiration(resp.ExpiresAt), CorrelationID: resp.CorrelationId}, nil
}

func (q *grpcQuoteClient) GetQuote(institutionID, quoteID string) (QuoteResponse, error) {
	resp, err := q.client.GetQuote(context.Background(), &quotev1.GetQuoteRequest{InstitutionId: institutionID, QuoteId: quoteID})
	if err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: resp.QuoteId, Status: resp.Status, BaseAmountMinor: resp.BaseAmountMinor, BaseCurrency: resp.BaseCurrency, QuoteCurrency: resp.QuoteCurrency, DestinationRail: resp.DestinationRail, AmountOutMinor: resp.AmountOutMinor, RateMinor: resp.RateMinor, FeeMinor: resp.FeeMinor, ExpiresAt: parseExpiration(resp.ExpiresAt), CorrelationID: resp.CorrelationId}, nil
}

func (q *grpcQuoteClient) AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID string) (QuoteResponse, error) {
	ctx := correlation.WithContext(context.Background(), correlationID)
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := q.client.AcceptQuote(ctx, &quotev1.AcceptQuoteRequest{InstitutionId: institutionID, QuoteId: quoteID, IdempotencyKey: idempotencyKey, CorrelationId: correlationID})
	if err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: resp.QuoteId, TradeID: resp.TradeId, Status: resp.Status, BaseAmountMinor: resp.BaseAmountMinor, BaseCurrency: resp.BaseCurrency, QuoteCurrency: resp.QuoteCurrency, DestinationRail: resp.DestinationRail, AmountOutMinor: resp.AmountOutMinor, RateMinor: resp.RateMinor, FeeMinor: resp.FeeMinor, ExpiresAt: parseExpiration(resp.ExpiresAt), CorrelationID: resp.CorrelationId}, nil
}

func (t *grpcTradeClient) GetTrade(institutionID, tradeID string) (TradeReadResponse, error) {
	resp, err := t.client.GetTrade(context.Background(), &tradev1.GetTradeRequest{InstitutionId: institutionID, TradeId: tradeID})
	if err != nil {
		return TradeReadResponse{}, err
	}
	return TradeReadResponse{TradeID: resp.TradeId, InstitutionID: resp.InstitutionId, QuoteID: resp.QuoteId, Status: resp.Status, BaseAmountMinor: resp.BaseAmountMinor, BaseCurrency: resp.BaseCurrency, QuoteCurrency: resp.QuoteCurrency, SettlementID: resp.SettlementId, CorrelationID: resp.CorrelationId}, nil
}

func (s *grpcSettlementClient) GetSettlement(institutionID, settlementID string) (SettlementReadResponse, error) {
	resp, err := s.client.GetSettlement(context.Background(), &settlementv1.GetSettlementRequest{InstitutionId: institutionID, SettlementId: settlementID})
	if err != nil {
		return SettlementReadResponse{}, err
	}
	return SettlementReadResponse{SettlementID: resp.SettlementId, InstitutionID: resp.InstitutionId, TradeID: resp.TradeId, QuoteID: resp.QuoteId, Status: resp.Status, Beneficiary: resp.Beneficiary, AmountMinor: resp.AmountMinor, Currency: resp.Currency, CorrelationID: resp.CorrelationId}, nil
}

func (r *grpcReportingClient) GetBalances(institutionID string) (BalanceReadResponse, error) {
	resp, err := r.client.GetBalances(context.Background(), &reportingv1.GetBalancesRequest{InstitutionId: institutionID})
	if err != nil {
		return BalanceReadResponse{}, err
	}
	if len(resp.Balances) == 0 {
		return BalanceReadResponse{}, fmt.Errorf("no balances found")
	}
	balance := resp.Balances[0]
	return BalanceReadResponse{InstitutionID: balance.InstitutionId, Currency: balance.Currency, AvailableMinor: balance.AvailableMinor, HeldMinor: balance.HeldMinor, TotalMinor: balance.TotalMinor}, nil
}

func (r *grpcReportingClient) GetTransactions(institutionID string, page, pageSize int) (TransactionPage, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 25
	}
	resp, err := r.client.ListTransactions(context.Background(), &reportingv1.ListTransactionsRequest{InstitutionId: institutionID, Page: int32(page), PageSize: int32(pageSize)})
	if err != nil {
		return TransactionPage{}, err
	}
	items := make([]TransactionItem, 0, len(resp.Items))
	for _, item := range resp.Items {
		occurredAt := time.Time{}
		if strings.TrimSpace(item.OccurredAt) != "" {
			if parsed, err := time.Parse(time.RFC3339, item.OccurredAt); err == nil {
				occurredAt = parsed
			}
		}
		items = append(items, TransactionItem{ID: item.TransactionId, Type: item.Type, AmountMinor: item.AmountMinor, Currency: item.Currency, Status: item.Status, OccurredAt: occurredAt, CorrelationID: item.CorrelationId})
	}
	return TransactionPage{Items: items, Page: int(resp.Page), PageSize: int(resp.PageSize), Total: int(resp.Total)}, nil
}

func parseExpiration(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	if strings.Contains(value, "Z") {
		return time.Now()
	}
	return time.Time{}
}
