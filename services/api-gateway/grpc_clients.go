package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osai/osai/pkg/correlation"
	"github.com/osai/osai/pkg/observability"
	customerv1 "github.com/osai/osai/proto/osai/customer/v1"
	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	quotev1 "github.com/osai/osai/proto/osai/quote/v1"
	reportingv1 "github.com/osai/osai/proto/osai/reporting/v1"
	settlementv1 "github.com/osai/osai/proto/osai/settlement/v1"
	tradev1 "github.com/osai/osai/proto/osai/trade/v1"
	"github.com/osai/osai/services/quote/quotecore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var errInvalidWebhookConfiguration = errors.New("invalid webhook configuration")

type customerServiceClientAdapter struct{ svc *CustomerService }

type quoteServiceClientAdapter struct{ store *quotecore.QuoteStore }

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

type grpcWebhookConfigClient struct {
	client notificationv1.NotificationServiceClient
}

func NewCustomerServiceClientAdapter(svc *CustomerService) CustomerClient {
	if svc == nil {
		return nil
	}
	return &customerServiceClientAdapter{svc: svc}
}

func NewQuoteServiceClientAdapterFromStore(store *quotecore.QuoteStore) QuoteClient {
	if store == nil {
		return nil
	}
	return &quoteServiceClientAdapter{store: store}
}

func NewCustomerGRPCClient(conn grpc.ClientConnInterface) CustomerClient {
	return &grpcCustomerClient{client: customerv1.NewCustomerServiceClient(conn)}
}

