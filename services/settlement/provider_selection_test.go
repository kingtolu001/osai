package main

import (
	"context"
	"errors"
	"testing"

	"github.com/osai/osai/pkg/provider"
)

type eligibilityRail struct {
	provider.SettlementRail
	health   provider.Health
	probeErr error
}

func (r *eligibilityRail) Health() provider.Health                   { return r.health }
func (r *eligibilityRail) ProbeTransferHealth(context.Context) error { return r.probeErr }

func TestRouteEligibilityExcludesDegradedAndKilledProvider(t *testing.T) {
	rail := &eligibilityRail{health: provider.Health{Up: true, Capabilities: map[string]bool{"transfer": true}}}
	config := provider.RouteConfig{Enabled: true, Environment: "test", Currencies: []string{"NGN"}, Rails: []string{"bank_transfer"}, Transfer: true}
	selector := settlementRouteSelector{Routes: map[string]settlementRoute{"flutterwave": {Config: config, Rail: rail}}}
	check := func() error { return selector.Check(context.Background(), "flutterwave", "NGN", "bank_transfer") }
	if err := check(); err != nil {
		t.Fatalf("healthy route excluded: %v", err)
	}
	rail.health.Up = false
	if err := check(); err == nil {
		t.Fatal("degraded route selected")
	}
	rail.health.Up = true
	rail.probeErr = errors.New("provider unavailable")
	if err := check(); err == nil {
		t.Fatal("failed live probe selected")
	}
	rail.probeErr = nil
	config.KillSwitch = true
	selector.Routes["flutterwave"] = settlementRoute{Config: config, Rail: rail}
	if err := check(); err == nil {
		t.Fatal("kill switch selected route")
	}
	config.KillSwitch = false
	config.Enabled = false
	selector.Routes["flutterwave"] = settlementRoute{Config: config, Rail: rail}
	if err := check(); err == nil {
		t.Fatal("disabled route selected")
	}
}
