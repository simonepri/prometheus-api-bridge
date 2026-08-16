// Package signoz adapts SigNoz's native Prometheus query endpoints.
package signoz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/simonepri/prometheus-api-bridge/bridge/backend"
)

const (
	queryPath        = "/api/v1/query"
	queryRangePath   = "/api/v1/query_range"
	discoverySamples = 240
	responseStatusOK = "success"
)

// Config configures the SigNoz Prometheus API adapter.
type Config struct {
	URL              string
	APIKey           string
	MaxResponseBytes int64
	MaxSeries        int
	MaxSamples       int
}

// Client queries SigNoz through the PromQL engine exposed by its Prometheus API.
type Client struct {
	queryEndpoint      *url.URL
	queryRangeEndpoint *url.URL
	apiKey             string
	httpClient         *http.Client
	maxResponseBytes   int64
	maxSeries          int
	maxSamples         int
}

// New creates a SigNoz backend client.
func New(config Config, httpClient *http.Client) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(config.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse SigNoz URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("SigNoz URL must use http or https")
	}
	if base.Host == "" {
		return nil, fmt.Errorf("SigNoz URL must include a host")
	}
	if base.User != nil {
		return nil, fmt.Errorf("SigNoz URL must not include user information")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("SigNoz API key is required")
	}
	// Own the client so every construction path rejects redirects before the
	// non-standard SIGNOZ-API-KEY header can be copied to another origin.
	client := http.Client{}
	if httpClient != nil {
		client = *httpClient
	}
	client.CheckRedirect = backend.RejectRedirect
	responseLimit := config.MaxResponseBytes
	if responseLimit <= 0 {
		responseLimit = backend.DefaultMaxResponseBytes
	}
	seriesLimit := config.MaxSeries
	if seriesLimit <= 0 {
		seriesLimit = backend.DefaultMaxSeries
	}
	sampleLimit := config.MaxSamples
	if sampleLimit <= 0 {
		sampleLimit = backend.DefaultMaxSamples
	}
	return &Client{
		queryEndpoint:      endpoint(base, queryPath),
		queryRangeEndpoint: endpoint(base, queryRangePath),
		apiKey:             config.APIKey,
		httpClient:         &client,
		maxResponseBytes:   responseLimit,
		maxSeries:          seriesLimit,
		maxSamples:         sampleLimit,
	}, nil
}

func endpoint(base *url.URL, suffix string) *url.URL {
	result := *base
	result.Path = strings.TrimRight(result.Path, "/") + suffix
	result.RawPath = ""
	result.RawQuery = ""
	result.Fragment = ""
	return &result
}

// QueryRange executes an instant or range PromQL query through SigNoz's native Prometheus API.
func (c *Client) QueryRange(ctx context.Context, query backend.Query) (backend.Result, error) {
	if query.Step <= 0 {
		return backend.Result{}, fmt.Errorf("%w: query step must be positive", backend.ErrUnsupportedQuery)
	}
	request, err := c.queryRequest(ctx, query)
	if err != nil {
		return backend.Result{}, err
	}
	return c.execute(ctx, request)
}

// QuerySeries discovers label sets by evaluating bounded selectors over the requested window.
func (c *Client) QuerySeries(ctx context.Context, query backend.SeriesQuery) (backend.SeriesResult, error) {
	matchers := query.Matchers
	if len(matchers) == 0 {
		matchers = []string{`{__name__!=""}`}
	}
	step := max(time.Millisecond, query.End.Sub(query.Start)/discoverySamples)
	unique := make(map[string]map[string]string)
	result := backend.SeriesResult{}
	for _, matcher := range matchers {
		queried, err := c.QueryRange(ctx, backend.Query{
			Expression: matcher,
			Start:      query.Start,
			End:        query.End,
			Step:       step,
			RangeQuery: true,
		})
		if err != nil {
			return backend.SeriesResult{}, err
		}
		result.Warnings = append(result.Warnings, queried.Warnings...)
		for _, series := range queried.Series {
			key, err := json.Marshal(series.Labels)
			if err != nil {
				return backend.SeriesResult{}, fmt.Errorf("encode discovered label set: %w", err)
			}
			unique[string(key)] = series.Labels
			if len(unique) > c.maxSeries {
				return backend.SeriesResult{}, fmt.Errorf(
					"%w: discovery returned more than %d series",
					backend.ErrResponseLimit,
					c.maxSeries,
				)
			}
		}
	}
	result.Series = make([]map[string]string, 0, len(unique))
	for _, labels := range unique {
		result.Series = append(result.Series, labels)
	}
	return result, nil
}

