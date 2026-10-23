package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/osai/osai/pkg/observability"
	"github.com/osai/osai/services/settlement/settlementstore"
	"go.opentelemetry.io/otel/metric"
)

func sandboxPayoutAlertAge() (time.Duration, error) {
	// This local operational threshold never changes financial finality.
	age := 15 * time.Minute
	if selected := os.Getenv("OSAI_SANDBOX_PAYOUT_UNKNOWN_ALERT_AFTER"); selected != "" {
		parsed, err := time.ParseDuration(selected)
		if err != nil || parsed < time.Second || parsed > 24*time.Hour {
			return 0, errors.New("invalid sandbox payout UNKNOWN alert duration")
		}
		age = parsed
	}
	return age, nil
}

func registerSandboxPayoutMetrics(repository settlementstore.Postgres) (metric.Registration, error) {
	meter := observability.Meter("osai/settlement")
	unresolved, err := meter.Int64ObservableGauge("sandbox_payout_unresolved", metric.WithDescription("Unresolved local fixture payout instructions"))
	if err != nil {
		return nil, err
	}
	unknown, err := meter.Int64ObservableGauge("sandbox_payout_unknown", metric.WithDescription("Local fixture payouts with an UNKNOWN outcome"))
	if err != nil {
		return nil, err
	}
	oldest, err := meter.Float64ObservableGauge("sandbox_payout_oldest_unresolved_seconds", metric.WithUnit("s"), metric.WithDescription("Age of the oldest unresolved local fixture payout"))
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		query, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		m, err := repository.SandboxPayoutMetrics(query)
		if err != nil {
			return err
		}
		observer.ObserveInt64(unresolved, m.Unresolved)
		observer.ObserveInt64(unknown, m.Unknown)
		observer.ObserveFloat64(oldest, m.OldestUnresolvedSeconds)
		return nil
	}, unresolved, unknown, oldest)
}
