package integration

import (
	"context"
	"errors"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"sync"
	"testing"
)

type payoutMetricForwarder struct {
	metricspb.UnimplementedMetricsServiceServer
	client   metricspb.MetricsServiceClient
	mu       sync.Mutex
	verified bool
}

func (p *payoutMetricForwarder) Export(ctx context.Context, request *metricspb.ExportMetricsServiceRequest) (*metricspb.ExportMetricsServiceResponse, error) {
	response, err := p.client.Export(ctx, request)
	if err != nil {
		return nil, err
	}
	if response.GetPartialSuccess().GetRejectedDataPoints() != 0 {
		return nil, errors.New("collector rejected payout metric observations")
	}
	for _, resource := range request.ResourceMetrics {
		settlement := false
		for _, a := range resource.GetResource().GetAttributes() {
			if a.Key == "service.name" && a.Value.GetStringValue() == "settlement" {
				settlement = true
			}
		}
		if !settlement {
			continue
		}
		values := map[string]float64{}
		for _, scope := range resource.ScopeMetrics {
			for _, metric := range scope.Metrics {
				for _, point := range metric.GetGauge().GetDataPoints() {
					value := point.GetAsDouble()
					if value == 0 {
						value = float64(point.GetAsInt())
					}
					values[metric.Name] = value
				}
			}
		}
		if values["sandbox_payout_unknown"] >= 1 && values["sandbox_payout_unresolved"] >= 1 && values["sandbox_payout_oldest_unresolved_seconds"] >= 1 {
			p.mu.Lock()
			p.verified = true
			p.mu.Unlock()
		}
	}
	return response, nil
}

type payoutTraceForwarder struct {
	tracepb.UnimplementedTraceServiceServer
	client tracepb.TraceServiceClient
}

func (p *payoutTraceForwarder) Export(ctx context.Context, request *tracepb.ExportTraceServiceRequest) (*tracepb.ExportTraceServiceResponse, error) {
	return p.client.Export(ctx, request)
}

func startPayoutMetricForwarder(t *testing.T) (string, func() bool) {
	t.Helper()
	conn, err := grpc.NewClient("127.0.0.1:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	metrics := &payoutMetricForwarder{client: metricspb.NewMetricsServiceClient(conn)}
	server := grpc.NewServer()
	metricspb.RegisterMetricsServiceServer(server, metrics)
	tracepb.RegisterTraceServiceServer(server, &payoutTraceForwarder{client: tracepb.NewTraceServiceClient(conn)})
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close(); conn.Close() })
	return listener.Addr().String(), func() bool { metrics.mu.Lock(); defer metrics.mu.Unlock(); return metrics.verified }
}