func NewQuoteGRPCClient(conn grpc.ClientConnInterface) *grpcQuoteClient {
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

func NewWebhookConfigGRPCClient(conn grpc.ClientConnInterface) WebhookConfigClient {
	return &grpcWebhookConfigClient{client: notificationv1.NewNotificationServiceClient(conn)}
}

func NewWebhookReplayGRPCClient(conn grpc.ClientConnInterface) WebhookReplayClient {
	return &grpcWebhookConfigClient{client: notificationv1.NewNotificationServiceClient(conn)}
}

func (c *grpcWebhookConfigClient) ConfigureWebhook(ctx context.Context, institutionID, endpointURL, signingSecret string, eventTypes []string) error {
	token := strings.TrimSpace(os.Getenv("OSAI_WEBHOOK_ADMIN_TOKEN"))
	if token == "" {
		return errors.New("webhook configuration credential is not configured")
	}
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	_, err := c.client.ConfigureWebhook(ctx, &notificationv1.ConfigureWebhookRequest{
		InstitutionId:        institutionID,
		EndpointUrl:          endpointURL,
		SigningSecret:        signingSecret,
		SubscribedEventTypes: eventTypes,
	})
	if status.Code(err) == codes.InvalidArgument {
		return errInvalidWebhookConfiguration
	}
	return err
}

func (c *grpcWebhookConfigClient) ReplayWebhook(ctx context.Context, institutionID, eventID, idempotencyKey string) error {
	token := strings.TrimSpace(os.Getenv("OSAI_WEBHOOK_ADMIN_TOKEN"))
	if token == "" {
		return errors.New("webhook replay credential is not configured")
	}
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	_, err := c.client.ReplayWebhook(ctx, &notificationv1.ReplayWebhookRequest{
		InstitutionId: institutionID, EventId: eventID, IdempotencyKey: idempotencyKey,
	})
	switch status.Code(err) {
	case codes.NotFound:
		return errWebhookEventNotFound
	case codes.FailedPrecondition:
		return errWebhookReplayNotAllowed
	case codes.AlreadyExists:
		return errIdempotencyConflict
	default:
		return err
	}
}

func (c *customerServiceClientAdapter) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	ok, err := c.svc.ValidateCredential(publicID, secret)
	if err != nil || !ok {
		return CustomerAuthContext{}, fmt.Errorf("invalid credentials")
	}
	c.svc.mu.Lock()
	cred := c.svc.publicToCred[publicID]
	if cred == nil {
		c.svc.mu.Unlock()
		return CustomerAuthContext{}, fmt.Errorf("invalid credentials")
	}
	credential := *cred
	c.svc.mu.Unlock()
	inst, ok := c.svc.GetInstitution(credential.InstitutionID)
	if !ok {
		return CustomerAuthContext{}, fmt.Errorf("institution not found")
	}
	return CustomerAuthContext{InstitutionID: inst.ID, InstitutionStatus: inst.Status, KYBStatus: inst.KYBStatus, APIAccessStatus: "ACTIVE", Environment: credential.Environment, APIEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
}

func (c *customerServiceClientAdapter) GetInstitutionAccess(instID string) (CustomerAuthContext, error) {
	if err := c.svc.ValidateInstitutionAccess(instID); err != nil {
		return CustomerAuthContext{}, err
	}
	inst, _ := c.svc.GetInstitution(instID)
	return CustomerAuthContext{InstitutionID: inst.ID, InstitutionStatus: inst.Status, KYBStatus: inst.KYBStatus, APIAccessStatus: "ACTIVE", APIEnabled: inst.APIEnabled, Entitlements: []string{"quotes:read", "quotes:write"}}, nil
}

func (c *customerServiceClientAdapter) GetInstitution(instID string) (CustomerInstitution, error) {
	inst, ok := c.svc.GetInstitution(instID)
	if !ok {
		return CustomerInstitution{}, fmt.Errorf("institution not found")
	}
	return inst, nil
}

func (c *customerServiceClientAdapter) GetWebhookConfiguration(instID string) (WebhookConfig, error) {
	_, ok := c.svc.GetInstitution(instID)
	if !ok {
		return WebhookConfig{}, fmt.Errorf("institution not found")
	}
	return WebhookConfig{InstitutionID: instID, WebhookURL: "https://example.invalid/webhook", Enabled: true, Status: "ACTIVE"}, nil
}

func (c *customerServiceClientAdapter) CreateCredential(institutionID, label, environment string) (*CustomerCredential, string, error) {
	cred, secret, err := c.svc.CreateCredential(institutionID, label, environment)
	if err != nil {
		return nil, "", err
	}
	return &cred, secret, nil
}

func (c *customerServiceClientAdapter) RotateCredential(institutionID, publicID string) (*CustomerCredential, string, error) {
	cred, secret, err := c.svc.RotateCredential(institutionID, publicID)
	if err != nil {
		return nil, "", err
	}
	return &cred, secret, nil
}

func (c *customerServiceClientAdapter) RevokeCredential(institutionID, publicID string) error {
	return c.svc.RevokeCredential(institutionID, publicID)
}

func (c *customerServiceClientAdapter) GetCredential(institutionID, publicID string) (*CustomerCredential, error) {
	return c.svc.GetCredential(institutionID, publicID)
}

func (c *customerServiceClientAdapter) ListCredentials(institutionID string) ([]*CustomerCredential, error) {
	return c.svc.ListCredentials(institutionID)
}

func (q *quoteServiceClientAdapter) CreateQuote(req QuoteRequest) (QuoteResponse, error) {
	quote, err := q.store.Create(quotecore.QuoteRequest{
		CustomerID:             req.InstitutionID,
		QuoteMode:              quotecore.QuoteMode(req.QuoteMode),
		BaseAmountMinor:        req.BaseAmountMinor,
		DestinationAmountMinor: req.DestinationAmountMinor,
		BaseCurrency:           req.BaseCurrency,
		QuoteCurrency:          req.QuoteCurrency,
		DestinationRail:        req.DestinationRail,
		Urgency:                req.Urgency,
		IdempotencyKey:         req.IdempotencyKey,
	}, req.DestinationRail, 1200, 500, req.BaseAmountMinor/100, time.Now().Add(30*time.Minute))
	if err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: quote.ID, Status: string(quote.Status), BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, DestinationRail: quote.Request.DestinationRail, AmountOutMinor: quote.AmountOutMinor, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, ExpiresAt: quote.ExpiresAt}, nil
}

