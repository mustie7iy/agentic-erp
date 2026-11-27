package watchers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Finding is a series that held the condition in an alert's period.
type Finding struct {
	Company      string `json:"company"`
	Currency     string `json:"currency,omitempty"`
	Figure       string `json:"figure"`
	ComparedWith string `json:"compared_with"`
	Percent      string `json:"percent,omitempty"`
}

// Alert is a row of analytics.metric_alert with what its watcher says.
type Alert struct {
	ID           uuid.UUID
	WatcherID    uuid.UUID
	WatcherName  string
	Metric       string
	Grain        string
	Condition    Condition
	FiscalYear   int32
	FiscalPeriod *int32 // nil for the year grain
	Companies    []string
	Findings     []Finding
	Status       string
	Version      int32
	RaisedAt     time.Time
	RaisedAs     uuid.UUID
	// AcknowledgedAt and AcknowledgedBy are nil while it is open.
	AcknowledgedAt *time.Time
	AcknowledgedBy *uuid.UUID
}

// alertSelect reads alerts with their watchers' names and conditions, as
// columns of one row source, so a list's keyset names them alone; its
// companies and permissions decide who sees one (visible).
const alertSelect = `SELECT id, watcher_id, watcher_name, metric, grain, condition, threshold, currency, percent, periods, fiscal_year,
	fiscal_period, companies, findings, status, version, raised_at, raised_as, acknowledged_at, acknowledged_by FROM (
	SELECT a.id, a.watcher_id, w.name AS watcher_name, w.metric, w.grain, w.condition, coalesce(w.threshold, '') AS threshold,
		coalesce(w.currency, '') AS currency, coalesce(w.percent, '') AS percent, coalesce(w.periods, 0) AS periods, a.fiscal_year,
		a.fiscal_period, a.companies, a.permissions, a.findings, a.status, a.version, a.created_at AS raised_at, a.created_by AS raised_as,
		a.acknowledged_at, a.acknowledged_by
	FROM analytics.metric_alert a JOIN analytics.metric_watcher w ON w.tenant_id = a.tenant_id AND w.id = a.watcher_id) x`

func scanAlert(row pgx.Row) (Alert, error) {
	var (
		a        Alert
		findings []byte
	)
	if err := row.Scan(&a.ID, &a.WatcherID, &a.WatcherName, &a.Metric, &a.Grain, &a.Condition.Kind, &a.Condition.Value, &a.Condition.Currency,
		&a.Condition.Percent, &a.Condition.Periods, &a.FiscalYear, &a.FiscalPeriod, &a.Companies, &findings, &a.Status, &a.Version, &a.RaisedAt,
		&a.RaisedAs, &a.AcknowledgedAt, &a.AcknowledgedBy); err != nil {
		return Alert{}, errs.Wrapf(err, "scan metric alert")
	}
	// The column holds what raise wrote from this type, and the database
	// keeps it an array (metric_alert_findings_check).
	if err := json.Unmarshal(findings, &a.Findings); err != nil {
		return Alert{}, fmt.Errorf("metric alert %s: findings: %w", a.ID, err)
	}
	return a, nil
}

// alertOf reads the watcher's alert of a period; nil when it raised none.
func alertOf(ctx context.Context, tx pgx.Tx, watcher uuid.UUID, at period) (*Alert, error) {
	a, err := scanAlert(tx.QueryRow(ctx, alertSelect+` WHERE watcher_id = $1 AND fiscal_year = $2 AND coalesce(fiscal_period, 0) = $3`,
		watcher, at.year, at.month))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("analytics: read the alert of metric watcher %s for %d/%d: %w", watcher, at.year, at.month, err)
	}
	return &a, nil
}

// Evaluation is what evaluating a watcher found: the watcher after it,
// the period judged (nil when the answer had no figure or the watcher
// failed), each series' figure, and the alert of the period, raised now or
// before.
type Evaluation struct {
	Watcher Watcher
	At      *period
	Series  []Judged
	Alert   *Alert
	Raised  bool
}

// Evaluate evaluates one of the caller's active watchers at the caller's
// version of it (ADR-0077 §2, §3), as the caller: a metric the model no
// longer has fails the watcher; otherwise its query is answered as the
// caller's would be, and a condition holding in a series raises the
// period's alert once. The watcher is locked meanwhile, so two
// evaluations of it run one after the other.
func (e *Endpoints) Evaluate(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Evaluation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Evaluation{}, err
	}
	w, err := watcher(ctx, tx, p.ID, id, true)
	if err != nil {
		return Evaluation{}, err
	}
	if err := ifMatch(w.Version); err != nil {
		return Evaluation{}, err
	}
	if w.Status != Active {
		return Evaluation{}, errInactive(w)
	}
	return e.evaluate(ctx, tx, p, w)
}

