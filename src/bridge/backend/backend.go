// Package backend defines the query contract implemented by observability adapters.
package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// DefaultMaxResponseBytes bounds an upstream response before JSON decoding.
	DefaultMaxResponseBytes int64 = 4 << 20
	// DefaultMaxSeries bounds normalized query and discovery results.
	DefaultMaxSeries = 5_000
	// DefaultMaxSamples bounds the total samples in a normalized query result.
	DefaultMaxSamples = 100_000
)

var (
	// ErrUnsupportedQuery marks a PromQL expression that a bounded backend adapter cannot translate.
	ErrUnsupportedQuery = errors.New("unsupported query")
	// ErrUnsupportedMetadata marks a backend that cannot enumerate Prometheus series metadata.
	ErrUnsupportedMetadata = errors.New("unsupported metadata query")
	// ErrResponseLimit marks an upstream response or normalized result that exceeds a configured bound.
	ErrResponseLimit = errors.New("backend response exceeds configured limit")
	// ErrRedirect marks a backend redirect rejected before credential headers can be forwarded.
	ErrRedirect = errors.New("backend redirects are disabled")
)

// RejectRedirect prevents backend credentials from being copied to a redirect target.
func RejectRedirect(_ *http.Request, _ []*http.Request) error {
	return ErrRedirect
}

// Query is the backend-independent form of a Prometheus instant or range query.
type Query struct {
	Expression string
	Start      time.Time
	End        time.Time
	Step       time.Duration
	// RangeQuery distinguishes Prometheus /query_range evaluation from the
	// lookback window used to answer an instant query.
	RangeQuery bool
}

// Point is one sample in a time series.
type Point struct {
	Timestamp time.Time
	Value     float64
}

// Series is one labeled time series returned by a backend.
type Series struct {
	Labels map[string]string
	Points []Point
}

// ResultType is the Prometheus value type returned by a backend query.
type ResultType string

const (
	// ResultTypeMatrix contains a set of labeled range vectors.
	ResultTypeMatrix ResultType = "matrix"
	// ResultTypeScalar contains one timestamped scalar value.
	ResultTypeScalar ResultType = "scalar"
	// ResultTypeVector contains a set of labeled instant vectors.
	ResultTypeVector ResultType = "vector"
)

// Result is the normalized backend response consumed by the Prometheus API.
type Result struct {
	Type     ResultType
	Series   []Series
	Scalar   *Point
	Warnings []string
}

// ResultBudget enforces normalized series and sample limits while a backend
// adapter incrementally decodes its response.
type ResultBudget struct {
	maxSeries  int
	maxSamples int
	series     int
	samples    int
}

// NewResultBudget creates a budget using the shared defaults for non-positive limits.
func NewResultBudget(maxSeries int, maxSamples int) *ResultBudget {
	if maxSeries <= 0 {
		maxSeries = DefaultMaxSeries
	}
	if maxSamples <= 0 {
		maxSamples = DefaultMaxSamples
	}
	return &ResultBudget{maxSeries: maxSeries, maxSamples: maxSamples}
}

// AddSeries reserves capacity for one decoded source series.
func (b *ResultBudget) AddSeries() error {
	if b.series >= b.maxSeries {
		return fmt.Errorf("%w: result contains more than %d series", ErrResponseLimit, b.maxSeries)
	}
	b.series++
	return nil
}

// AddSample reserves capacity for one decoded sample.
func (b *ResultBudget) AddSample() error {
	if b.samples >= b.maxSamples {
		return fmt.Errorf("%w: result contains more than %d samples", ErrResponseLimit, b.maxSamples)
	}
	b.samples++
	return nil
}

// SeriesQuery requests the label sets matching one or more Prometheus selectors.
type SeriesQuery struct {
	Matchers []string
	Start    time.Time
	End      time.Time
}

// SeriesResult contains unique Prometheus label sets and backend warnings.
type SeriesResult struct {
	Series   []map[string]string
	Warnings []string
}

// Querier executes PromQL-compatible queries against one observability backend.
type Querier interface {
	QueryRange(context.Context, Query) (Result, error)
}

// SeriesQuerier is implemented by backends that support Prometheus metadata discovery.
type SeriesQuerier interface {
	QuerySeries(context.Context, SeriesQuery) (SeriesResult, error)
}
