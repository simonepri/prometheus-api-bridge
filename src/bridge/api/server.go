// Package api exposes the supported Prometheus HTTP query API.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simonepri/prometheus-api-bridge/bridge/backend"
)

const (
	defaultStep              = time.Minute
	defaultDiscoveryLookback = time.Hour
	defaultMaxQueryRange     = 30 * 24 * time.Hour
	defaultMaxConcurrent     = 10
	defaultMaxPoints         = 50_000
	defaultMaxRequestBody    = 1 << 20
	defaultMaxMatchers       = 32
	defaultMaxSeries         = backend.DefaultMaxSeries
	defaultMaxSamples        = backend.DefaultMaxSamples
)

// Observer receives bounded-cardinality operational events from the query API.
type Observer interface {
	HTTPRequest(context.Context, string, string, int, time.Duration)
	BackendQuery(context.Context, string, time.Duration, bool)
	ActiveQueries(context.Context, int64)
}

type noopObserver struct{}

func (noopObserver) HTTPRequest(context.Context, string, string, int, time.Duration) {}
func (noopObserver) BackendQuery(context.Context, string, time.Duration, bool)       {}
func (noopObserver) ActiveQueries(context.Context, int64)                            {}

// Options bounds the public HTTP API and backend resource usage.
type Options struct {
	Timeout               time.Duration
	DiscoveryLookback     time.Duration
	MaxQueryRange         time.Duration
	MaxConcurrentQueries  int
	MaxPointsPerSeries    int
	MaxRequestBodyBytes   int64
	MaxMatchersPerRequest int
	MaxSeries             int
	MaxSamples            int
	BearerToken           string
	Observer              Observer
}

// Server exposes a bounded Prometheus HTTP API over one backend.
type Server struct {
	querier  backend.Querier
	logger   *slog.Logger
	options  Options
	slots    chan struct{}
	observer Observer
}

// NewServer builds the HTTP API.
func NewServer(querier backend.Querier, logger *slog.Logger, timeout time.Duration) *Server {
	return NewServerWithOptions(querier, logger, Options{Timeout: timeout})
}

// NewServerWithOptions builds the HTTP API with explicit operational bounds.
func NewServerWithOptions(querier backend.Querier, logger *slog.Logger, options Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Second
	}
	if options.DiscoveryLookback <= 0 {
		options.DiscoveryLookback = defaultDiscoveryLookback
	}
	if options.MaxQueryRange <= 0 {
		options.MaxQueryRange = defaultMaxQueryRange
	}
	if options.MaxConcurrentQueries <= 0 {
		options.MaxConcurrentQueries = defaultMaxConcurrent
	}
	if options.MaxPointsPerSeries <= 0 {
		options.MaxPointsPerSeries = defaultMaxPoints
	}
	if options.MaxRequestBodyBytes <= 0 {
		options.MaxRequestBodyBytes = defaultMaxRequestBody
	}
	if options.MaxMatchersPerRequest <= 0 {
		options.MaxMatchersPerRequest = defaultMaxMatchers
	}
	if options.MaxSeries <= 0 {
		options.MaxSeries = defaultMaxSeries
	}
	if options.MaxSamples <= 0 {
		options.MaxSamples = defaultMaxSamples
	}
	if options.Observer == nil {
		options.Observer = noopObserver{}
	}
	return &Server{
		querier:  querier,
		logger:   logger,
		options:  options,
		slots:    make(chan struct{}, options.MaxConcurrentQueries),
		observer: options.Observer,
	}
}

// Handler returns the bridge HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/healthy", s.health)
	mux.HandleFunc("GET /-/ready", s.health)
	mux.HandleFunc("GET /api/v1/status/buildinfo", s.buildInfo)
	mux.HandleFunc("GET /api/v1/query", s.query)
	mux.HandleFunc("POST /api/v1/query", s.query)
	mux.HandleFunc("GET /api/v1/query_range", s.queryRange)
	mux.HandleFunc("POST /api/v1/query_range", s.queryRange)
	mux.HandleFunc("GET /api/v1/series", s.series)
	mux.HandleFunc("POST /api/v1/series", s.series)
	mux.HandleFunc("GET /api/v1/labels", s.labels)
	mux.HandleFunc("POST /api/v1/labels", s.labels)
	mux.HandleFunc("GET /api/v1/label/{name}/values", s.labelValues)
	mux.HandleFunc("POST /api/v1/label/{name}/values", s.labelValues)
	return s.observeRequests(s.authorize(mux))
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ok\n"))
}

