package main
import (
  "context"
  "fmt"
  "time"
  "go.opentelemetry.io/otel"
  "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
  "go.opentelemetry.io/otel/sdk/resource"
  sdktrace "go.opentelemetry.io/otel/sdk/trace"
  semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)
func main(){
  ctx:=context.Background()
  exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure(), otlptracegrpc.WithEndpoint("localhost:4317"))
  if err != nil { panic(err) }
  res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName("otel-probe")))
  if err != nil { panic(err) }
  tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.AlwaysSample()))
  otel.SetTracerProvider(tp)
  ctx, span := otel.Tracer("probe").Start(ctx, "probe-span")
  defer span.End()
  time.Sleep(200 * time.Millisecond)
  fmt.Println("probe-span-sent")
  _ = tp.Shutdown(ctx)
}