// evaluate evaluates a locked active watcher as the caller p, its
// creator, in the request's transaction.
func (e *Endpoints) evaluate(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w Watcher) (Evaluation, error) {
	m, err := e.Queries.ActiveModel(ctx)
	if err != nil {
		return Evaluation{}, err
	}
	if m.Metric(w.Metric) == nil {
		failed, err := setStatus(ctx, tx, p, w, Failed, "analytics.watcher_metric_removed",
			"The semantic model no longer has the metric "+w.Metric+"; the watcher is not evaluated again.")
		return Evaluation{Watcher: failed, Series: []Judged{}}, err
	}
	var companies []string
	if w.Company != "" {
		companies = []string{w.Company}
	}
	res, err := e.Queries.Answer(ctx, m, w.query(m, clock.Now(e.Now).Year()), companies)
	if err != nil {
		return Evaluation{}, fmt.Errorf("analytics: evaluate metric watcher %s: %w", w.ID, err)
	}
	if res.Truncated {
		return Evaluation{}, problem.Unprocessable("analytics.watcher_too_many_series",
			"The watcher's answer has more than 500 companies, currencies and periods; set it on one company, or filter it.")
	}
	f := w.figuresOf(res)
	at, ok := f.latest()
	if !ok {
		return Evaluation{Watcher: w, Series: []Judged{}}, nil
	}
	ev := Evaluation{Watcher: w, At: &at, Series: w.judge(f, at)}
	ev.Alert, ev.Raised, err = raise(ctx, tx, p, w, at, ev.Series, readPermissions(m, w))
	return ev, err
}

// readPermissions are the permissions the watcher's metric's data is read
// with, as the database path reads it (analytics.Source.Permissions), the
// centers' read too when its filters name a center's name or node
// (analytics.NamesCenters), sorted: an alert is shown only to a caller
// holding them (ADR-0077 §5).
func readPermissions(m *analytics.Model, w Watcher) []string {
	needed := map[string]bool{}
	for _, b := range m.BaseMetrics(w.Metric) {
		needed[m.Metric(b).Source] = true
	}
	datasets := []string{} // not nil: nil builds every dataset
	for _, f := range analytics.FactDatasets {
		if needed[f.Dataset] && !slices.Contains(datasets, f.Dataset) {
			datasets = append(datasets, f.Dataset)
		}
	}
	perms := analytics.Source{Datasets: datasets, Centers: analytics.NamesCenters(analytics.Query{Where: w.Filters})}.Permissions()
	slices.Sort(perms)
	return perms
}

// raise raises the watcher's alert of the period when a series holds the
// condition, once per watcher and period (metric_alert_once), with its
// audit record and MetricAlertRaised; otherwise, or when it was raised
// before, it answers the period's alert, nil for none.
func raise(ctx context.Context, tx pgx.Tx, p reqctx.Principal, w Watcher, at period, series []Judged, perms []string) (*Alert, bool, error) {
	findings, companies := []Finding{}, []string{}
	for _, j := range series {
		if j.Holds {
			findings = append(findings, Finding{Company: j.Company, Currency: j.Currency, Figure: j.Figure.String(), ComparedWith: j.ComparedWith, Percent: j.Percent})
			if !slices.Contains(companies, j.Company) {
				companies = append(companies, j.Company)
			}
		}
	}
	if len(findings) == 0 {
		a, err := alertOf(ctx, tx, w.ID, at)
		return a, false, err
	}
	b, _ := json.Marshal(findings) // plain data: always encodes
	tag, err := tx.Exec(ctx, `INSERT INTO analytics.metric_alert (tenant_id, id, watcher_id, fiscal_year, fiscal_period, companies, permissions, findings, created_by)
		VALUES ($1, $2, $3, $4, nullif($5, 0), $6, $7, $8, $9)
		ON CONFLICT (tenant_id, watcher_id, fiscal_year, (coalesce(fiscal_period, 0))) DO NOTHING`,
		p.TenantID, uuid.Must(uuid.NewV7()), w.ID, at.year, at.month, companies, perms, b, p.ID)
	if err != nil {
		return nil, false, fmt.Errorf("analytics: raise the alert of metric watcher %s: %w", w.ID, err)
	}
	a, err := alertOf(ctx, tx, w.ID, at)
	if err != nil || tag.RowsAffected() == 0 {
		return a, false, err
	}
	return a, true, raised(ctx, *a)
}

// raised adds an alert's audit record and publishes MetricAlertRaised in
// the same transaction (ADR-0007, ADR-0011; ADR-0077 §3).
func raised(ctx context.Context, a Alert) error {
	findings := make([]map[string]any, len(a.Findings))
	for i, f := range a.Findings {
		findings[i] = map[string]any{"company": f.Company, "currency": nullable(f.Currency), "figure": f.Figure,
			"compared_with": f.ComparedWith, "percent": nullable(f.Percent)}
	}
	var month any
	if a.FiscalPeriod != nil {
		month = *a.FiscalPeriod
	}
	payload := map[string]any{"metric_alert_id": a.ID, "watcher_id": a.WatcherID, "watcher_name": a.WatcherName, "metric": a.Metric,
		"condition": a.Condition.Kind, "fiscal_year": a.FiscalYear, "fiscal_period": month, "companies": a.Companies, "findings": findings,
		"raised_as": a.RaisedAs}
	if err := audit.Record(ctx, "analytics.metric_alert.raise", "analytics.metric_alert", a.ID,
		map[string]any{"watcher_id": a.WatcherID, "fiscal_year": a.FiscalYear, "fiscal_period": month, "companies": a.Companies},
		outbox.Event{Context: "analytics", Name: "metric_alert_raised", AggregateType: "analytics.metric_alert", AggregateID: a.ID, Payload: payload}); err != nil {
		return fmt.Errorf("analytics: record the alert %s: %w", a.ID, err)
	}
	return nil
}

// nullable is a text the event writes as null when it is empty.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