func (s *Server) buildInfo(response http.ResponseWriter, _ *http.Request) {
	writeSuccess(response, map[string]string{
		"version":   "2.55.0",
		"revision":  "prometheus-api-bridge",
		"branch":    "HEAD",
		"buildUser": "prometheus-api-bridge",
		"buildDate": "",
		"goVersion": "",
	}, nil)
}

func (s *Server) query(response http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", "invalid form data")
		return
	}
	expression := strings.TrimSpace(request.Form.Get("query"))
	if expression == "" {
		writeError(response, http.StatusBadRequest, "bad_data", "missing query parameter")
		return
	}
	at, err := parseTime(request.Form.Get("time"), time.Now())
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if value, ok := simpleScalar(expression); ok {
		writeSuccess(response, prometheusData{
			ResultType: "scalar",
			Result:     samplePair{float64(at.UnixMilli()) / 1000, prometheusFloat(value)},
		}, nil)
		return
	}
	step := defaultStep
	start := at.Add(-step)
	_, window, isRangeSelector, err := splitRangeSelector(expression)
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if isRangeSelector {
		start = at.Add(-window)
		if window > s.options.MaxQueryRange {
			writeError(response, http.StatusBadRequest, "bad_data", "query range exceeds configured maximum")
			return
		}
		if window < step {
			step = window
		}
		if err := s.validateResolution(start, at, step); err != nil {
			writeError(response, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
	}
	result, err := s.queryBackend(request.Context(), backend.Query{
		Expression: expression,
		Start:      start,
		End:        at,
		Step:       step,
	})
	if err != nil {
		s.writeBackendError(response, err)
		return
	}
	if isRangeSelector {
		if result.Type != backend.ResultTypeMatrix {
			s.writeBackendError(response, fmt.Errorf("backend returned %q for range selector", result.Type))
			return
		}
		result.Series = filterOpenClosed(result.Series, start, at)
	}
	s.writeQuerySuccess(response, result)
}

func (s *Server) queryRange(response http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", "invalid form data")
		return
	}
	expression := strings.TrimSpace(request.Form.Get("query"))
	if expression == "" {
		writeError(response, http.StatusBadRequest, "bad_data", "missing query parameter")
		return
	}
	start, err := parseRequiredTime(request.Form.Get("start"), "start")
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	end, err := parseRequiredTime(request.Form.Get("end"), "end")
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if end.Before(start) {
		writeError(response, http.StatusBadRequest, "bad_data", "end timestamp must not be before start time")
		return
	}
	if end.Sub(start) > s.options.MaxQueryRange {
		writeError(response, http.StatusBadRequest, "bad_data", "query range exceeds configured maximum")
		return
	}
	step, err := parseStep(request.Form.Get("step"))
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if err := s.validateResolution(start, end, step); err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	result, err := s.queryBackend(request.Context(), backend.Query{
		Expression: expression,
		Start:      start,
		End:        end,
		Step:       step,
		RangeQuery: true,
	})
	if err != nil {
		s.writeBackendError(response, err)
		return
	}
	if result.Type != backend.ResultTypeMatrix {
		s.writeBackendError(response, fmt.Errorf("backend returned %q for range query", result.Type))
		return
	}
	s.writeQuerySuccess(response, result)
}

func (s *Server) writeQuerySuccess(response http.ResponseWriter, result backend.Result) {
	var encoded any
	switch result.Type {
	case backend.ResultTypeMatrix:
		encoded = rangeResult(result.Series)
	case backend.ResultTypeVector:
		encoded = instantResult(result.Series)
	case backend.ResultTypeScalar:
		if result.Scalar == nil {
			s.writeBackendError(response, fmt.Errorf("backend returned scalar result without a value"))
			return
		}
		encoded = prometheusSample(*result.Scalar)
	default:
		s.writeBackendError(response, fmt.Errorf("backend returned unsupported result type %q", result.Type))
		return
	}
	writeSuccess(response, prometheusData{ResultType: string(result.Type), Result: encoded}, result.Warnings)
}