func (c *Client) queryRequest(ctx context.Context, query backend.Query) (*http.Request, error) {
	requestEndpoint := *c.queryEndpoint
	parameters := requestEndpoint.Query()
	parameters.Set("query", query.Expression)
	if query.RangeQuery {
		requestEndpoint = *c.queryRangeEndpoint
		parameters = requestEndpoint.Query()
		parameters.Set("query", query.Expression)
		parameters.Set("start", query.Start.UTC().Format(time.RFC3339Nano))
		parameters.Set("end", query.End.UTC().Format(time.RFC3339Nano))
		parameters.Set("step", strconv.FormatFloat(query.Step.Seconds(), 'g', -1, 64))
	} else {
		parameters.Set("time", query.End.UTC().Format(time.RFC3339Nano))
	}
	requestEndpoint.RawQuery = parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestEndpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create SigNoz request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("SIGNOZ-API-KEY", c.apiKey)
	return request, nil
}

func (c *Client) execute(_ context.Context, request *http.Request) (backend.Result, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return backend.Result{}, fmt.Errorf("query SigNoz: %w", sanitizeTransportError(err))
	}
	defer response.Body.Close()

	limited := &io.LimitedReader{R: response.Body, N: c.maxResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	result, status, err := decodeResponse(
		decoder,
		backend.NewResultBudget(c.maxSeries, c.maxSamples),
	)
	if err != nil {
		if errors.Is(err, backend.ErrResponseLimit) {
			return backend.Result{}, err
		}
		if limited.N == 0 {
			return backend.Result{}, fmt.Errorf(
				"%w: body is larger than %d bytes",
				backend.ErrResponseLimit,
				c.maxResponseBytes,
			)
		}
		return backend.Result{}, fmt.Errorf("decode SigNoz response with status %d: %w", response.StatusCode, err)
	}
	if err := requireEndOfResponse(decoder, limited, c.maxResponseBytes); err != nil {
		return backend.Result{}, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || status != responseStatusOK {
		return backend.Result{}, fmt.Errorf("SigNoz query failed with status %d", response.StatusCode)
	}
	return result, nil
}

func sanitizeTransportError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return &url.Error{
		Op:  urlErr.Op,
		URL: sanitizedRequestURL(urlErr.URL),
		Err: urlErr.Err,
	}
}

func sanitizedRequestURL(rawURL string) string {
	requestURL, err := url.Parse(rawURL)
	if err != nil {
		return "<redacted>"
	}
	requestURL.RawQuery = ""
	requestURL.ForceQuery = false
	requestURL.Fragment = ""
	return requestURL.String()
}

func requireEndOfResponse(decoder *json.Decoder, limited *io.LimitedReader, maximum int64) error {
	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if err == nil {
		return fmt.Errorf("SigNoz response contains multiple JSON values")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("finish decoding SigNoz response: %w", err)
	}
	if limited.N == 0 {
		return fmt.Errorf("%w: body is larger than %d bytes", backend.ErrResponseLimit, maximum)
	}
	return nil
}

type responseState struct {
	result     backend.Result
	status     string
	resultType string
}

func decodeResponse(
	decoder *json.Decoder,
	budget *backend.ResultBudget,
) (backend.Result, string, error) {
	state := responseState{}
	err := decodeObject(decoder, func(name string) error {
		switch name {
		case "status":
			return decoder.Decode(&state.status)
		case "warnings":
			return decoder.Decode(&state.result.Warnings)
		case "data":
			return decodeData(decoder, &state, budget)
		default:
			return skipValue(decoder)
		}
	})
	return state.result, state.status, err
}

