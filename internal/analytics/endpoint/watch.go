package endpoint

import (
	"context"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
)

// ActiveModel is the active configuration version's semantic model, the
// one a metric watcher is set and evaluated under (ADR-0077 §1).
func (e *Endpoints) ActiveModel(ctx context.Context) (*analytics.Model, error) {
	m, _, err := e.model(ctx, 0)
	return m, err
}

// Answer answers a watcher's query under m as runAnalyticsQuery answers
// the caller in ctx, over the companies asked for within the caller's
// (nil: all of those), on current data (ADR-0077 §2).
func (e *Endpoints) Answer(ctx context.Context, m *analytics.Model, q analytics.Query, companies []string) (*analytics.Result, error) {
	return e.answer(ctx, m, q, companies, time.Time{})
}
