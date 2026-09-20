package webhookcore

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/workflows"
	"go.temporal.io/sdk/client"
)

type Signaler interface {
	SignalSettlement(ctx context.Context, clientRef string, event provider.WebhookEvent) error
}

type TemporalSignaler struct{ Client client.Client }

func (s TemporalSignaler) SignalSettlement(ctx context.Context, clientRef string, event provider.WebhookEvent) error {
	return s.Client.SignalWorkflow(ctx, "settlement-"+clientRef, "", workflows.CallbackSignal, event)
}

type Handler struct {
	providers map[string]provider.SettlementRail
	signaler  Signaler
	mu        sync.Mutex
	accepted  map[string]struct{}
}

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
		writeError(response, http.StatusUnauthorized, "webhook verification failed")
		return
	}
	if event.ProviderEventID == "" || event.ClientRef == "" {
		writeError(response, http.StatusBadRequest, "webhook event is incomplete")
		return
	}
	h.mu.Lock()
	_, duplicate := h.accepted[event.ProviderEventID]
	h.mu.Unlock()
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
	h.mu.Lock()
	h.accepted[event.ProviderEventID] = struct{}{}
	h.mu.Unlock()
	response.WriteHeader(http.StatusAccepted)
}

func writeError(response http.ResponseWriter, code int, message string) {
	response.Header().Set("Content-Type", "text/plain")
	response.WriteHeader(code)
	_, _ = response.Write([]byte(message))
}
