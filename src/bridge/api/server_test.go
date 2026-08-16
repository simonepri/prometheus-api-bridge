package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/simonepri/prometheus-api-bridge/bridge/backend"
)

type fakeQuerier struct {
	query        backend.Query
	seriesQuery  backend.SeriesQuery
	result       backend.Result
	seriesResult backend.SeriesResult
	err          error
	queryCount   int
}

func (f *fakeQuerier) QuerySeries(_ context.Context, query backend.SeriesQuery) (backend.SeriesResult, error) {
	f.seriesQuery = query
	if f.err != nil {
		return backend.SeriesResult{}, f.err
	}
	if f.seriesResult.Series != nil || f.seriesResult.Warnings != nil {
		return f.seriesResult, nil
	}
	return backend.SeriesResult{Series: []map[string]string{
		{"__name__": "http_requests_total", "namespace": "api", "pod": "api-0"},
		{"__name__": "http_requests_total", "namespace": "api", "pod": "api-1"},
	}}, nil
}

func (f *fakeQuerier) QueryRange(_ context.Context, query backend.Query) (backend.Result, error) {
	f.query = query
	f.queryCount++
	if f.err != nil {
		return backend.Result{}, f.err
	}
	if f.result.Type != "" || f.result.Series != nil || f.result.Scalar != nil || f.result.Warnings != nil {
		result := f.result
		if result.Type == "" {
			result.Type = fakeResultType(query)
		}
		return result, nil
	}
	return backend.Result{Type: fakeResultType(query), Series: []backend.Series{{
		Labels: map[string]string{"pod": "api-0"},
		Points: []backend.Point{
			{Timestamp: time.Unix(1_700_000_000, 0), Value: 1.25},
			{Timestamp: time.Unix(1_700_000_060, 0), Value: 2.5},
		},
	}}}, nil
}

func fakeResultType(query backend.Query) backend.ResultType {
	if query.RangeQuery || strings.Contains(query.Expression, "[") {
		return backend.ResultTypeMatrix
	}
	return backend.ResultTypeVector
}

func TestUnsupportedQueryUsesPrometheusExecutionError(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=unsupported", nil)
	response := httptest.NewRecorder()
	NewServer(&fakeQuerier{err: backend.ErrUnsupportedQuery}, nil, time.Second).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", response.Code)
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Status != "error" || envelope.ErrorType != "execution" {
		t.Fatalf("response = %#v", envelope)
	}
}