func filterOpenClosed(series []backend.Series, start time.Time, end time.Time) []backend.Series {
	filtered := make([]backend.Series, 0, len(series))
	for _, item := range series {
		points := make([]backend.Point, 0, len(item.Points))
		for _, point := range item.Points {
			if point.Timestamp.After(start) && !point.Timestamp.After(end) {
				points = append(points, point)
			}
		}
		if len(points) > 0 {
			item.Points = points
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func (s *Server) series(response http.ResponseWriter, request *http.Request) {
	query, limit, err := s.parseSeriesQuery(request, true)
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	result, err := s.querySeries(request.Context(), query)
	if err != nil {
		s.writeBackendError(response, err)
		return
	}
	sort.Slice(result.Series, func(left int, right int) bool {
		return labelSetKey(result.Series[left]) < labelSetKey(result.Series[right])
	})
	writeSuccess(response, limited(result.Series, limit), result.Warnings)
}

func (s *Server) labels(response http.ResponseWriter, request *http.Request) {
	query, limit, err := s.parseSeriesQuery(request, false)
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	result, err := s.querySeries(request.Context(), query)
	if err != nil {
		s.writeBackendError(response, err)
		return
	}
	unique := make(map[string]struct{})
	for _, series := range result.Series {
		for name := range series {
			unique[name] = struct{}{}
		}
	}
	writeSuccess(response, limited(sortedKeys(unique), limit), result.Warnings)
}

func (s *Server) labelValues(response http.ResponseWriter, request *http.Request) {
	name := strings.TrimSpace(request.PathValue("name"))
	if name == "" {
		writeError(response, http.StatusBadRequest, "bad_data", "missing label name")
		return
	}
	query, limit, err := s.parseSeriesQuery(request, false)
	if err != nil {
		writeError(response, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	result, err := s.querySeries(request.Context(), query)
	if err != nil {
		s.writeBackendError(response, err)
		return
	}
	unique := make(map[string]struct{})
	for _, series := range result.Series {
		if value, ok := series[name]; ok {
			unique[value] = struct{}{}
		}
	}
	writeSuccess(response, limited(sortedKeys(unique), limit), result.Warnings)
}

func (s *Server) querySeries(requestContext context.Context, query backend.SeriesQuery) (backend.SeriesResult, error) {
	querier, ok := s.querier.(backend.SeriesQuerier)
	if !ok {
		return backend.SeriesResult{}, backend.ErrUnsupportedMetadata
	}
	ctx, cancel := context.WithTimeout(requestContext, s.options.Timeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return backend.SeriesResult{}, err
	}
	defer s.release()
	started := time.Now()
	result, err := querier.QuerySeries(ctx, query)
	if err == nil {
		err = validateSeriesCount(len(result.Series), s.options.MaxSeries)
	}
	s.observer.BackendQuery(context.WithoutCancel(ctx), "series", time.Since(started), err != nil)
	return result, err
}

func (s *Server) parseSeriesQuery(request *http.Request, requireMatcher bool) (backend.SeriesQuery, int, error) {
	if err := request.ParseForm(); err != nil {
		return backend.SeriesQuery{}, 0, fmt.Errorf("invalid form data")
	}
	matchers := request.Form["match[]"]
	if requireMatcher && len(matchers) == 0 {
		return backend.SeriesQuery{}, 0, fmt.Errorf("missing match[] parameter")
	}
	if len(matchers) > s.options.MaxMatchersPerRequest {
		return backend.SeriesQuery{}, 0, fmt.Errorf("match[] count exceeds configured maximum")
	}
	now := time.Now()
	start, err := parseTime(request.Form.Get("start"), now.Add(-s.options.DiscoveryLookback))
	if err != nil {
		return backend.SeriesQuery{}, 0, fmt.Errorf("invalid start parameter: %w", err)
	}
	end, err := parseTime(request.Form.Get("end"), now)
	if err != nil {
		return backend.SeriesQuery{}, 0, fmt.Errorf("invalid end parameter: %w", err)
	}
	if end.Before(start) {
		return backend.SeriesQuery{}, 0, fmt.Errorf("end timestamp must not be before start time")
	}
	if end.Sub(start) > s.options.MaxQueryRange {
		return backend.SeriesQuery{}, 0, fmt.Errorf("query range exceeds configured maximum")
	}
	limit, err := parseLimit(request.Form.Get("limit"))
	if err != nil {
		return backend.SeriesQuery{}, 0, err
	}
	return backend.SeriesQuery{Matchers: matchers, Start: start, End: end}, limit, nil
}

func (s *Server) queryBackend(requestContext context.Context, query backend.Query) (backend.Result, error) {
	ctx, cancel := context.WithTimeout(requestContext, s.options.Timeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		return backend.Result{}, err
	}
	defer s.release()
	started := time.Now()
	result, err := s.querier.QueryRange(ctx, query)
	if err == nil {
		err = validateQueryResult(result, s.options.MaxSeries, s.options.MaxSamples)
	}
	s.observer.BackendQuery(context.WithoutCancel(ctx), "query", time.Since(started), err != nil)
	return result, err
}

func validateQueryResult(result backend.Result, maxSeries int, maxSamples int) error {
	if err := validateSeriesCount(len(result.Series), maxSeries); err != nil {
		return err
	}
	totalSamples := 0
	for _, series := range result.Series {
		if len(series.Points) > maxSamples-totalSamples {
			return fmt.Errorf("%w: result contains more than %d samples", backend.ErrResponseLimit, maxSamples)
		}
		totalSamples += len(series.Points)
	}
	return nil
}

func validateSeriesCount(count int, maximum int) error {
	if count > maximum {
		return fmt.Errorf("%w: result contains %d series, maximum is %d", backend.ErrResponseLimit, count, maximum)
	}
	return nil
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		s.observer.ActiveQueries(context.WithoutCancel(ctx), 1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() {
	<-s.slots
	s.observer.ActiveQueries(context.Background(), -1)
}

func (s *Server) validateResolution(start time.Time, end time.Time, step time.Duration) error {
	if step <= 0 || end.Before(start) {
		return fmt.Errorf("invalid query resolution")
	}
	points := int64(end.Sub(start)/step) + 1
	if points > int64(s.options.MaxPointsPerSeries) {
		return fmt.Errorf(
			"query resolution requests %d points per series, exceeding configured maximum %d",
			points,
			s.options.MaxPointsPerSeries,
		)
	}
	return nil
}

func (s *Server) authorize(next http.Handler) http.Handler {
	if s.options.BearerToken == "" {
		return next
	}
	expectedTokenHash := sha256.Sum256([]byte(s.options.BearerToken))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/-/") {
			next.ServeHTTP(response, request)
			return
		}
		provided, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		providedTokenHash := sha256.Sum256([]byte(provided))
		if !ok || subtle.ConstantTimeCompare(providedTokenHash[:], expectedTokenHash[:]) != 1 {
			writeError(response, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(response, request)
	})
}

func parseLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("limit must be a non-negative integer")
	}
	return limit, nil
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func limited[T any](values []T, limit int) []T {
	if limit > 0 && len(values) > limit {
		return values[:limit]
	}
	return values
}

func labelSetKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var key strings.Builder
	for _, name := range names {
		key.WriteString(name)
		key.WriteByte(0)
		key.WriteString(labels[name])
		key.WriteByte(0)
	}
	return key.String()
}

func (s *Server) writeBackendError(response http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	errorType := "execution"
	message := "backend query failed"
	switch {
	case errors.Is(err, backend.ErrUnsupportedQuery), errors.Is(err, backend.ErrUnsupportedMetadata):
		status = http.StatusUnprocessableEntity
		message = "query is not supported by the configured backend"
	case errors.Is(err, backend.ErrResponseLimit):
		status = http.StatusUnprocessableEntity
		message = "query result exceeds configured limits"
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusServiceUnavailable
		errorType = "timeout"
		message = "backend query timed out"
	}
	if errors.Is(err, backend.ErrUnsupportedQuery) || errors.Is(err, backend.ErrUnsupportedMetadata) {
		s.logger.Warn("backend rejected unsupported query")
	} else {
		s.logger.Error("backend query failed", "error", err)
	}
	writeError(response, status, errorType, message)
}

type responseEnvelope struct {
	Status    string   `json:"status"`
	Data      any      `json:"data,omitempty"`
	ErrorType string   `json:"errorType,omitempty"`
	Error     string   `json:"error,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type prometheusData struct {
	Result     any    `json:"result"`
	ResultType string `json:"resultType"`
}

type vectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  samplePair        `json:"value"`
}

type matrixSeries struct {
	Metric map[string]string `json:"metric"`
	Values []samplePair      `json:"values"`
}

type samplePair [2]any

func instantResult(series []backend.Series) []vectorSample {
	result := make([]vectorSample, 0, len(series))
	for _, item := range series {
		if len(item.Points) == 0 {
			continue
		}
		point := item.Points[len(item.Points)-1]
		result = append(result, vectorSample{
			Metric: item.Labels,
			Value:  prometheusSample(point),
		})
	}
	return result
}

func rangeResult(series []backend.Series) []matrixSeries {
	result := make([]matrixSeries, 0, len(series))
	for _, item := range series {
		values := make([]samplePair, 0, len(item.Points))
		for _, point := range item.Points {
			values = append(values, prometheusSample(point))
		}
		if len(values) > 0 {
			result = append(result, matrixSeries{Metric: item.Labels, Values: values})
		}
	}
	return result
}

func prometheusSample(point backend.Point) samplePair {
	return samplePair{float64(point.Timestamp.UnixMilli()) / 1000, prometheusFloat(point.Value)}
}

func prometheusFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
}

func simpleScalar(expression string) (float64, bool) {
	expression = strings.TrimSpace(expression)
	if value, err := strconv.ParseFloat(expression, 64); err == nil {
		return value, true
	}
	for index := 1; index < len(expression)-1; index++ {
		operator := expression[index]
		if !strings.ContainsRune("+-*/%^", rune(operator)) {
			continue
		}
		if (operator == '+' || operator == '-') && (expression[index-1] == 'e' || expression[index-1] == 'E') {
			continue
		}
		left, leftErr := strconv.ParseFloat(strings.TrimSpace(expression[:index]), 64)
		right, rightErr := strconv.ParseFloat(strings.TrimSpace(expression[index+1:]), 64)
		if leftErr != nil || rightErr != nil {
			continue
		}
		return scalarOperation(left, right, operator)
	}
	return 0, false
}

func scalarOperation(left float64, right float64, operator byte) (float64, bool) {
	switch operator {
	case '+':
		return left + right, true
	case '-':
		return left - right, true
	case '*':
		return left * right, true
	case '/':
		return left / right, true
	case '%':
		return math.Mod(left, right), true
	case '^':
		return math.Pow(left, right), true
	default:
		return 0, false
	}
}

func writeSuccess(response http.ResponseWriter, data any, warnings []string) {
	writeJSON(response, http.StatusOK, responseEnvelope{Status: "success", Data: data, Warnings: warnings})
}

func writeError(response http.ResponseWriter, status int, errorType string, message string) {
	writeJSON(response, status, responseEnvelope{Status: "error", ErrorType: errorType, Error: message})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func parseRequiredTime(raw string, name string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, fmt.Errorf("missing %s parameter", name)
	}
	parsed, err := parseTime(raw, time.Time{})
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s parameter: %w", name, err)
	}
	return parsed, nil
}

func parseTime(raw string, fallback time.Time) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err == nil {
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) ||
			seconds < -float64(uint64(1)<<63) || seconds >= float64(uint64(1)<<63) {
			return time.Time{}, fmt.Errorf("must be a finite Unix timestamp")
		}
		whole, fraction := math.Modf(seconds)
		return time.Unix(int64(whole), int64(fraction*float64(time.Second))), nil
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, raw)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("must be a Unix timestamp or RFC3339 time")
	}
	return parsed, nil
}

func parseStep(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("missing step parameter")
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err == nil {
		maxDurationSeconds := float64(time.Duration(1<<63-1)) / float64(time.Second)
		if seconds <= 0 || seconds > maxDurationSeconds || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return 0, fmt.Errorf("step must be greater than zero")
		}
		return time.Duration(seconds * float64(time.Second)), nil
	}
	step, parseErr := time.ParseDuration(raw)
	if parseErr != nil || step <= 0 {
		return 0, fmt.Errorf("invalid step parameter")
	}
	return step, nil
}

func splitRangeSelector(expression string) (string, time.Duration, bool, error) {
	expression = strings.TrimSpace(expression)
	if !strings.HasSuffix(expression, "]") {
		return expression, 0, false, nil
	}
	open := strings.LastIndexByte(expression, '[')
	if open < 1 {
		return expression, 0, false, nil
	}
	selector := strings.TrimSpace(expression[:open])
	if !isMetricSelector(selector) {
		return expression, 0, false, nil
	}
	window, err := parsePrometheusDuration(expression[open+1 : len(expression)-1])
	if err != nil {
		return "", 0, false, err
	}
	return selector, window, true, nil
}

func isMetricSelector(expression string) bool {
	if expression == "" || !isMetricNameStart(expression[0]) {
		return false
	}
	index := 1
	for index < len(expression) && isMetricNameCharacter(expression[index]) {
		index++
	}
	if index == len(expression) {
		return true
	}
	return expression[index] == '{' && strings.HasSuffix(expression, "}")
}

func isMetricNameStart(character byte) bool {
	return character == '_' || character == ':' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
}

func isMetricNameCharacter(character byte) bool {
	return isMetricNameStart(character) || character >= '0' && character <= '9'
}

func parsePrometheusDuration(raw string) (time.Duration, error) {
	remaining := raw
	var total time.Duration
	for remaining != "" {
		digits := 0
		for digits < len(remaining) && remaining[digits] >= '0' && remaining[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("invalid Prometheus duration %q", raw)
		}
		value, err := strconv.ParseInt(remaining[:digits], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid Prometheus duration %q", raw)
		}
		remaining = remaining[digits:]
		unit, consumed, ok := prometheusDurationUnit(remaining)
		if !ok || time.Duration(value) > (time.Duration(1<<63-1)-total)/unit {
			return 0, fmt.Errorf("invalid Prometheus duration %q", raw)
		}
		total += time.Duration(value) * unit
		remaining = remaining[consumed:]
	}
	if total <= 0 {
		return 0, fmt.Errorf("prometheus duration must be greater than zero")
	}
	return total, nil
}

func prometheusDurationUnit(raw string) (time.Duration, int, bool) {
	if strings.HasPrefix(raw, "ms") {
		return time.Millisecond, 2, true
	}
	if raw == "" {
		return 0, 0, false
	}
	switch raw[0] {
	case 'y':
		return 365 * 24 * time.Hour, 1, true
	case 'w':
		return 7 * 24 * time.Hour, 1, true
	case 'd':
		return 24 * time.Hour, 1, true
	case 'h':
		return time.Hour, 1, true
	case 'm':
		return time.Minute, 1, true
	case 's':
		return time.Second, 1, true
	default:
		return 0, 0, false
	}
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(body)
}

func (s *Server) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Body != nil {
			request.Body = http.MaxBytesReader(response, request.Body, s.options.MaxRequestBodyBytes)
		}
		recorder := &responseRecorder{ResponseWriter: response}
		started := time.Now()
		next.ServeHTTP(recorder, request)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		route := request.Pattern
		if route == "" {
			route = "unmatched"
		}
		duration := time.Since(started)
		s.observer.HTTPRequest(context.WithoutCancel(request.Context()), request.Method, route, status, duration)
		level := slog.LevelInfo
		if strings.HasPrefix(request.URL.Path, "/-/") {
			level = slog.LevelDebug
		}
		s.logger.Log(
			context.WithoutCancel(request.Context()),
			level,
			"request",
			"method", request.Method,
			"path", request.URL.Path,
			"status", status,
			"duration", duration,
		)
	})
}
