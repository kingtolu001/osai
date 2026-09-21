package observability

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/osai/osai/pkg/correlation"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	initOnce sync.Once
	initErr  error
)

func Init(serviceName string) error {
	if serviceName == "" {
		serviceName = os.Getenv("OSAI_SERVICE_NAME")
	}
	if serviceName == "" {
		serviceName = "osai"
	}
	initOnce.Do(func() {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		ctx := context.Background()
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithEndpoint("localhost:4317"),
		)
		if err != nil {
			initErr = err
			return
		}
		res, err := resource.New(ctx,
			resource.WithAttributes(
				semconv.ServiceName(serviceName),
			),
		)
		if err != nil {
			initErr = err
			return
		}
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(provider)
	})
	return initErr
}

func Start(ctx context.Context, component, operation string) (context.Context, oteltrace.Span) {
	return otel.Tracer(component).Start(ctx, operation)
}

func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, span := Start(ctx, "grpc/client", method)
		defer span.End()
		ctx = AttachOutgoingGRPCMetadata(ctx)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = ExtractGRPCMetadata(ctx, md)
		}
		ctx, span := Start(ctx, "grpc/server", info.FullMethod)
		defer span.End()
		return handler(ctx, req)
	}
}

func AttachOutgoingGRPCMetadata(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	if md == nil {
		md = metadata.New(nil)
	} else {
		md = md.Copy()
	}
	if corr := correlation.FromContext(ctx); corr != "" {
		md.Set(strings.ToLower(correlation.HeaderName), corr)
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	for k, v := range carrier {
		if v != "" {
			md.Set(strings.ToLower(k), v)
		}
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func ExtractGRPCMetadata(ctx context.Context, md metadata.MD) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	carrier := propagation.MapCarrier{}
	for key, values := range md {
		if len(values) > 0 {
			carrier[key] = values[0]
		}
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	if values := md.Get(strings.ToLower(correlation.HeaderName)); len(values) > 0 && values[0] != "" {
		ctx = correlation.WithContext(ctx, values[0])
	}
	return ctx
}

func InjectGRPCMetadata(ctx context.Context) metadata.MD {
	ctx = AttachOutgoingGRPCMetadata(ctx)
	md, _ := metadata.FromOutgoingContext(ctx)
	if md == nil {
		return metadata.MD{}
	}
	return md
}

func ExtractGRPCMetadataFromIncoming(ctx context.Context, md metadata.MD) context.Context {
	return ExtractGRPCMetadata(ctx, md)
}
