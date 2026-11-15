package watchers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// viewer is who reads alerts (ADR-0077 §5): the codes of the companies
// they may access (nil: every company) and the permissions they may use.
type viewer struct {
	companies   []string
	permissions []string
}

// visible is the condition on alertSelect's rows a viewer sees, its
// arguments numbered from n: every company of the alert accessible, every
// permission of its data usable.
func (v viewer) visible(n int) (string, []any) {
	return fmt.Sprintf(` AND ($%d::text[] IS NULL OR companies <@ $%d) AND permissions <@ $%d`, n, n, n+1), []any{v.companies, v.permissions}
}

// viewer reads the caller's company access, as company codes, and the
// permissions they may use, those a segregation-of-duties rule bars left
// out (authz.Effective).
func (e *Endpoints) viewer(ctx context.Context) (viewer, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return viewer{}, err
	}
	usable, _, err := authz.Effective(ctx)
	if err != nil {
		return viewer{}, err
	}
	v := viewer{permissions: append([]string{}, usable...)}
	if access.All {
		return v, nil
	}
	v.companies = []string{} // not nil: nil is every company
	for _, id := range access.IDs {
		code, ok, err := e.Companies.CompanyCode(ctx, id)
		if err != nil {
			return viewer{}, fmt.Errorf("analytics: company of legal entity %s: %w", id, err)
		}
		if ok {
			v.companies = append(v.companies, code)
		}
	}
	return v, nil
}

// alert reads one alert the viewer sees; another is not found, as one
// that does not exist.
func alert(ctx context.Context, tx pgx.Tx, v viewer, id uuid.UUID) (Alert, error) {
	cond, args := v.visible(2)
	a, err := scanAlert(tx.QueryRow(ctx, alertSelect+` WHERE id = $1`+cond, append([]any{id}, args...)...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Alert{}, problem.NotFoundOf("metric alert")
	case err != nil:
		return Alert{}, fmt.Errorf("analytics: read metric alert %s: %w", id, err)
	}
	return a, nil
}

// GetAlert reads one alert the caller may see (ADR-0077 §5).
func (e *Endpoints) GetAlert(ctx context.Context, id uuid.UUID) (Alert, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Alert{}, err
	}
	v, err := e.viewer(ctx)
	if err != nil {
		return Alert{}, err
	}
	return alert(ctx, tx, v, id)
}

// Acknowledge moves an open alert the caller may see to acknowledged,
// once, at the caller's version of it, with its audit record (ADR-0077
// §5). The alert row alone is locked: an evaluation raising the next
// period's alert locks its watcher, never an alert, so the two never wait
// on each other in opposite orders.
func (e *Endpoints) Acknowledge(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Alert, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Alert{}, err
	}
	v, err := e.viewer(ctx)
	if err != nil {
		return Alert{}, err
	}
	if _, err := alert(ctx, tx, v, id); err != nil {
		return Alert{}, err
	}
	var status string
	var version int32
	if err := tx.QueryRow(ctx, `SELECT status, version FROM analytics.metric_alert WHERE id = $1 FOR UPDATE`, id).Scan(&status, &version); err != nil {
		return Alert{}, fmt.Errorf("analytics: lock metric alert %s: %w", id, err)
	}
	if err := ifMatch(version); err != nil {
		return Alert{}, err
	}
	if status != "open" {
		return Alert{}, problem.Conflict("analytics.alert_acknowledged", "The alert is acknowledged already: it is acknowledged once.")
	}
	if _, err := tx.Exec(ctx, `UPDATE analytics.metric_alert SET status = 'acknowledged', acknowledged_at = now(), acknowledged_by = $2,
			version = version + 1 WHERE id = $1 AND status = 'open'`, id, p.ID); err != nil {
		return Alert{}, fmt.Errorf("analytics: acknowledge metric alert %s: %w", id, err)
	}
	a, err := alert(ctx, tx, v, id)
	if err != nil {
		return Alert{}, err
	}
	if err := audit.Record(ctx, "analytics.metric_alert.acknowledge", "analytics.metric_alert", a.ID,
		map[string]any{"watcher_id": a.WatcherID, "fiscal_year": a.FiscalYear, "fiscal_period": a.FiscalPeriod}); err != nil {
		return Alert{}, fmt.Errorf("analytics: record the acknowledgement of metric alert %s: %w", a.ID, err)
	}
	return a, nil
}

// names reads the names of the alerts' creators and acknowledgers in one
// call.
func (e *Endpoints) names(ctx context.Context, alerts []Alert) (map[uuid.UUID]string, error) {
	ids := []uuid.UUID{}
	for _, a := range alerts {
		ids = append(ids, a.RaisedAs)
		if a.AcknowledgedBy != nil {
			ids = append(ids, *a.AcknowledgedBy)
		}
	}
	names, err := e.People.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("analytics: names of the people of %d metric alerts: %w", len(alerts), err)
	}
	return names, nil
}