func TestQueryRangePrometheusContract(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	server := httptest.NewServer(NewServer(querier, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + `/api/v1/query_range?query=up&start=1700000000&end=1700000060&step=60`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values []samplePair      `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.StatusCode != http.StatusOK || envelope.Status != "success" || envelope.Data.ResultType != "matrix" {
		t.Fatalf("response = %#v, status = %d", envelope, response.StatusCode)
	}
	if len(envelope.Data.Result) != 1 || len(envelope.Data.Result[0].Values) != 2 {
		t.Fatalf("result = %#v", envelope.Data.Result)
	}
	if querier.query.Expression != "up" || querier.query.Step != time.Minute || !querier.query.RangeQuery {
		t.Fatalf("query = %#v", querier.query)
	}
}

func TestInstantRangeSelectorUsesOpenClosedBoundary(t *testing.T) {
	t.Parallel()
	evaluationTime := time.Unix(1_700_000_060, 0)
	start := evaluationTime.Add(-30 * time.Minute)
	querier := &fakeQuerier{result: backend.Result{Series: []backend.Series{{
		Labels: map[string]string{"pod": "api-0"},
		Points: []backend.Point{
			{Timestamp: start, Value: 1},
			{Timestamp: start.Add(time.Millisecond), Value: 2},
			{Timestamp: evaluationTime, Value: 3},
			{Timestamp: evaluationTime.Add(time.Millisecond), Value: 4},
		},
	}}}}
	request := httptest.NewRequest(
		http.MethodGet,
		`/api/v1/query?query=up%5B30m%5D&time=1700000060`,
		nil,
	)
	response := httptest.NewRecorder()
	NewServer(querier, nil, time.Second).Handler().ServeHTTP(response, request)
	var envelope struct {
		Data struct {
			Result []struct {
				Values []samplePair `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Code != http.StatusOK || len(envelope.Data.Result) != 1 || len(envelope.Data.Result[0].Values) != 2 {
		t.Fatalf("response = %s", response.Body.String())
	}
	if got := envelope.Data.Result[0].Values[0][1]; got != "2" {
		t.Fatalf("first value = %#v", got)
	}
	if got := envelope.Data.Result[0].Values[1][1]; got != "3" {
		t.Fatalf("last value = %#v", got)
	}
}

func TestInstantQueryReturnsLastSample(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up&time=1700000060", nil)
	response := httptest.NewRecorder()
	NewServer(querier, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second).Handler().ServeHTTP(response, request)
	var envelope struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value samplePair `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Data.ResultType != "vector" || len(envelope.Data.Result) != 1 {
		t.Fatalf("response = %s", response.Body.String())
	}
	if got := envelope.Data.Result[0].Value[1]; got != "2.5" {
		t.Fatalf("last value = %#v", got)
	}
}

func TestSimpleScalarQueryDoesNotReachBackend(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader("query=1%2B1&time=1700000060"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	NewServer(querier, nil, time.Second).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"resultType":"scalar"`) ||
		!strings.Contains(response.Body.String(), `"2"`) {
		t.Fatalf("response = %s", response.Body.String())
	}
	if querier.query.Expression != "" {
		t.Fatalf("backend received scalar probe: %#v", querier.query)
	}
}

func TestBackendScalarResultPreservesPrometheusType(t *testing.T) {
	t.Parallel()
	point := backend.Point{Timestamp: time.Unix(1_700_000_060, 0), Value: 42}
	for _, expression := range []string{"scalar(up)", "time()"} {
		querier := &fakeQuerier{result: backend.Result{
			Type:   backend.ResultTypeScalar,
			Scalar: &point,
		}}
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/v1/query?query="+url.QueryEscape(expression)+"&time=1700000060",
			nil,
		)
		response := httptest.NewRecorder()
		NewServer(querier, nil, time.Second).Handler().ServeHTTP(response, request)
		var envelope struct {
			Data struct {
				ResultType string     `json:"resultType"`
				Result     samplePair `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s decode: %v", expression, err)
		}
		if response.Code != http.StatusOK || envelope.Data.ResultType != "scalar" ||
			envelope.Data.Result[1] != "42" {
			t.Fatalf("%s response = %s", expression, response.Body.String())
		}
	}
}

func TestBuildInfoCompatibilityEndpoint(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/status/buildinfo", nil)
	response := httptest.NewRecorder()
	NewServer(&fakeQuerier{}, nil, time.Second).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"version":"2.55.0"`) ||
		!strings.Contains(response.Body.String(), `"revision":"prometheus-api-bridge"`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestInstantRangeSelectorReturnsMatrix(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	request := httptest.NewRequest(
		http.MethodGet,
		`/api/v1/query?query=kube_pod_labels%7Bnamespace%3D%22bridge-test%22%7D%5B30m%5D&time=1700000060`,
		nil,
	)
	response := httptest.NewRecorder()
	NewServer(querier, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second).Handler().ServeHTTP(response, request)
	var envelope struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values []samplePair `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Code != http.StatusOK || envelope.Data.ResultType != "matrix" {
		t.Fatalf("response = %s", response.Body.String())
	}
	if len(envelope.Data.Result) != 1 || len(envelope.Data.Result[0].Values) != 2 {
		t.Fatalf("result = %#v", envelope.Data.Result)
	}
	if querier.query.Expression != `kube_pod_labels{namespace="bridge-test"}[30m]` {
		t.Fatalf("expression = %q", querier.query.Expression)
	}
	if querier.query.Start != time.Unix(1_699_998_260, 0) || querier.query.Step != time.Minute {
		t.Fatalf("query = %#v", querier.query)
	}
}

func TestInstantRangeSelectorIsBounded(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up%5B2h%5D", nil)
	response := httptest.NewRecorder()
	server := NewServerWithOptions(querier, nil, Options{
		MaxQueryRange:      time.Hour,
		MaxPointsPerSeries: 1_000_000,
	})
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "configured maximum") {
		t.Fatalf("response = %s", response.Body.String())
	}
	if querier.queryCount != 0 {
		t.Fatalf("backend received %d queries", querier.queryCount)
	}
}

func TestPrometheusDuration(t *testing.T) {
	t.Parallel()
	duration, err := parsePrometheusDuration("1w2d3h4m5s6ms")
	if err != nil {
		t.Fatal(err)
	}
	want := 9*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second + 6*time.Millisecond
	if duration != want {
		t.Fatalf("duration = %s, want %s", duration, want)
	}
}

func TestBadRequestUsesPrometheusErrorEnvelope(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/query_range?query=up", nil)
	response := httptest.NewRecorder()
	NewServer(&fakeQuerier{}, nil, time.Second).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Status != "error" || envelope.ErrorType != "bad_data" {
		t.Fatalf("response = %#v", envelope)
	}
}

func TestSeriesDiscoveryContract(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	request := httptest.NewRequest(
		http.MethodGet,
		`/api/v1/series?match%5B%5D=http_requests_total%7Bnamespace%3D%22api%22%7D&start=1700000000&end=1700000060&limit=1`,
		nil,
	)
	response := httptest.NewRecorder()
	NewServer(querier, nil, time.Second).Handler().ServeHTTP(response, request)
	var envelope struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Code != http.StatusOK || envelope.Status != "success" || len(envelope.Data) != 1 {
		t.Fatalf("response = %s", response.Body.String())
	}
	if len(querier.seriesQuery.Matchers) != 1 || querier.seriesQuery.Matchers[0] != `http_requests_total{namespace="api"}` {
		t.Fatalf("query = %#v", querier.seriesQuery)
	}
}

func TestLabelDiscoveryContracts(t *testing.T) {
	t.Parallel()
	server := NewServer(&fakeQuerier{}, nil, time.Second).Handler()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/api/v1/labels", want: `["__name__","namespace","pod"]`},
		{path: "/api/v1/label/__name__/values", want: `["http_requests_total"]`},
		{path: "/api/v1/label/pod/values", want: `["api-0","api-1"]`},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) {
			t.Fatalf("%s response = %s", test.path, response.Body.String())
		}
	}
}

func TestSeriesRequiresMatcher(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/series", nil)
	response := httptest.NewRecorder()
	NewServer(&fakeQuerier{}, nil, time.Second).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "missing match[] parameter") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestSeriesMatcherCountIsBounded(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/series?match%5B%5D=up&match%5B%5D=http_requests_total",
		nil,
	)
	response := httptest.NewRecorder()
	server := NewServerWithOptions(&fakeQuerier{}, nil, Options{MaxMatchersPerRequest: 1})
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "match[] count") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestQueryRangeIsBounded(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query_range?query=up&start=1700000000&end=1700000120&step=60",
		nil,
	)
	response := httptest.NewRecorder()
	server := NewServerWithOptions(&fakeQuerier{}, nil, Options{MaxQueryRange: time.Minute})
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "configured maximum") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestQueryResolutionIsBounded(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{}
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query_range?query=up&start=1700000000&end=1702592000&step=0.001",
		nil,
	)
	response := httptest.NewRecorder()
	server := NewServerWithOptions(querier, nil, Options{MaxPointsPerSeries: 50_000})
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "points per series") {
		t.Fatalf("response = %s", response.Body.String())
	}
	if querier.queryCount != 0 {
		t.Fatalf("backend received %d queries", querier.queryCount)
	}
}

func TestRequestBodyIsBounded(t *testing.T) {
	t.Parallel()
	body := "query=" + strings.Repeat("a", 128)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server := NewServerWithOptions(&fakeQuerier{}, nil, Options{MaxRequestBodyBytes: 32})
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid form data") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

type recordingObserver struct {
	httpMethod       string
	httpRoute        string
	httpStatus       int
	backendOperation string
	activeDelta      int64
}

func (o *recordingObserver) HTTPRequest(
	_ context.Context,
	method string,
	route string,
	status int,
	_ time.Duration,
) {
	o.httpMethod = method
	o.httpRoute = route
	o.httpStatus = status
}

func (o *recordingObserver) BackendQuery(
	_ context.Context,
	operation string,
	_ time.Duration,
	_ bool,
) {
	o.backendOperation = operation
}

func (o *recordingObserver) ActiveQueries(_ context.Context, delta int64) {
	o.activeDelta += delta
}

func TestObserverReceivesBoundedRequestAndBackendAttributes(t *testing.T) {
	t.Parallel()
	observer := &recordingObserver{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
	response := httptest.NewRecorder()
	server := NewServerWithOptions(&fakeQuerier{}, nil, Options{Observer: observer})
	server.Handler().ServeHTTP(response, request)
	if observer.httpMethod != http.MethodGet || observer.httpRoute != "GET /api/v1/query" ||
		observer.httpStatus != http.StatusOK {
		t.Fatalf("HTTP observation = %#v", observer)
	}
	if observer.backendOperation != "query" || observer.activeDelta != 0 {
		t.Fatalf("backend observation = %#v", observer)
	}
}

func TestOptionalBearerAuthentication(t *testing.T) {
	t.Parallel()
	server := NewServerWithOptions(&fakeQuerier{}, nil, Options{BearerToken: "test-token"}).Handler()
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer test-token")
	authorized := httptest.NewRecorder()
	server.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized response = %s", authorized.Body.String())
	}
	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/-/healthy", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
}

func TestBackendErrorDoesNotExposeUpstreamDetails(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
	response := httptest.NewRecorder()
	NewServer(&fakeQuerier{err: errors.New("upstream response contained api-key=secret")}, nil, time.Second).
		Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "secret") || !strings.Contains(response.Body.String(), "backend query failed") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestNormalizedQueryResultIsBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		result     backend.Result
		maxSeries  int
		maxSamples int
	}{
		{
			name: "series",
			result: backend.Result{Series: []backend.Series{
				{Points: []backend.Point{{Value: 1}}},
				{Points: []backend.Point{{Value: 2}}},
			}},
			maxSeries:  1,
			maxSamples: 10,
		},
		{
			name: "samples",
			result: backend.Result{Series: []backend.Series{{Points: []backend.Point{
				{Value: 1}, {Value: 2}, {Value: 3},
			}}}},
			maxSeries:  10,
			maxSamples: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
			response := httptest.NewRecorder()
			server := NewServerWithOptions(&fakeQuerier{result: test.result}, nil, Options{
				MaxSeries:  test.maxSeries,
				MaxSamples: test.maxSamples,
			})
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity ||
				!strings.Contains(response.Body.String(), "configured limits") {
				t.Fatalf("response = %s", response.Body.String())
			}
		})
	}
}

func TestNormalizedMetadataResultIsBounded(t *testing.T) {
	t.Parallel()
	querier := &fakeQuerier{seriesResult: backend.SeriesResult{Series: []map[string]string{
		{"__name__": "one"},
		{"__name__": "two"},
	}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/labels", nil)
	response := httptest.NewRecorder()
	NewServerWithOptions(querier, nil, Options{MaxSeries: 1}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "configured limits") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestNonFiniteQueryParametersAreRejected(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/api/v1/query?query=up&time=NaN",
		"/api/v1/query?query=up&time=%2BInf",
		"/api/v1/query_range?query=up&start=NaN&end=1700000060&step=60",
		"/api/v1/query_range?query=up&start=1700000000&end=1700000060&step=%2BInf",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		NewServer(&fakeQuerier{}, nil, time.Second).Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, response = %s", path, response.Code, response.Body.String())
		}
	}
}
