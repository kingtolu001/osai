package webhookcore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/workflows"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

type Signaler interface {
	SignalSettlement(ctx context.Context, clientRef string, event provider.WebhookEvent) error
}

type TemporalSignaler struct{ Client client.Client }

func (s TemporalSignaler) SignalSettlement(ctx context.Context, clientRef string, event provider.WebhookEvent) error {
	id := "settlement-" + clientRef
	err := s.Client.SignalWorkflow(ctx, id, "", workflows.CallbackSignal, event)
	if err == nil {
		return nil
	}
	// A late callback is still acknowledged when the matching workflow has
	// already completed successfully after authoritative polling.
	description, describeErr := s.Client.DescribeWorkflowExecution(ctx, id, "")
	if describeErr == nil && description.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
		return nil
	}
	return err
}

type Handler struct {
	providers map[string]provider.SettlementRail
	signaler  Signaler
	inbox     ProviderInbox
	mu        sync.Mutex
	accepted  map[string]struct{}
}

func (h *Handler) SetProviderInbox(inbox ProviderInbox) { h.inbox = inbox }

func NewHandler(providers map[string]provider.SettlementRail, signaler Signaler) *Handler {
	return &Handler{providers: providers, signaler: signaler, accepted: make(map[string]struct{})}
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	ctx, span := observability.Start(request.Context(), "osai/notification-webhook", "provider.webhook")
	defer span.End()
	request = request.WithContext(ctx)
	if request.Method != http.MethodPost {
		writeError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "webhooks" || parts[2] != "providers" || parts[3] == "" {
		writeError(response, http.StatusNotFound, "webhook route not found")
		return
	}
	adapter, ok := h.providers[parts[3]]
	if !ok {
		writeError(response, http.StatusNotFound, "unknown provider")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
	if err != nil || len(raw) == 0 {
		writeError(response, http.StatusBadRequest, "malformed webhook body")
		return
	}
	headers := make(map[string]string, len(request.Header))
	for key, values := range request.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	event, err := adapter.VerifyWebhook(raw, headers)
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrProviderUnavailable):
			writeError(response, http.StatusServiceUnavailable, "provider webhook unavailable")
		case errors.Is(err, provider.ErrMalformedWebhook):
			writeError(response, http.StatusBadRequest, "malformed webhook body")
		case errors.Is(err, provider.ErrWebhookIgnored):
			response.WriteHeader(http.StatusOK)
		default:
			writeError(response, http.StatusUnauthorized, "webhook verification failed")
		}
		return
	}
	if event.ProviderEventID == "" || event.ClientRef == "" {
		writeError(response, http.StatusBadRequest, "webhook event is incomplete")
		return
	}
	if h.inbox != nil {
		if h.signaler == nil {
			writeError(response, http.StatusServiceUnavailable, "workflow signaler unavailable")
			return
		}
		duplicate, err := h.inbox.Accept(request.Context(), parts[3], event, func() error {
			return h.signaler.SignalSettlement(request.Context(), event.ClientRef, event)
		})
		if err != nil {
			writeError(response, http.StatusServiceUnavailable, "webhook could not be accepted")
			return
		}
		if duplicate {
			response.WriteHeader(http.StatusOK)
			return
		}
		if acknowledger, ok := adapter.(interface{ WebhookAcceptedStatus() int }); ok {
			response.WriteHeader(acknowledger.WebhookAcceptedStatus())
			return
		}
		response.WriteHeader(http.StatusAccepted)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, duplicate := h.accepted[event.ProviderEventID]
	if duplicate {
		response.WriteHeader(http.StatusOK)
		return
	}
	if h.signaler == nil {
		writeError(response, http.StatusServiceUnavailable, "workflow signaler unavailable")
		return
	}
	if err := h.signaler.SignalSettlement(request.Context(), event.ClientRef, event); err != nil {
		writeError(response, http.StatusServiceUnavailable, "webhook could not be accepted")
		return
	}
	h.accepted[event.ProviderEventID] = struct{}{}
	if acknowledger, ok := adapter.(interface{ WebhookAcceptedStatus() int }); ok {
		response.WriteHeader(acknowledger.WebhookAcceptedStatus())
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func writeError(response http.ResponseWriter, code int, message string) {
	response.Header().Set("Content-Type", "text/plain")
	response.WriteHeader(code)
	_, _ = response.Write([]byte(message))
}
