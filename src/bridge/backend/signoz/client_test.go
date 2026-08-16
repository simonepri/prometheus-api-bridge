package signoz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonepri/prometheus-api-bridge/bridge/backend"
)

func TestQueryRangeUsesNativePrometheusAPI(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(request *http.Request) string {
		if request.URL.Path != queryRangePath {
			t.Errorf("path = %q, want %q", request.URL.Path, queryRangePath)
		}
		assertAPIKey(t, request)
		assertQuery(t, request.URL.Query(), map[string]string{
			"query": `sum(rate(cpu_total[1m])) by (pod)`,
			"start": "2023-11-14T22:13:20Z",
			"end":   "2023-11-14T22:14:20Z",
			"step":  "15",
		})
		return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"api-0"},"values":[[1700000000,"1.5"],[1700000015,"2.5"]]}]},"warnings":["bounded result"]}`
	})

	client := newTestClient(t, server, Config{})
	result, err := client.QueryRange(context.Background(), backend.Query{
		Expression: `sum(rate(cpu_total[1m])) by (pod)`,
		Start:      time.Unix(1_700_000_000, 0),
		End:        time.Unix(1_700_000_060, 0),
		Step:       15 * time.Second,
		RangeQuery: true,
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(result.Series) != 1 || result.Series[0].Labels["pod"] != "api-0" {
		t.Fatalf("series = %#v", result.Series)
	}
	if len(result.Series[0].Points) != 2 || result.Series[0].Points[1].Value != 2.5 {
		t.Fatalf("points = %#v", result.Series[0].Points)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "bounded result" {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestNullPrometheusResultBecomesEmpty(t *testing.T) {
	t.Parallel()
	for _, response := range []string{
		`{"status":"success","data":{"resultType":"matrix","result":null}}`,
		`{"status":"success","data":{"result":null,"resultType":"matrix"}}`,
	} {
		server := prometheusServer(t, func(*http.Request) string { return response })
		client := newTestClient(t, server, Config{})
		result, err := client.QueryRange(context.Background(), backend.Query{
			Expression: "up",
			Start:      time.Unix(1_700_000_000, 0),
			End:        time.Unix(1_700_000_060, 0),
			Step:       15 * time.Second,
			RangeQuery: true,
		})
		if err != nil {
			t.Fatalf("QueryRange: %v", err)
		}
		if len(result.Series) != 0 {
			t.Fatalf("series = %#v, want empty", result.Series)
		}
	}
}

func TestPrometheusResultFieldOrderIsIgnored(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(*http.Request) string {
		return `{"status":"success","data":{"result":[{"metric":{"pod":"api-0"},"values":[[1700000000,"1.5"]]}],"resultType":"matrix"}}`
	})

	client := newTestClient(t, server, Config{})
	result, err := client.QueryRange(context.Background(), backend.Query{
		Expression: "up",
		Start:      time.Unix(1_700_000_000, 0),
		End:        time.Unix(1_700_000_060, 0),
		Step:       15 * time.Second,
		RangeQuery: true,
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(result.Series) != 1 || result.Series[0].Labels["pod"] != "api-0" || len(result.Series[0].Points) != 1 {
		t.Fatalf("series = %#v", result.Series)
	}
}

func TestScalarResultTypeAndValueArePreserved(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(*http.Request) string {
		return `{"status":"success","data":{"resultType":"scalar","result":[1700000060,"42"]}}`
	})

	client := newTestClient(t, server, Config{})
	result, err := client.QueryRange(context.Background(), backend.Query{
		Expression: "scalar(up)",
		End:        time.Unix(1_700_000_060, 0),
		Step:       time.Minute,
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if result.Type != backend.ResultTypeScalar || result.Scalar == nil || result.Scalar.Value != 42 {
		t.Fatalf("result = %#v", result)
	}
}

func TestInstantRangeSelectorIsPassedThrough(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(request *http.Request) string {
		if request.URL.Path != queryPath {
			t.Errorf("path = %q, want %q", request.URL.Path, queryPath)
		}
		assertQuery(t, request.URL.Query(), map[string]string{
			"query": `kube_pod_labels{namespace="bridge-test"}[30m]`,
			"time":  "2023-11-14T22:14:20Z",
		})
		return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"api-0"},"values":[[1699998260,"1"],[1700000060,"2"]]}]}}`
	})

	client := newTestClient(t, server, Config{})
	result, err := client.QueryRange(context.Background(), backend.Query{
		Expression: `kube_pod_labels{namespace="bridge-test"}[30m]`,
		Start:      time.Unix(1_699_998_260, 0),
		End:        time.Unix(1_700_000_060, 0),
		Step:       time.Minute,
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(result.Series) != 1 || len(result.Series[0].Points) != 2 {
		t.Fatalf("series = %#v", result.Series)
	}
}

func TestFractionalStepIsPreserved(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(request *http.Request) string {
		if got := request.URL.Query().Get("step"); got != "0.5" {
			t.Errorf("step = %q, want 0.5", got)
		}
		return `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	})

	client := newTestClient(t, server, Config{})
	_, err := client.QueryRange(context.Background(), backend.Query{
		Expression: "up",
		Start:      time.Unix(1_700_000_000, 0),
		End:        time.Unix(1_700_000_001, 0),
		Step:       500 * time.Millisecond,
		RangeQuery: true,
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
}

func TestQuerySeriesUsesPromQLSelectors(t *testing.T) {
	t.Parallel()
	queries := make(chan string, 2)
	server := prometheusServer(t, func(request *http.Request) string {
		if request.URL.Path != queryRangePath {
			t.Errorf("path = %q, want %q", request.URL.Path, queryRangePath)
		}
		queries <- request.URL.Query().Get("query")
		return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"http_requests_total","namespace":"api"},"values":[[1700000000,"1"]]}]}}`
	})

	client := newTestClient(t, server, Config{})
	result, err := client.QuerySeries(context.Background(), backend.SeriesQuery{
		Matchers: []string{`http_requests_total{namespace="api"}`, `http_requests_total`},
		Start:    time.Unix(1_700_000_000, 0),
		End:      time.Unix(1_700_000_060, 0),
	})
	if err != nil {
		t.Fatalf("QuerySeries: %v", err)
	}
	if len(result.Series) != 1 || result.Series[0]["__name__"] != "http_requests_total" {
		t.Fatalf("series = %#v", result.Series)
	}
	if first, second := <-queries, <-queries; first != `http_requests_total{namespace="api"}` || second != "http_requests_total" {
		t.Fatalf("queries = %q, %q", first, second)
	}
}

