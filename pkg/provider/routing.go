package provider

import (
	"errors"
	"time"
)

// RouteConfig is the provider-neutral admission policy for new money movement.
// It does not affect status recovery for instructions already submitted.
type RouteConfig struct {
	Enabled           bool
	Environment       string
	Currencies        []string
	Rails             []string
	Timeout           time.Duration
	KillSwitch        bool
	Transfer          bool
	Reconciliation    bool
	AccountResolution bool
}

func (c RouteConfig) Eligible(health Health, currency, rail string) error {
	if !c.Enabled || c.KillSwitch || !c.Transfer || !health.Up || !health.Capabilities["transfer"] {
		return errors.New("provider transfer route unavailable")
	}
	currencyAllowed := false
	for _, item := range c.Currencies {
		if item == currency {
			currencyAllowed = true
			break
		}
	}
	railAllowed := false
	for _, item := range c.Rails {
		if item == rail {
			railAllowed = true
			break
		}
	}
	if !currencyAllowed || !railAllowed {
		return errors.New("provider capability does not match settlement")
	}
	return nil
}
