package signoz

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/simonepri/prometheus-api-bridge/bridge/backend"
)

// SigNoz compiles supported PromQL shapes into single ClickHouse statements
// only on its range endpoint; its instant endpoint always evaluates in the Go
// engine over raw samples. A range query whose start equals its end evaluates
// the expression once, at that instant, exactly as an instant query does, so
// instant queries that yield a vector or a scalar run on the range endpoint.

// minimumInstantStep is the step used when the expression has no range window.
const minimumInstantStep = time.Second

var promQLParser = parser.NewParser(parser.Options{})

// instantPlan describes how to run an instant query on the range endpoint.
type instantPlan struct {
	resultType backend.ResultType
	step       time.Duration
}

// planInstant reports whether an instant query can run on the range endpoint.
// The step only matters to SigNoz's translation, which accepts windows that
// equal the step or are shorter than it, so it is the longest window, rounded
// up to the whole seconds the translation requires.
func planInstant(expression string) (instantPlan, bool) {
	expr, err := promQLParser.ParseExpr(expression)
	if err != nil {
		return instantPlan{}, false
	}
	plan := instantPlan{step: minimumInstantStep}
	switch expr.Type() {
	case parser.ValueTypeVector:
		plan.resultType = backend.ResultTypeVector
	case parser.ValueTypeScalar:
		plan.resultType = backend.ResultTypeScalar
	case parser.ValueTypeMatrix, parser.ValueTypeString, parser.ValueTypeNone:
		// The range endpoint rejects range vectors and strings.
		return instantPlan{}, false
	default:
		return instantPlan{}, false
	}
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		switch typed := node.(type) {
		case *parser.MatrixSelector:
			plan.step = max(plan.step, typed.Range)
		case *parser.SubqueryExpr:
			plan.step = max(plan.step, typed.Range)
		}
		return nil
	})
	plan.step = (plan.step + time.Second - 1).Truncate(time.Second)
	return plan, true
}

// queryInstantAsRange evaluates an instant query as a one-point range query
// and returns the result in the shape the instant endpoint would return.
func (c *Client) queryInstantAsRange(ctx context.Context, query backend.Query, plan instantPlan) (backend.Result, error) {
	request, err := c.queryRequest(ctx, backend.Query{
		Expression: query.Expression,
		Start:      query.End,
		End:        query.End,
		Step:       plan.step,
		RangeQuery: true,
	})
	if err != nil {
		return backend.Result{}, err
	}
	result, err := c.execute(ctx, request)
	if err != nil {
		return backend.Result{}, err
	}
	if result.Type != backend.ResultTypeMatrix {
		return backend.Result{}, fmt.Errorf("SigNoz returned %q for a single-point range query", result.Type)
	}
	for _, series := range result.Series {
		if len(series.Points) != 1 {
			return backend.Result{}, fmt.Errorf("SigNoz returned %d points for a single-point range query", len(series.Points))
		}
	}
	result.Type = plan.resultType
	if plan.resultType == backend.ResultTypeScalar {
		if len(result.Series) != 1 {
			return backend.Result{}, fmt.Errorf("SigNoz returned %d series for a scalar expression", len(result.Series))
		}
		point := result.Series[0].Points[0]
		result.Scalar = &point
		result.Series = nil
	}
	return result, nil
}
