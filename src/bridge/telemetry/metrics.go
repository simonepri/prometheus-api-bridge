// Package telemetry exports bounded-cardinality bridge metrics over OTLP.
package telemetry

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

const instrumentationName = "github.com/simonepri/prometheus-api-bridge"

// Config controls native OTLP metrics. Exporter endpoint, headers, compression,
// and TLS use the standard OTEL_EXPORTER_OTLP_* environment variables.
type Config struct {
	Enabled        bool
	Backend        string
	ExportInterval time.Duration
}

// Runtime records bridge metrics and owns their exporter lifecycle.
type Runtime struct {
	provider       *sdkmetric.MeterProvider
	backend        attribute.KeyValue
	httpRequests   metric.Int64Counter
	httpDuration   metric.Float64Histogram
	backendQueries metric.Int64Counter
	backendLatency metric.Float64Histogram
	backendErrors  metric.Int64Counter
	activeQueries  metric.Int64UpDownCounter
}

// New builds a disabled no-op runtime or an OTLP/HTTP metrics pipeline.
func New(ctx context.Context, config Config) (*Runtime, error) {
	if !config.Enabled {
		return &Runtime{}, nil
	}
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	interval := config.ExportInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	bridgeResource, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", "prometheus-api-bridge")),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(bridgeResource),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval))),
	)
	runtime, err := newRuntime(provider, config.Backend)
	if err != nil {
		_ = provider.Shutdown(ctx)
		return nil, err
	}
	runtime.provider = provider
	return runtime, nil
}

func newRuntime(provider metric.MeterProvider, backend string) (*Runtime, error) {
	meter := provider.Meter(instrumentationName)
	httpRequests, err := meter.Int64Counter("prometheus_api_bridge_http_server_requests")
	if err != nil {
		return nil, err
	}
	httpDuration, err := meter.Float64Histogram(
		"prometheus_api_bridge_http_server_duration_seconds",
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	backendQueries, err := meter.Int64Counter("prometheus_api_bridge_backend_queries")
	if err != nil {
		return nil, err
	}
	backendLatency, err := meter.Float64Histogram(
		"prometheus_api_bridge_backend_query_duration_seconds",
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	backendErrors, err := meter.Int64Counter("prometheus_api_bridge_backend_query_errors")
	if err != nil {
		return nil, err
	}
	activeQueries, err := meter.Int64UpDownCounter("prometheus_api_bridge_backend_queries_active")
	if err != nil {
		return nil, err
	}
	return &Runtime{
		backend:        attribute.String("bridge.backend", backend),
		httpRequests:   httpRequests,
		httpDuration:   httpDuration,
		backendQueries: backendQueries,
		backendLatency: backendLatency,
		backendErrors:  backendErrors,
		activeQueries:  activeQueries,
	}, nil
}

// HTTPRequest records one completed Prometheus API request.
func (r *Runtime) HTTPRequest(
	ctx context.Context,
	method string,
	route string,
	status int,
	duration time.Duration,
) {
	if r == nil || r.httpRequests == nil {
		return
	}
	attributes := metric.WithAttributes(
		r.backend,
		attribute.String("http.request.method", normalizedHTTPMethod(method)),
		attribute.String("http.route", route),
		attribute.Int("http.response.status_code", status),
	)
	r.httpRequests.Add(ctx, 1, attributes)
	r.httpDuration.Record(ctx, duration.Seconds(), attributes)
}

func normalizedHTTPMethod(method string) string {
	switch method {
	case http.MethodConnect,
		http.MethodDelete,
		http.MethodGet,
		http.MethodHead,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
		http.MethodTrace:
		return method
	default:
		return "_OTHER"
	}
}

// BackendQuery records one completed backend operation.
func (r *Runtime) BackendQuery(ctx context.Context, operation string, duration time.Duration, failed bool) {
	if r == nil || r.backendQueries == nil {
		return
	}
	attributes := metric.WithAttributes(r.backend, attribute.String("bridge.operation", operation))
	r.backendQueries.Add(ctx, 1, attributes)
	r.backendLatency.Record(ctx, duration.Seconds(), attributes)
	if failed {
		r.backendErrors.Add(ctx, 1, attributes)
	}
}

// ActiveQueries adjusts the number of backend queries currently in flight.
func (r *Runtime) ActiveQueries(ctx context.Context, delta int64) {
	if r == nil || r.activeQueries == nil {
		return
	}
	r.activeQueries.Add(ctx, delta, metric.WithAttributes(r.backend))
}

// Shutdown flushes pending metrics within the caller's deadline.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}

var _ interface {
	HTTPRequest(context.Context, string, string, int, time.Duration)
	BackendQuery(context.Context, string, time.Duration, bool)
	ActiveQueries(context.Context, int64)
} = (*Runtime)(nil)
