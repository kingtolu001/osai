package observability

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osai/osai/pkg/correlation"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	initOnce       sync.Once
	initErr        error
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
)

func Init(serviceName string) error {
	if serviceName == "" {
		serviceName = os.Getenv("OSAI_SERVICE_NAME")
	}
	if serviceName == "" {
		serviceName = "osai"
	}
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	initOnce.Do(func() {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		ctx := context.Background()
		traceOptions := make([]otlptracegrpc.Option, 0, 2)
		metricOptions := make([]otlpmetricgrpc.Option, 0, 2)
		if endpoint == "" {
			traceOptions = append(traceOptions, otlptracegrpc.WithEndpoint("localhost:4317"), otlptracegrpc.WithInsecure())
			metricOptions = append(metricOptions, otlpmetricgrpc.WithEndpoint("localhost:4317"), otlpmetricgrpc.WithInsecure())
		} else if strings.HasPrefix(strings.ToLower(endpoint), "http://") || strings.HasPrefix(strings.ToLower(endpoint), "unix://") {
			traceOptions = append(traceOptions, otlptracegrpc.WithInsecure())
			metricOptions = append(metricOptions, otlpmetricgrpc.WithInsecure())
		}
		exporter, err := otlptracegrpc.New(ctx, traceOptions...)
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
			_ = exporter.Shutdown(ctx)
			initErr = err
			return
		}
		metricExporter, err := otlpmetricgrpc.New(ctx, metricOptions...)
		if err != nil {
			_ = exporter.Shutdown(ctx)
			initErr = err
			return
		}
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)
		metrics := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(10*time.Second))),
			sdkmetric.WithResource(res),
		)
		tracerProvider = provider
		meterProvider = metrics
		otel.SetTracerProvider(provider)
		otel.SetMeterProvider(metrics)
	})
	return initErr
}

func Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var errs []error
	if tracerProvider != nil {
		errs = append(errs, tracerProvider.Shutdown(ctx))
	}
	if meterProvider != nil {
		errs = append(errs, meterProvider.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func Meter(name string) otelmetric.Meter { return otel.Meter(name) }

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
