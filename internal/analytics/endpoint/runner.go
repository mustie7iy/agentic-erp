package endpoint

import (
	"context"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
)

// Runner answers a saved report's queries as runAnalyticsQuery answers
// them for the caller in ctx (ADR-0076 §2): under the active
// configuration version's model, which it returns beside it, over every
// company the caller may read, on current data.
func (e *Endpoints) Runner(ctx context.Context) (analytics.Runner, *analytics.Model, error) {
	m, version, err := e.model(ctx, 0)
	if err != nil {
		return nil, nil, err
	}
	return caller{e: e, m: m, version: version}, m, nil
}

// caller is a Runner of the request's caller under one model.
type caller struct {
	e       *Endpoints
	m       *analytics.Model
	version int32
}

func (c caller) Query(ctx context.Context, q analytics.Query) (*analytics.Result, error) {
	res, err := c.e.answer(ctx, c.m, q, nil, time.Time{})
	if err != nil {
		return nil, err
	}
	res.ConfigVersion = c.version
	return res, nil
}

// ResultToAPI is a result as the contract writes it (AnalyticsResult).
func ResultToAPI(r *analytics.Result) gen.AnalyticsResult { return toAPI(r) }