func TestResponseBodyIsBounded(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(*http.Request) string {
		return `{"status":"success","padding":"` + strings.Repeat("x", 128) + `"}`
	})
	client := newTestClient(t, server, Config{MaxResponseBytes: 64})
	_, err := client.QueryRange(context.Background(), backend.Query{Expression: "up", Step: time.Minute})
	if !errors.Is(err, backend.ErrResponseLimit) {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizedResultIsBoundedDuringDecode(t *testing.T) {
	t.Parallel()
	server := prometheusServer(t, func(*http.Request) string {
		return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"a"},"values":[[1700000000,"1"],[1700000060,"2"]]},{"metric":{"pod":"b"},"values":[[1700000000,"3"]]}]}}`
	})

	for _, test := range []struct {
		name       string
		maxSeries  int
		maxSamples int
	}{
		{name: "series", maxSeries: 1, maxSamples: 10},
		{name: "samples", maxSeries: 10, maxSamples: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, server, Config{MaxSeries: test.maxSeries, MaxSamples: test.maxSamples})
			_, err := client.QueryRange(context.Background(), backend.Query{Expression: "up", Step: time.Minute})
			if !errors.Is(err, backend.ErrResponseLimit) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSeriesDiscoveryIsBoundedAcrossMatchers(t *testing.T) {
	t.Parallel()
	requestCount := 0
	server := prometheusServer(t, func(*http.Request) string {
		requestCount++
		return fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"pod-%d"},"values":[[1700000000,"1"]]}]}}`, requestCount)
	})
	client := newTestClient(t, server, Config{MaxSeries: 1})
	_, err := client.QuerySeries(context.Background(), backend.SeriesQuery{
		Matchers: []string{"metric_one", "metric_two"},
		Start:    time.Unix(1_700_000_000, 0),
		End:      time.Unix(1_700_000_060, 0),
	})
	if !errors.Is(err, backend.ErrResponseLimit) {
		t.Fatalf("error = %v", err)
	}
}

func TestBackendErrorBodyIsNotReturned(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
		_, _ = response.Write([]byte(`{"status":"error","error":"api-key=secret"}`))
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server, Config{})
	_, err := client.QueryRange(context.Background(), backend.Query{Expression: "up", Step: time.Minute})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestTransportErrorDoesNotExposeQuery(t *testing.T) {
	t.Parallel()
	transportError := errors.New("dial failed")
	client, err := New(Config{
		URL:    "https://signoz.example.test",
		APIKey: "test-key",
	}, &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportError
	})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.QueryRange(context.Background(), backend.Query{
		Expression: `secret_metric{tenant="sensitive-tenant"}`,
		End:        time.Unix(1_700_000_060, 0),
		Step:       time.Minute,
	})
	if err == nil {
		t.Fatal("QueryRange succeeded")
	}
	if strings.Contains(err.Error(), "sensitive-tenant") || strings.Contains(err.Error(), "query=") {
		t.Fatalf("transport error exposed query: %v", err)
	}
	if !errors.Is(err, transportError) {
		t.Fatalf("transport error lost wrapped cause: %v", err)
	}
}

func TestNewRejectsRedirectsForSuppliedHTTPClient(t *testing.T) {
	t.Parallel()
	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	suppliedClient := source.Client()
	client, err := New(Config{URL: source.URL, APIKey: "signoz-secret"}, suppliedClient)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.QueryRange(context.Background(), backend.Query{Expression: "up", Step: time.Minute})
	if !errors.Is(err, backend.ErrRedirect) {
		t.Fatalf("QueryRange error = %v, want redirect rejection", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d request(s), want 0", targetRequests.Load())
	}
	if suppliedClient.CheckRedirect != nil {
		t.Fatal("New mutated the supplied HTTP client")
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{URL: "https://example.test"}, nil); err == nil {
		t.Fatal("New succeeded without API key")
	}
}

func prometheusServer(t *testing.T, response func(*http.Request) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(response(request)))
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestClient(t *testing.T, server *httptest.Server, config Config) *Client {
	t.Helper()
	config.URL = server.URL
	config.APIKey = "test-key"
	client, err := New(config, server.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func assertAPIKey(t *testing.T, request *http.Request) {
	t.Helper()
	if got := request.Header.Get("SIGNOZ-API-KEY"); got != "test-key" {
		t.Errorf("SIGNOZ-API-KEY = %q", got)
	}
}

func assertQuery(t *testing.T, parameters url.Values, expected map[string]string) {
	t.Helper()
	for name, want := range expected {
		if got := parameters.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (transport roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
