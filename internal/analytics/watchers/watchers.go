// Package watchers keeps analysts' metric watchers and the alerts they
// raise (ADR-0077; 03-data-model §3.23): a watcher names one governed
// metric, its filters, one company or all the creator may read, a period
// grain and a condition; an evaluation runs the semantic layer's query as
// the watcher's creator (ADR-0044) and raises one alert per watcher and
// period whose condition holds. A watcher is its creator's: read,
// evaluated and removed by them alone (ADR-0077 §1). Removing it keeps it
// (04 §4.3: no DELETE).
//
// The package is part of the analytics context. It runs queries through
// its Queries port, the semantic layer's endpoint, by the analytics
// package's types, and names principals and companies through its People
// and Companies ports.
package watchers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Queries answers a watcher's query as the caller in ctx (the semantic
// layer's endpoint, endpoint.Endpoints): under the active model, over the
// companies asked for within the caller's (nil: all of those).
type Queries interface {
	ActiveModel(ctx context.Context) (*analytics.Model, error)
	Answer(ctx context.Context, m *analytics.Model, q analytics.Query, companies []string) (*analytics.Result, error)
}

// People names principals (identity context), keyed by id; an id the
// tenant has no principal of is absent.
type People interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Companies names legal entities by their company codes (tenancy), which
// an alert's companies are: who may see an alert (ADR-0077 §5).
type Companies interface {
	CompanyCode(ctx context.Context, id uuid.UUID) (code string, ok bool, err error)
}

// Endpoints implements the metric watchers' part of the generated API.
type Endpoints struct {
	Pages     *pagination.Codec
	Queries   Queries
	People    People
	Companies Companies
	// Now dates the evaluations: the fiscal years a query reads from
	// (ADR-0077 §2); nil: the wall clock.
	Now func() time.Time
}

// The statuses of a watcher.
const (
	Active  = "active"
	Removed = "removed"
	Failed  = "failed"
)

// The grains and conditions of a watcher (ADR-0077 §1).
const (
	Month     = "month"
	Year      = "year"
	Above     = "above"
	Below     = "below"
	Change    = "change"
	Deviation = "deviation"
)

// Condition is a watcher's condition: a Value (with the Currency of a
// money metric) for above and below, a Percent for change and deviation,
// and the Periods a deviation averages.
type Condition struct {
	Kind     string
	Value    string
	Currency string
	Percent  string
	Periods  int32
}

// Watcher is a row of analytics.metric_watcher.
type Watcher struct {
	ID            uuid.UUID
	Name          string
	Metric        string
	Filters       []analytics.Filter
	Company       string // "": every company the creator may read
	Grain         string
	Condition     Condition
	Status        string
	FailureCode   string
	FailureDetail string
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
}

const watcherSelect = `SELECT id, name, metric, filters, coalesce(company, ''), grain, condition, coalesce(threshold, ''),
	coalesce(currency, ''), coalesce(percent, ''), coalesce(periods, 0), status, coalesce(failure_code, ''), coalesce(failure_detail, ''),
	version, created_at, created_by, updated_at, updated_by FROM analytics.metric_watcher`

func scanWatcher(row pgx.Row) (Watcher, error) {
	var (
		w       Watcher
		filters []byte
	)
	if err := row.Scan(&w.ID, &w.Name, &w.Metric, &filters, &w.Company, &w.Grain, &w.Condition.Kind, &w.Condition.Value,
		&w.Condition.Currency, &w.Condition.Percent, &w.Condition.Periods, &w.Status, &w.FailureCode, &w.FailureDetail,
		&w.Version, &w.CreatedAt, &w.CreatedBy, &w.UpdatedAt, &w.UpdatedBy); err != nil {
		return Watcher{}, errs.Wrapf(err, "scan metric watcher")
	}
	// The column holds what Set wrote from this type, and the database
	// keeps it an array (metric_watcher_filters_check).
	if err := json.Unmarshal(filters, &w.Filters); err != nil {
		return Watcher{}, fmt.Errorf("metric watcher %s: filters: %w", w.ID, err)
	}
	return w, nil
}

// watcher reads one of the creator's watchers, locked for an evaluation
// or a change when forUpdate; another's is not found, as one that does
// not exist.
func watcher(ctx context.Context, tx pgx.Tx, creator, id uuid.UUID, forUpdate bool) (Watcher, error) {
	q := watcherSelect + ` WHERE id = $1 AND created_by = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	w, err := scanWatcher(tx.QueryRow(ctx, q, id, creator))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Watcher{}, problem.NotFoundOf("metric watcher")
	case err != nil:
		return Watcher{}, fmt.Errorf("analytics: read metric watcher %s: %w", id, err)
	}
	return w, nil
}

// errInactive refuses evaluating or removing a watcher that is no longer
// active.
func errInactive(w Watcher) *problem.Problem {
	return problem.Conflict("analytics.watcher_inactive", "Metric watcher "+w.Name+" is "+w.Status+": it is not evaluated and changes no more.")
}