func decodeData(decoder *json.Decoder, state *responseState, budget *backend.ResultBudget) error {
	var pendingResult json.RawMessage
	err := decodeObject(decoder, func(name string) error {
		switch name {
		case "resultType":
			if err := decoder.Decode(&state.resultType); err != nil {
				return err
			}
			state.result.Type = backend.ResultType(state.resultType)
			return nil
		case "result":
			if state.resultType == "" {
				return decoder.Decode(&pendingResult)
			}
			return decodeResult(decoder, state, budget)
		default:
			return skipValue(decoder)
		}
	})
	if err != nil {
		return err
	}
	if pendingResult == nil {
		return nil
	}
	return decodeResult(json.NewDecoder(bytes.NewReader(pendingResult)), state, budget)
}

func decodeResult(decoder *json.Decoder, state *responseState, budget *backend.ResultBudget) error {
	switch state.resultType {
	case string(backend.ResultTypeVector), string(backend.ResultTypeMatrix):
		return decodeSeries(decoder, state.resultType, &state.result, budget)
	case string(backend.ResultTypeScalar):
		var sample prometheusSample
		if err := decoder.Decode(&sample); err != nil {
			return err
		}
		if err := budget.AddSeries(); err != nil {
			return err
		}
		if err := budget.AddSample(); err != nil {
			return err
		}
		point := sample.Point()
		state.result.Scalar = &point
		return nil
	default:
		return fmt.Errorf("unsupported SigNoz Prometheus result type %q", state.resultType)
	}
}

type prometheusSeries struct {
	Metric map[string]string  `json:"metric"`
	Value  prometheusSample   `json:"value"`
	Values []prometheusSample `json:"values"`
}

func decodeSeries(
	decoder *json.Decoder,
	resultType string,
	result *backend.Result,
	budget *backend.ResultBudget,
) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	// SigNoz can encode an empty Prometheus result as null instead of [].
	// Normalize both representations to an empty result for API consumers.
	if token == nil {
		return nil
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '[' {
		return fmt.Errorf("expected JSON delimiter %q", '[')
	}
	for decoder.More() {
		if err := budget.AddSeries(); err != nil {
			return err
		}
		var source prometheusSeries
		if err := decoder.Decode(&source); err != nil {
			return err
		}
		points := source.Values
		if resultType == string(backend.ResultTypeVector) {
			if !source.Value.Valid {
				return fmt.Errorf("SigNoz vector series is missing value")
			}
			points = []prometheusSample{source.Value}
		}
		series := backend.Series{Labels: source.Metric, Points: make([]backend.Point, 0, len(points))}
		for _, sample := range points {
			if !sample.Valid {
				return fmt.Errorf("SigNoz matrix contains an invalid sample")
			}
			if err := budget.AddSample(); err != nil {
				return err
			}
			series.Points = append(series.Points, sample.Point())
		}
		if len(series.Points) > 0 {
			result.Series = append(result.Series, series)
		}
	}
	_, err = decoder.Token()
	return err
}

type prometheusSample struct {
	Timestamp float64
	Value     float64
	Valid     bool
}

func (s *prometheusSample) UnmarshalJSON(data []byte) error {
	var fields []json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 2 {
		return fmt.Errorf("prometheus sample must contain timestamp and value")
	}
	if err := json.Unmarshal(fields[0], &s.Timestamp); err != nil || math.IsNaN(s.Timestamp) || math.IsInf(s.Timestamp, 0) {
		return fmt.Errorf("invalid Prometheus sample timestamp")
	}
	var encoded string
	if err := json.Unmarshal(fields[1], &encoded); err != nil {
		encoded = string(bytes.TrimSpace(fields[1]))
	}
	value, err := strconv.ParseFloat(encoded, 64)
	if err != nil {
		return fmt.Errorf("invalid Prometheus sample value: %w", err)
	}
	s.Value = value
	s.Valid = true
	return nil
}

func (s prometheusSample) Point() backend.Point {
	seconds, fraction := math.Modf(s.Timestamp)
	return backend.Point{
		Timestamp: time.Unix(int64(seconds), int64(math.Round(fraction*float64(time.Second)))),
		Value:     s.Value,
	}
}

func decodeObject(decoder *json.Decoder, visit func(string) error) error {
	if err := expectDelimiter(decoder, '{'); err != nil {
		return err
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected JSON object key")
		}
		if err := visit(name); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func expectDelimiter(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != expected {
		return fmt.Errorf("expected JSON delimiter %q", expected)
	}
	return nil
}

func skipValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
