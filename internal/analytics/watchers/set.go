package watchers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// NewWatcher is a watcher as its creator sets it.
type NewWatcher struct {
	Name      string
	Metric    string
	Filters   []analytics.Filter
	Company   string
	Grain     string
	Condition Condition
}

// conditionSettings checks that the condition carries exactly its kind's
// settings (metric_watcher_condition_whole), naming the field.
func conditionSettings(c Condition, money bool) error {
	invalid := func(field, message string) error {
		return problem.Invalid("analytics.invalid_watcher", "condition."+field, message)
	}
	compares := c.Kind == Above || c.Kind == Below
	if err := periodSettings(c); err != nil {
		return err
	}
	switch {
	case !slices.Contains(kinds, c.Kind):
		return invalid("kind", "The condition is one of "+strings.Join(kinds, ", ")+".")
	case compares && c.Value == "":
		return invalid("value", "An above or below condition compares with a value.")
	case !compares && c.Value != "":
		return invalid("value", "Only an above or below condition compares with a value.")
	case compares && money && c.Currency == "":
		return invalid("currency", "The value of a money metric is in a currency.")
	case c.Currency != "" && (!compares || !money):
		return invalid("currency", "Only the value of a money metric is in a currency.")
	case !compares && c.Percent == "":
		return invalid("percent", "A "+c.Kind+" condition compares with a percentage.")
	case compares && c.Percent != "":
		return invalid("percent", "An above or below condition takes no percentage.")
	}
	return nil
}

// periodSettings checks that a deviation, and only a deviation, names how
// many periods it averages, 1 to 12.
func periodSettings(c Condition) error {
	switch {
	case c.Kind == Deviation && (c.Periods < 1 || c.Periods > 12):
		return problem.Invalid("analytics.invalid_watcher", "condition.periods", "A deviation averages 1 to 12 previous periods.")
	case c.Kind != Deviation && c.Periods != 0:
		return problem.Invalid("analytics.invalid_watcher", "condition.periods", "Only a deviation averages previous periods.")
	}
	return nil
}

// kinds are the conditions a watcher may have (ADR-0077 §1).
var kinds = []string{Above, Below, Change, Deviation}

// check refuses a watcher the model cannot evaluate: an unknown grain, a
// condition without exactly its settings, and a query the semantic layer
// refuses (422 analytics.invalid_query), as the query it runs is checked.
func check(m *analytics.Model, in NewWatcher, now int) error {
	if err := problem.RequireText("name", in.Name); err != nil {
		return err
	}
	if in.Grain != Month && in.Grain != Year {
		return problem.Invalid("analytics.invalid_watcher", "grain", "The grain is month or year.")
	}
	met := m.Metric(in.Metric)
	if met == nil {
		return problem.Unprocessable("analytics.invalid_query", fmt.Sprintf("unknown metric %q (use list_metrics)", in.Metric))
	}
	if err := conditionSettings(in.Condition, met.Type == analytics.Money); err != nil {
		return err
	}
	w := Watcher{Metric: in.Metric, Filters: in.Filters, Grain: in.Grain}
	if err := analytics.Validate(m, w.query(m, now)); err != nil {
		return problem.Unprocessable("analytics.invalid_query", err.Error())
	}
	return nil
}

// Set sets a watcher of the caller's (ADR-0077 §1, T96a), active, with
// its audit record, after checking it under the active model.
func (e *Endpoints) Set(ctx context.Context, in NewWatcher) (Watcher, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Watcher{}, err
	}
	m, err := e.Queries.ActiveModel(ctx)
	if err != nil {
		return Watcher{}, err
	}
	if err := check(m, in, clock.Now(e.Now).Year()); err != nil {
		return Watcher{}, err
	}
	filters, _ := json.Marshal(append([]analytics.Filter{}, in.Filters...)) // plain data: always encodes
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO analytics.metric_watcher (tenant_id, id, name, metric, filters, company, grain, condition, threshold,
			currency, percent, periods, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, nullif($9, ''), nullif($10, ''), nullif($11, ''), nullif($12, 0), $13, $13)`,
		p.TenantID, id, in.Name, in.Metric, filters, in.Company, in.Grain, in.Condition.Kind, in.Condition.Value, in.Condition.Currency,
		in.Condition.Percent, in.Condition.Periods, p.ID); err != nil {
		return Watcher{}, fmt.Errorf("analytics: set metric watcher %s: %w", in.Name, err)
	}
	w, err := watcher(ctx, tx, p.ID, id, false)
	if err != nil {
		return Watcher{}, err
	}
	return w, recordWatcher(ctx, w, "create", map[string]any{"name": w.Name, "metric": w.Metric, "company": w.Company,
		"grain": w.Grain, "condition": w.Condition.Kind})
}

// Remove removes one of the caller's active watchers for good, at the
// caller's version of it; it and its alerts are kept.
func Remove(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Watcher, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Watcher{}, err
	}
	w, err := watcher(ctx, tx, p.ID, id, true)
	if err != nil {
		return Watcher{}, err
	}
	if err := ifMatch(w.Version); err != nil {
		return Watcher{}, err
	}
	if w.Status != Active {
		return Watcher{}, errInactive(w)
	}
	return setStatus(ctx, tx, p, w, Removed, "", "")
}

// setStatus ends an active watcher, removed or failed with why, with its
// audit record; the watcher row is locked by the caller.
func setStatus(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w Watcher, status, code, detail string) (Watcher, error) {
	if _, err := tx.Exec(ctx, `UPDATE analytics.metric_watcher SET status = $2, failure_code = nullif($3, ''), failure_detail = nullif($4, ''),
			version = version + 1, updated_by = $5 WHERE id = $1 AND status = 'active'`, w.ID, status, code, detail, p.ID); err != nil {
		return Watcher{}, fmt.Errorf("analytics: set metric watcher %s %s: %w", w.ID, status, err)
	}
	after, err := watcher(ctx, tx, w.CreatedBy, w.ID, false)
	if err != nil {
		return Watcher{}, err
	}
	action := map[string]string{Removed: "remove", Failed: "fail"}[status]
	return after, recordWatcher(ctx, after, action, map[string]any{"name": after.Name, "metric": after.Metric, "failure_code": code})
}

// recordWatcher adds the audit record of a watcher's change (ADR-0011).
// No event: watchers are their creator's own, and an alert is what others
// hear of (MetricAlertRaised).
func recordWatcher(ctx context.Context, w Watcher, action string, detail map[string]any) error {
	if err := audit.Record(ctx, "analytics.metric_watcher."+action, "analytics.metric_watcher", w.ID, detail); err != nil {
		return fmt.Errorf("analytics: record %s of metric watcher %s: %w", action, w.ID, err)
	}
	return nil
}
