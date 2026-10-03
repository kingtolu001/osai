package main

import (
	"context"
	"errors"

	"github.com/osai/osai/pkg/provider"
)

type settlementRoute struct {
	Config provider.RouteConfig
	Rail   provider.SettlementRail
}

type settlementRouteSelector struct{ Routes map[string]settlementRoute }

func (s settlementRouteSelector) Check(ctx context.Context, providerID, currency, rail string) error {
	route, ok := s.Routes[providerID]
	if !ok || route.Rail == nil {
		return errors.New("settlement route unavailable")
	}
	if !route.Config.Enabled || route.Config.KillSwitch || !route.Config.Transfer {
		return errors.New("settlement route disabled")
	}
	if prober, ok := route.Rail.(interface{ ProbeTransferHealth(context.Context) error }); ok {
		if err := prober.ProbeTransferHealth(ctx); err != nil {
			return errors.New("provider health unavailable")
		}
	}
	return route.Config.Eligible(route.Rail.Health(), currency, rail)
}
