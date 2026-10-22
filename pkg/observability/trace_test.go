package observability

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/osai/osai/pkg/correlation"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestInitTracerProvider(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	receiver := &testMetricsReceiver{requests: make(chan *metricspb.ExportMetricsServiceRequest, 1)}
	server := grpc.NewServer()
	metricspb.RegisterMetricsServiceServer(server, receiver)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())

	if err := Init("test-service"); err != nil {
		t.Fatalf("expected tracer provider to initialize: %v", err)
	}
	counter, err := Meter("observability-test").Int64Counter("phase5_export_test_total")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := meterProvider.ForceFlush(ctx); err != nil {
		t.Fatalf("flush OTLP metric: %v", err)
	}
	select {
	case request := <-receiver.requests:
		for _, resource := range request.GetResourceMetrics() {
			for _, scope := range resource.GetScopeMetrics() {
				for _, metric := range scope.GetMetrics() {
					if metric.GetName() == "phase5_export_test_total" {
						return
					}
				}
			}
		}
		t.Fatalf("OTLP receiver did not receive the test metric: %+v", request)
	case <-ctx.Done():
		t.Fatal("timed out waiting for OTLP metric export")
	}
}

type testMetricsReceiver struct {
	metricspb.UnimplementedMetricsServiceServer
	requests chan *metricspb.ExportMetricsServiceRequest
}

func (receiver *testMetricsReceiver) Export(_ context.Context, request *metricspb.ExportMetricsServiceRequest) (*metricspb.ExportMetricsServiceResponse, error) {
	select {
	case receiver.requests <- request:
	default:
	}
	return &metricspb.ExportMetricsServiceResponse{}, nil
}

func TestGRPCMetadataRoundTrip(t *testing.T) {
	ctx := correlation.WithContext(context.Background(), "req-otel-phase5-final")
	ctx, span := otel.Tracer("test").Start(ctx, "root")
	defer span.End()

	md := InjectGRPCMetadata(ctx)
	if md.Get("x-osai-correlation-id")[0] != "req-otel-phase5-final" {
		t.Fatalf("correlation header missing from grpc metadata: %#v", md)
	}
	if got := md.Get("traceparent"); len(got) == 0 || got[0] == "" {
		t.Fatalf("traceparent missing from grpc metadata: %#v", md)
	}

	extracted := ExtractGRPCMetadata(context.Background(), metadata.New(map[string]string{
		"x-osai-correlation-id": "req-otel-phase5-final",
		"traceparent":           md.Get("traceparent")[0],
	}))
	if got := correlation.FromContext(extracted); got != "req-otel-phase5-final" {
		t.Fatalf("expected correlation id to survive extraction, got %q", got)
	}
	if traceID := oteltrace.SpanContextFromContext(extracted).TraceID().String(); traceID == "00000000000000000000000000000000" {
		t.Fatalf("expected extracted context to contain a non-zero trace id: %#v", extracted)
	}
}