func (q *quoteServiceClientAdapter) GetQuote(institutionID, quoteID string) (QuoteResponse, error) {
	quote, ok := q.store.Get(quoteID)
	if !ok {
		return QuoteResponse{}, fmt.Errorf("quote not found")
	}
	if quote.Request.CustomerID != institutionID {
		return QuoteResponse{}, fmt.Errorf("forbidden")
	}
	return QuoteResponse{QuoteID: quote.ID, Status: string(quote.Status), BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, DestinationRail: quote.Request.DestinationRail, AmountOutMinor: quote.AmountOutMinor, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, ExpiresAt: quote.ExpiresAt}, nil
}

func (q *quoteServiceClientAdapter) AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, _ string) (QuoteResponse, error) {
	quote, ok := q.store.Get(quoteID)
	if !ok {
		return QuoteResponse{}, fmt.Errorf("quote not found")
	}
	if quote.Request.CustomerID != institutionID {
		return QuoteResponse{}, fmt.Errorf("forbidden")
	}
	if quote.Status == quotecore.QuoteStatusAccepted {
		return QuoteResponse{QuoteID: quote.ID, Status: string(quote.Status), TradeID: quote.AcceptedTradeID}, nil
	}
	tradeID := "trd_" + time.Now().UTC().Format("20060102150405")
	if err := quote.Accept(tradeID, idempotencyKey, time.Now().UTC(), q.store); err != nil {
		return QuoteResponse{}, err
	}
	return QuoteResponse{QuoteID: quote.ID, TradeID: tradeID, Status: string(quote.Status), BaseAmountMinor: quote.Request.BaseAmountMinor, BaseCurrency: quote.Request.BaseCurrency, QuoteCurrency: quote.Request.QuoteCurrency, DestinationRail: quote.Request.DestinationRail, AmountOutMinor: quote.AmountOutMinor, RateMinor: quote.RateMinor, FeeMinor: quote.FeeMinor, ExpiresAt: quote.ExpiresAt, CorrelationID: correlationID}, nil
}

func (c *grpcCustomerClient) AuthenticateAPIClient(publicID, secret string) (CustomerAuthContext, error) {
	resp, err := c.client.AuthenticateAPIClient(context.Background(), &customerv1.CredentialAuthRequest{PublicId: publicID, Secret: secret})
	if err != nil {
		return CustomerAuthContext{}, err
	}
	return CustomerAuthContext{InstitutionID: resp.InstitutionId, InstitutionStatus: resp.InstitutionStatus, KYBStatus: resp.KybStatus, APIAccessStatus: resp.ApiAccessStatus, Environment: resp.Environment, APIEnabled: resp.ApiEnabled, Entitlements: resp.Entitlements}, nil
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

func (c *grpcCustomerClient) CreateCredential(institutionID, label, environment string) (*CustomerCredential, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := c.client.CreateCredential(ctx, &customerv1.CreateCredentialRequest{
		InstitutionId: institutionID,
		Label:         label,
		Environment:   environment,
	})
	if err != nil {
		return nil, "", err
	}
	if resp == nil || resp.Credential == nil || resp.Secret == "" {
		return nil, "", errors.New("customer service returned an incomplete credential response")
	}
	credential, err := customerCredentialFromProto(resp.Credential)
	if err != nil {
		return nil, "", err
	}
	return credential, resp.Secret, nil
}

func (c *grpcCustomerClient) RotateCredential(institutionID, publicID string) (*CustomerCredential, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := c.client.RotateCredential(ctx, &customerv1.CredentialRequest{InstitutionId: institutionID, PublicId: publicID})
	if err != nil {
		return nil, "", err
	}
	if resp == nil || resp.Credential == nil || resp.Secret == "" {
		return nil, "", errors.New("customer service returned an incomplete credential rotation response")
	}
	credential, err := customerCredentialFromProto(resp.Credential)
	if err != nil {
		return nil, "", err
	}
	return credential, resp.Secret, nil
}

func (c *grpcCustomerClient) RevokeCredential(institutionID, publicID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	_, err := c.client.RevokeCredential(ctx, &customerv1.CredentialRequest{InstitutionId: institutionID, PublicId: publicID})
	return err
}

func (c *grpcCustomerClient) GetCredential(institutionID, publicID string) (*CustomerCredential, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := c.client.GetCredential(ctx, &customerv1.CredentialRequest{InstitutionId: institutionID, PublicId: publicID})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("customer service returned an empty credential response")
	}
	return customerCredentialFromProto(resp)
}

