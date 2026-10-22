package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	notificationv1 "github.com/osai/osai/proto/osai/notification/v1"
	"github.com/osai/osai/services/notification-webhook/webhookcore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type notificationGRPCServer struct {
	notificationv1.UnimplementedNotificationServiceServer
	store          *webhookcore.Postgres
	publishToken   string
	configureToken string
}

func (s *notificationGRPCServer) PublishEvent(ctx context.Context, event *notificationv1.Event) (*notificationv1.Receipt, error) {
	if s == nil || !s.authorized(ctx, s.publishToken) {
		return nil, status.Error(codes.PermissionDenied, "notification publish access denied")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "notification persistence unavailable")
	}
	if err := s.store.Publish(ctx, event); err != nil {
		if strings.Contains(err.Error(), "conflict") {
			return nil, status.Error(codes.AlreadyExists, "event id payload conflict")
		}
		if strings.Contains(err.Error(), "incomplete") || strings.Contains(err.Error(), "invalid") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Unavailable, "notification event could not be persisted")
	}
	return &notificationv1.Receipt{EventId: event.EventId}, nil
}

func (s *notificationGRPCServer) ConfigureWebhook(ctx context.Context, req *notificationv1.ConfigureWebhookRequest) (*notificationv1.ConfigureWebhookResponse, error) {
	if s == nil || !s.authorized(ctx, s.configureToken) {
		return nil, status.Error(codes.PermissionDenied, "webhook configuration access denied")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "notification persistence unavailable")
	}
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.EndpointUrl) == "" || strings.TrimSpace(req.SigningSecret) == "" || len(req.SubscribedEventTypes) == 0 {
		return nil, status.Error(codes.InvalidArgument, "complete webhook configuration required")
	}
	if err := s.store.Configure(req.InstitutionId, req.EndpointUrl, req.SigningSecret, req.SubscribedEventTypes); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, status.Error(codes.Canceled, "webhook configuration cancelled")
		}
		if strings.Contains(err.Error(), "configuration incomplete") || strings.Contains(err.Error(), "webhook endpoint") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Unavailable, "webhook configuration could not be persisted")
	}
	return &notificationv1.ConfigureWebhookResponse{Configured: true}, nil
}

func (s *notificationGRPCServer) ReplayWebhook(ctx context.Context, req *notificationv1.ReplayWebhookRequest) (*notificationv1.ReplayWebhookResponse, error) {
	if s == nil || !s.authorized(ctx, s.configureToken) {
		return nil, status.Error(codes.PermissionDenied, "webhook replay access denied")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "notification persistence unavailable")
	}
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.EventId) == "" || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution, event, and idempotency key required")
	}
	if err := s.store.Replay(ctx, req.InstitutionId, req.EventId, req.IdempotencyKey); err != nil {
		switch {
		case errors.Is(err, webhookcore.ErrReplayEventNotFound):
			return nil, status.Error(codes.NotFound, "webhook event not found")
		case errors.Is(err, webhookcore.ErrReplayNotAllowed):
			return nil, status.Error(codes.FailedPrecondition, "webhook event cannot be replayed")
		case errors.Is(err, webhookcore.ErrReplayKeyConflict):
			return nil, status.Error(codes.AlreadyExists, "webhook replay idempotency key conflict")
		default:
			return nil, status.Error(codes.Unavailable, "webhook replay could not be persisted")
		}
	}
	return &notificationv1.ReplayWebhookResponse{Queued: true}, nil
}

func (s *notificationGRPCServer) GetWebhookDeliveryHistory(ctx context.Context, req *notificationv1.GetWebhookDeliveryHistoryRequest) (*notificationv1.GetWebhookDeliveryHistoryResponse, error) {
	if s == nil || !s.authorized(ctx, s.configureToken) {
		return nil, status.Error(codes.PermissionDenied, "webhook delivery history access denied")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "notification persistence unavailable")
	}
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" || strings.TrimSpace(req.EventId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution and event are required")
	}
	history, err := s.store.DeliveryHistory(ctx, req.InstitutionId, req.EventId, int(req.Limit), int(req.Offset))
	if err != nil {
		switch {
		case errors.Is(err, webhookcore.ErrDeliveryHistoryNotFound):
			return nil, status.Error(codes.NotFound, "webhook delivery history not found")
		case errors.Is(err, webhookcore.ErrInvalidDeliveryHistoryRequest):
			return nil, status.Error(codes.InvalidArgument, err.Error())
		default:
			return nil, status.Error(codes.Unavailable, "webhook delivery history unavailable")
		}
	}
	response := &notificationv1.GetWebhookDeliveryHistoryResponse{
		EventId: history.EventID, EventType: history.EventType, Status: history.Status,
		AttemptCount: int32(history.AttemptCount), ReplayCount: int32(history.ReplayCount),
		NextAttemptAt: history.NextAttemptAt.UTC().Format(time.RFC3339Nano),
		Attempts:      make([]*notificationv1.WebhookDeliveryAttempt, 0, len(history.Attempts)),
	}
	for _, attempt := range history.Attempts {
		item := &notificationv1.WebhookDeliveryAttempt{
			AttemptNumber: int32(attempt.AttemptNumber), AttemptedAt: attempt.AttemptedAt.UTC().Format(time.RFC3339Nano),
			HttpStatus: int32(attempt.HTTPStatus), Outcome: attempt.Outcome,
		}
		if attempt.CompletedAt != nil {
			item.CompletedAt = attempt.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
		response.Attempts = append(response.Attempts, item)
	}
	return response, nil
}

func (s *notificationGRPCServer) ListWebhookDeliveryEvents(ctx context.Context, req *notificationv1.ListWebhookDeliveryEventsRequest) (*notificationv1.ListWebhookDeliveryEventsResponse, error) {
	if s == nil || !s.authorized(ctx, s.configureToken) {
		return nil, status.Error(codes.PermissionDenied, "webhook delivery operations access denied")
	}
	if s.store == nil {
		return nil, status.Error(codes.Unavailable, "notification persistence unavailable")
	}
	if req == nil || strings.TrimSpace(req.InstitutionId) == "" {
		return nil, status.Error(codes.InvalidArgument, "institution is required")
	}
	events, total, err := s.store.ListDeliveryEvents(ctx, req.InstitutionId, req.Status, int(req.Limit), int(req.Offset))
	if err != nil {
		if errors.Is(err, webhookcore.ErrInvalidDeliveryHistoryRequest) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Unavailable, "webhook delivery operations unavailable")
	}
	response := &notificationv1.ListWebhookDeliveryEventsResponse{Total: int32(total), Events: make([]*notificationv1.WebhookDeliveryEventSummary, 0, len(events))}
	for _, event := range events {
		response.Events = append(response.Events, &notificationv1.WebhookDeliveryEventSummary{
			EventId: event.EventID, EventType: event.EventType, Status: event.Status,
			AttemptCount: int32(event.AttemptCount), ReplayCount: int32(event.ReplayCount),
			NextAttemptAt: event.NextAttemptAt.UTC().Format(time.RFC3339Nano),
			OccurredAt:    event.OccurredAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return response, nil
}

func (s *notificationGRPCServer) authorized(ctx context.Context, expected string) bool {
	if expected == "" {
		return false
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get("authorization")) != 1 {
		return false
	}
	provided := strings.TrimPrefix(md.Get("authorization")[0], "Bearer ")
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
