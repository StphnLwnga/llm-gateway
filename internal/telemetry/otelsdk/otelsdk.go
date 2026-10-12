// Package otelsdk builds the OpenTelemetry tracer and meter providers. It is
// separate from the logger, so binaries that only log do not link the
// exporters.
package otelsdk

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the service.name resource attribute when OTEL_SERVICE_NAME is not set.
const ServiceName = "llm-gateway"

// Providers holds the tracer and meter providers the gateway records with.
type Providers struct {
	Tracer   trace.TracerProvider
	Meter    metric.MeterProvider
	shutdown func(context.Context) error
}

// Shutdown exports what is buffered and stops both providers.
func (p *Providers) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx)
}

// New returns OTLP/HTTP providers when endpoint is set, and no-op
// providers otherwise. The exporters read the other OTEL_EXPORTER_OTLP_*
// settings, and the resource reads OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES, from the environment.
func New(ctx context.Context, endpoint, version string) (*Providers, error) {
	if endpoint == "" {
		return &Providers{
			Tracer:   tracenoop.NewTracerProvider(),
			Meter:    metricnoop.NewMeterProvider(),
			shutdown: func(context.Context) error { return nil },
		}, nil
	}
	res, err := newResource(ctx, version)
	if err != nil {
		return nil, err
	}
	spans, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	return newSDKProviders(res, spans, sdkmetric.NewPeriodicReader(metrics)), nil
}

// newResource lets OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the defaults.
func newResource(ctx context.Context, version string) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", ServiceName),
			attribute.String("service.version", version),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}
	return res, nil
}

// Sampler records every span, whatever an incoming traceparent's sampled flag
// says: the collector's tail sampling makes every keep or drop decision, so a
// client cannot switch off recording (ADR 0016).
func Sampler() sdktrace.Sampler {
	return sdktrace.AlwaysSample()
}

func newSDKProviders(res *resource.Resource, spans sdktrace.SpanExporter, reader sdkmetric.Reader) *Providers {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(Sampler()),
		sdktrace.WithBatcher(spans),
	)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader))
	return &Providers{
		Tracer: tp,
		Meter:  mp,
		shutdown: func(ctx context.Context) error {
			return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
		},
	}
}
