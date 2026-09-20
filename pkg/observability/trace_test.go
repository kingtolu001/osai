package observability

import (
	"context"
	"testing"

	"github.com/osai/osai/pkg/correlation"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

func TestInitTracerProvider(t *testing.T) {
	if err := Init("test-service"); err != nil {
		t.Fatalf("expected tracer provider to initialize: %v", err)
	}
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
		"traceparent": md.Get("traceparent")[0],
	}))
	if got := correlation.FromContext(extracted); got != "req-otel-phase5-final" {
		t.Fatalf("expected correlation id to survive extraction, got %q", got)
	}
	if traceID := oteltrace.SpanContextFromContext(extracted).TraceID().String(); traceID == "00000000000000000000000000000000" {
		t.Fatalf("expected extracted context to contain a non-zero trace id: %#v", extracted)
	}
}