func (c *grpcCustomerClient) ListCredentials(institutionID string) ([]*CustomerCredential, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := c.client.ListCredentials(ctx, &customerv1.InstitutionRequest{InstitutionId: institutionID})
	if err != nil {
		return nil, err
	}
	result := make([]*CustomerCredential, 0, len(resp.GetCredentials()))
	for _, credential := range resp.GetCredentials() {
		converted, err := customerCredentialFromProto(credential)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func customerCredentialFromProto(credential *customerv1.Credential) (*CustomerCredential, error) {
	if credential == nil {
		return nil, errors.New("customer service returned an empty credential")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, credential.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("customer service returned an invalid credential creation time: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, credential.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("customer service returned an invalid credential update time: %w", err)
	}
	return &CustomerCredential{
		ID:            credential.CredentialId,
		PublicID:      credential.PublicId,
		InstitutionID: credential.InstitutionId,
		Label:         credential.Label,
		Environment:   credential.Environment,
		Status:        credential.Status,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	}, nil
}

func (c *grpcCustomerClient) RegisterBeneficiary(ctx context.Context, institutionID, name, bankCode, accountNumber, idempotencyKey string) (map[string]any, error) {
	writeToken := strings.TrimSpace(os.Getenv("OSAI_BENEFICIARY_WRITE_TOKEN"))
	if writeToken == "" {
		return nil, fmt.Errorf("beneficiary registration token is not configured")
	}
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-osai-beneficiary-write-token", writeToken)
	resp, err := c.client.RegisterBeneficiary(ctx, &customerv1.RegisterBeneficiaryRequest{
		InstitutionId:  institutionID,
		Name:           name,
		BankCode:       bankCode,
		AccountNumber:  accountNumber,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"beneficiary_id":  resp.BeneficiaryId,
		"institution_id":  resp.InstitutionId,
		"status":          resp.Status,
		"approval_status": resp.ApprovalStatus,
	}, nil
}

func (q *grpcQuoteClient) CreateQuote(req QuoteRequest) (QuoteResponse, error) {
	ctx := correlation.WithContext(context.Background(), req.CorrelationID)
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := q.client.CreateQuote(ctx, &quotev1.CreateQuoteRequest{InstitutionId: req.InstitutionID, IdempotencyKey: req.IdempotencyKey, CorrelationId: req.CorrelationID, BaseAmountMinor: req.BaseAmountMinor, BaseCurrency: req.BaseCurrency, QuoteCurrency: req.QuoteCurrency, DestinationRail: req.DestinationRail, Urgency: req.Urgency, CustomerReference: req.CustomerReference, QuoteMode: req.QuoteMode, DestinationAmountMinor: req.DestinationAmountMinor})
	if err != nil {
		return QuoteResponse{}, err
	}
	return quoteResponseFromProto(resp), nil
}

func (q *grpcQuoteClient) GetQuote(institutionID, quoteID string) (QuoteResponse, error) {
	resp, err := q.client.GetQuote(context.Background(), &quotev1.GetQuoteRequest{InstitutionId: institutionID, QuoteId: quoteID})
	if err != nil {
		return QuoteResponse{}, err
	}
	return quoteResponseFromProto(resp), nil
}

func (q *grpcQuoteClient) AcceptQuote(institutionID, quoteID, idempotencyKey, correlationID, beneficiaryID string) (QuoteResponse, error) {
	ctx := correlation.WithContext(context.Background(), correlationID)
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := q.client.AcceptQuote(ctx, &quotev1.AcceptQuoteRequest{InstitutionId: institutionID, QuoteId: quoteID, IdempotencyKey: idempotencyKey, CorrelationId: correlationID, BeneficiaryId: beneficiaryID})
	if err != nil {
		return QuoteResponse{}, err
	}
	response := quoteResponseFromProto(resp)
	response.TradeID = resp.TradeId
	return response, nil
}

func (q *grpcQuoteClient) GetProviderStatuses(ctx context.Context) ([]ProviderRouteStatus, error) {
	resp, err := q.client.ListProviderStatuses(ctx, &quotev1.ListProviderStatusesRequest{})
	if err != nil {
		return nil, err
	}
	items := make([]ProviderRouteStatus, 0, len(resp.GetItems()))
	for _, item := range resp.GetItems() {
		if item == nil {
			continue
		}
		items = append(items, ProviderRouteStatus{
			RouteID: item.GetRouteId(), Status: item.GetStatus(),
			Capabilities: append([]string(nil), item.GetCapabilities()...),
		})
	}
	return items, nil
}

func quoteResponseFromProto(resp *quotev1.QuoteResponse) QuoteResponse {
	if resp == nil {
		return QuoteResponse{}
	}
	return QuoteResponse{
		QuoteID: resp.QuoteId, TradeID: resp.TradeId, Status: resp.Status,
		BaseAmountMinor: resp.BaseAmountMinor, BaseCurrency: resp.BaseCurrency, QuoteCurrency: resp.QuoteCurrency,
		DestinationRail: resp.DestinationRail, AmountOutMinor: resp.AmountOutMinor, RateMinor: resp.RateMinor,
		FeeMinor: resp.FeeMinor, QuoteMode: resp.QuoteMode, SourcePrincipalMinor: resp.SourcePrincipalMinor,
		TotalDebitMinor: resp.TotalDebitMinor, DestinationTargetMinor: resp.DestinationTargetMinor,
		DestinationSurplusMinor: resp.DestinationSurplusMinor, RateDirection: resp.RateDirection,
		RateScale: resp.RateScale, FeeScheduleID: resp.FeeScheduleId, FeeScheduleVersion: resp.FeeScheduleVersion,
		SourceMinorExponent: resp.SourceMinorExponent, DestinationMinorExponent: resp.DestinationMinorExponent,
		ExpiresAt: parseExpiration(resp.ExpiresAt), CorrelationID: resp.CorrelationId,
	}
}

func (t *grpcTradeClient) GetTrade(institutionID, tradeID string) (TradeReadResponse, error) {
	resp, err := t.client.GetTrade(context.Background(), &tradev1.GetTradeRequest{InstitutionId: institutionID, TradeId: tradeID})
	if err != nil {
		return TradeReadResponse{}, err
	}
	return TradeReadResponse{TradeID: resp.TradeId, InstitutionID: resp.InstitutionId, QuoteID: resp.QuoteId, Status: resp.Status, BaseAmountMinor: resp.BaseAmountMinor, BaseCurrency: resp.BaseCurrency, QuoteCurrency: resp.QuoteCurrency, SettlementID: resp.SettlementId, CustomerReference: resp.CustomerReference, CorrelationID: resp.CorrelationId}, nil
}

func (t *grpcTradeClient) ListTrades(institutionID string, request TradeListRequest) (TradePage, error) {
	req := &tradev1.ListTradesRequest{
		InstitutionId:     institutionID,
		Page:              int32(request.Page),
		PageSize:          int32(request.PageSize),
		Currency:          request.Currency,
		Status:            request.Status,
		CustomerReference: request.CustomerReference,
		CorrelationId:     request.CorrelationID,
	}
	if !request.From.IsZero() {
		req.From = request.From.UTC().Format(time.RFC3339)
	}
	if !request.To.IsZero() {
		req.To = request.To.UTC().Format(time.RFC3339)
	}
	ctx := correlation.WithContext(context.Background(), request.CorrelationID)
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := t.client.ListTrades(ctx, req)
	if err != nil {
		return TradePage{}, err
	}
	items := make([]TradeReadResponse, 0, len(resp.Items))
	for _, item := range resp.Items {
		if item == nil {
			continue
		}
		items = append(items, TradeReadResponse{
			TradeID: item.TradeId, InstitutionID: item.InstitutionId, QuoteID: item.QuoteId,
			Status: item.Status, BaseAmountMinor: item.BaseAmountMinor, BaseCurrency: item.BaseCurrency,
			QuoteCurrency: item.QuoteCurrency, SettlementID: item.SettlementId,
			CustomerReference: item.CustomerReference, CorrelationID: item.CorrelationId,
		})
	}
	return TradePage{Items: items, Page: int(resp.Page), PageSize: int(resp.PageSize), Total: int(resp.Total)}, nil
}

func (s *grpcSettlementClient) GetSettlement(institutionID, settlementID string) (SettlementReadResponse, error) {
	resp, err := s.client.GetSettlement(context.Background(), &settlementv1.GetSettlementRequest{InstitutionId: institutionID, SettlementId: settlementID})
	if err != nil {
		return SettlementReadResponse{}, err
	}
	return SettlementReadResponse{SettlementID: resp.SettlementId, InstitutionID: resp.InstitutionId, TradeID: resp.TradeId, QuoteID: resp.QuoteId, Status: resp.Status, Beneficiary: resp.Beneficiary, AmountMinor: resp.AmountMinor, Currency: resp.Currency, CorrelationID: resp.CorrelationId}, nil
}

func (s *grpcSettlementClient) ListCustomerSettlementReport(institutionID string, request SettlementReportRequest) (SettlementReportPage, error) {
	req := &settlementv1.ListCustomerSettlementReportRequest{
		InstitutionId: institutionID, Page: int32(request.Page), PageSize: int32(request.PageSize),
		Currency: request.Currency, Status: request.Status,
	}
	if !request.From.IsZero() {
		req.From = request.From.UTC().Format(time.RFC3339)
	}
	if !request.To.IsZero() {
		req.To = request.To.UTC().Format(time.RFC3339)
	}
	ctx := correlation.WithContext(context.Background(), request.CorrelationID)
	ctx = observability.AttachOutgoingGRPCMetadata(ctx)
	resp, err := s.client.ListCustomerSettlementReport(ctx, req)
	if err != nil {
		return SettlementReportPage{}, err
	}
	items := make([]SettlementReportItem, 0, len(resp.Items))
	for _, item := range resp.Items {
		if item == nil {
			continue
		}
		occurredAt, err := time.Parse(time.RFC3339, item.OccurredAt)
		if err != nil {
			return SettlementReportPage{}, fmt.Errorf("settlement service returned invalid occurred_at")
		}
		items = append(items, SettlementReportItem{
			SettlementID: item.SettlementId, TradeID: item.TradeId, QuoteID: item.QuoteId,
			Status: item.Status, AmountMinor: item.AmountMinor, Currency: item.Currency, OccurredAt: occurredAt,
		})
	}
	return SettlementReportPage{Items: items, Page: int(resp.Page), PageSize: int(resp.PageSize), Total: int(resp.Total)}, nil
}

func (r *grpcReportingClient) GetBalances(institutionID string) (BalanceReadResponse, error) {
	resp, err := r.client.GetBalances(context.Background(), &reportingv1.GetBalancesRequest{InstitutionId: institutionID})
	if err != nil {
		return BalanceReadResponse{}, err
	}
	balances := make([]BalanceItem, 0, len(resp.Balances))
	for _, balance := range resp.Balances {
		if balance == nil {
			continue
		}
		balances = append(balances, BalanceItem{
			Currency:       balance.Currency,
			AvailableMinor: balance.AvailableMinor,
			HeldMinor:      balance.HeldMinor,
			TotalMinor:     balance.TotalMinor,
		})
	}
	return BalanceReadResponse{InstitutionID: institutionID, Balances: balances}, nil
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
