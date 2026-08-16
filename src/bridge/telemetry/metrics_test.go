package telemetry

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRuntimeRecordsBridgeMetrics(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	runtime, err := newRuntime(provider, "signoz")
	if err != nil {
		t.Fatal(err)
	}
	runtime.HTTPRequest(context.Background(), "GET", "GET /api/v1/query", 200, 10*time.Millisecond)
	runtime.ActiveQueries(context.Background(), 1)
	runtime.BackendQuery(context.Background(), "query", 5*time.Millisecond, true)
	runtime.ActiveQueries(context.Background(), -1)

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"prometheus_api_bridge_http_server_requests":           false,
		"prometheus_api_bridge_http_server_duration_seconds":   false,
		"prometheus_api_bridge_backend_queries":                false,
		"prometheus_api_bridge_backend_query_duration_seconds": false,
		"prometheus_api_bridge_backend_query_errors":           false,
		"prometheus_api_bridge_backend_queries_active":         false,
	}
	for _, scope := range metrics.ScopeMetrics {
		for _, item := range scope.Metrics {
			if _, ok := want[item.Name]; ok {
				want[item.Name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %q was not recorded", name)
		}
	}
}

func TestNormalizedHTTPMethodBoundsCardinality(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"GET":                 "GET",
		"POST":                "POST",
		"ATTACKER-METHOD-ONE": "_OTHER",
		"ATTACKER-METHOD-TWO": "_OTHER",
	}
	for method, want := range tests {
		if got := normalizedHTTPMethod(method); got != want {
			t.Errorf("normalizedHTTPMethod(%q) = %q, want %q", method, got, want)
		}
	}
}
