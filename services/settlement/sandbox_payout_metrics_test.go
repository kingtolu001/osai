package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/osai/osai/pkg/provider"
	"github.com/osai/osai/pkg/sandboxpolicy"
	"github.com/osai/osai/services/settlement/settlementcore"
	"github.com/osai/osai/services/settlement/settlementstore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSandboxPayoutMetricsAndDurableUnknownAlertsDoNotChangeFinality(t *testing.T) {
	dsn := os.Getenv("OSAI_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("local proof PostgreSQL required")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "payout_alert_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = settlementstore.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	p := settlementstore.Postgres{DB: db}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		instruction, err := p.CreateForTrade(ctx, "sbx:alerts", "trd_"+uuid.NewString(), "sbx:quote", "sbx:correlation", "sbx:beneficiary", "USD", 95, "fixture alerts", "sbx:provider", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if err = p.RecordRuntimeState(ctx, instruction.ID, settlementcore.Unknown, "sbx:provider-ref", provider.Evidence{}); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`UPDATE settlement_instructions SET created_at=now()-interval '20 minutes' WHERE id=$1`, instruction.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 3 {
		if err = p.EscalateSandboxUnknownPayouts(ctx, 10*time.Minute, 1); err != nil {
			t.Fatal(err)
		}
	}
	var alerts, unknown int
	if err = db.QueryRow(`SELECT count(*) FROM settlement_domain_outbox WHERE event_type='si.unknown_escalated'`).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM settlement_instructions WHERE state='UNKNOWN'`).Scan(&unknown); err != nil {
		t.Fatal(err)
	}
	if alerts != 2 || unknown != 2 {
		t.Fatalf("alert duplication/starvation or financial state mutation: alerts=%d UNKNOWN=%d", alerts, unknown)
	}
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(previous)
	defer provider.Shutdown(ctx)
	registration, err := registerSandboxPayoutMetrics(p)
	if err != nil {
		t.Fatal(err)
	}
	defer registration.Unregister()
	var result metricdata.ResourceMetrics
	if err = reader.Collect(ctx, &result); err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, scope := range result.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Gauge[int64]:
				if len(data.DataPoints) == 1 {
					values[m.Name] = float64(data.DataPoints[0].Value)
				}
			case metricdata.Gauge[float64]:
				if len(data.DataPoints) == 1 {
					values[m.Name] = data.DataPoints[0].Value
				}
			}
		}
	}
	if values["sandbox_payout_unknown"] != 2 || values["sandbox_payout_unresolved"] != 3 || values["sandbox_payout_oldest_unresolved_seconds"] < 1200 {
		t.Fatalf("runtime metric values: %+v", values)
	}
	if os.Getenv("OSAI_SANDBOX_PAYOUT_METRICS_EXPORT_VERIFY") == "true" {
		if !sandboxpolicy.LocalProofDSN(dsn) {
			t.Fatal("live metrics proof requires a dedicated loopback database")
		}
		exportCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		exporter, err := otlpmetricgrpc.New(exportCtx, otlpmetricgrpc.WithEndpoint("127.0.0.1:4317"), otlpmetricgrpc.WithInsecure())
		if err != nil {
			t.Fatal(err)
		}
		defer exporter.Shutdown(ctx)
		if err = exporter.Export(exportCtx, &result); err != nil {
			t.Fatal("live collector rejected database-backed payout metrics", err)
		}
		t.Log("exported actual PostgreSQL-backed payout gauges over OTLP gRPC to the local collector: UNKNOWN=2, unresolved=3, oldest unresolved age>=1200 seconds; direct SDK export, not subprocess periodic scheduling")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = reader.Collect(ctx, &result); err == nil {
		t.Fatal("unavailable database emitted a false successful metrics observation")
	}
}

func TestSandboxPayoutAlertThresholdIsBounded(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "25h", "invalid"} {
		t.Setenv("OSAI_SANDBOX_PAYOUT_UNKNOWN_ALERT_AFTER", value)
		if _, err := sandboxPayoutAlertAge(); err == nil {
			t.Fatalf("unsafe alert threshold accepted: %s", value)
		}
	}
	t.Setenv("OSAI_SANDBOX_PAYOUT_UNKNOWN_ALERT_AFTER", "1s")
	if age, err := sandboxPayoutAlertAge(); err != nil || age != time.Second {
		t.Fatalf("explicit proof alert threshold rejected: %v", err)
	}
}
