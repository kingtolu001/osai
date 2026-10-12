package main

import (
	"context"
	"net/http"
)

type ProviderRouteStatus struct {
	RouteID      string   `json:"route_id"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

type ProviderStatusClient interface {
	GetProviderStatuses(context.Context) ([]ProviderRouteStatus, error)
}

func (g *Gateway) handleProviderStatus(w http.ResponseWriter, r *http.Request, requestID string) {
	if g.providerStatusClient == nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "provider status service unavailable", requestID))
		return
	}
	items, err := g.providerStatusClient.GetProviderStatuses(r.Context())
	if err != nil {
		writeHTTPJSON(w, http.StatusServiceUnavailable, errorEnvelope("SERVICE_UNAVAILABLE", "provider status service unavailable", requestID))
		return
	}
	writeHTTPJSON(w, http.StatusOK, map[string]any{"items": items, "request_id": requestID})
}
