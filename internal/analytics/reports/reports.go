// Package reports keeps analysts' saved reports in erp-server (ADR-0076;
// 03-data-model §3.23): a report's dashboard specification and the
// semantic queries behind it, saved under its title through the API and
// run again on current data. A report is its owner's, read and run by
// them alone (ADR-0076 §2); a run executes each query as the caller,
// through the semantic layer's endpoint (ADR-0044), so it reads what the
// caller may read now. Removing a report keeps it (04 §4.3: no DELETE).
//
// The package is part of the analytics context. It reads principals'
// names through its People port, by types of its own.
package reports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Queries runs a report's queries as the caller (the semantic layer's
// endpoint, endpoint.Endpoints): a Runner under the active model, and
// that model.
type Queries interface {
	Runner(ctx context.Context) (analytics.Runner, *analytics.Model, error)
}

// People reads principals' names (identity context), keyed by id; an id
// the tenant has no principal of is absent.
type People interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Configs reads the active configuration (the configuration context's
// Settings): the companies whose time zones date scheduled runs.
type Configs interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the saved reports' part of the generated API.
type Endpoints struct {
	Pages   *pagination.Codec
	Queries Queries
	People  People
	Configs Configs
	// Objects keeps scheduled runs' results (ADR-0016); nil: reports are
	// not scheduled and kept runs not read (503).
	Objects blobstore.Store
	Now     func() time.Time
}

// The statuses of a report.
const (
	Active  = "active"
	Removed = "removed"
)

// Report is a row of analytics.report.
type Report struct {
	ID        uuid.UUID
	Title     string
	Question  string
	Spec      analytics.Spec
	Queries   map[string]analytics.Query
	OwnerID   uuid.UUID
	Status    string
	Schedule  *Schedule // nil: none
	Version   int32
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Schedule is a report's schedule (ADR-0076 §4).
type Schedule struct {
	Frequency string
	Company   string
	Weekday   *int32 // weekly: the ISO weekday
	Day       *int32 // monthly: the day of the month
	From      time.Time
}

// On says whether the schedule has a run due on the company's date day.
func (s Schedule) On(day time.Time) bool {
	if day.Before(s.From) {
		return false
	}
	switch s.Frequency {
	case "weekly":
		wd := int32(day.Weekday())
		if wd == 0 {
			wd = 7 // ISO: Sunday is 7
		}
		return wd == *s.Weekday
	case "monthly":
		return int32(day.Day()) == *s.Day
	}
	return true // daily
}

const reportSelect = `SELECT id, title, coalesce(question, ''), spec, queries, owner_id, status, version, created_at, updated_at,
	schedule_frequency, schedule_company, schedule_weekday, schedule_day, scheduled_from FROM analytics.report`

func scanReport(row pgx.Row) (Report, error) {
	var (
		r                  Report
		spec, queries      []byte
		frequency, company *string
		weekday, day       *int32
		from               *time.Time
	)
	if err := row.Scan(&r.ID, &r.Title, &r.Question, &spec, &queries, &r.OwnerID, &r.Status, &r.Version, &r.CreatedAt, &r.UpdatedAt,
		&frequency, &company, &weekday, &day, &from); err != nil {
		return Report{}, errs.Wrapf(err, "scan report")
	}
	if frequency != nil { // the database keeps a schedule whole (report_schedule_whole)
		r.Schedule = &Schedule{Frequency: *frequency, Company: *company, Weekday: weekday, Day: day, From: *from}
	}
	// The columns hold what save wrote from these types, and the database
	// keeps them objects (report_spec_check, report_queries_check).
	if err := json.Unmarshal(spec, &r.Spec); err != nil {
		return Report{}, fmt.Errorf("report %s: spec: %w", r.ID, err)
	}
	if err := json.Unmarshal(queries, &r.Queries); err != nil {
		return Report{}, fmt.Errorf("report %s: queries: %w", r.ID, err)
	}
	return r, nil
}

// report reads one of the owner's reports, locked for a change when
// forUpdate (FOR NO KEY UPDATE: a run being kept references the row,
// taking FOR KEY SHARE, which this lock lets through); another's is not
// found, as one that does not exist.
func report(ctx context.Context, tx pgx.Tx, owner, id uuid.UUID, forUpdate bool) (Report, error) {
	q := reportSelect + ` WHERE id = $1 AND owner_id = $2`
	if forUpdate {
		q += ` FOR NO KEY UPDATE`
	}
	r, err := scanReport(tx.QueryRow(ctx, q, id, owner))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Report{}, problem.NotFoundOf("report")
	case err != nil:
		return Report{}, fmt.Errorf("analytics: read report %s: %w", id, err)
	}
	return r, nil
}

// errRemoved refuses running or removing a removed report.
func errRemoved(title string) *problem.Problem {
	return problem.Conflict("analytics.report_removed", "Report "+title+" is removed: it is not run and changes no more.")
}

// queryID is what a report names a query by.
var queryID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// checkIDs refuses a query id the contract does not allow, naming it.
func checkIDs(queries map[string]analytics.Query) error {
	for id := range queries {
		if !queryID.MatchString(id) {
			return problem.Unprocessable("request.validation_failed", "A query's id is 1 to 100 letters, digits, '_', '-' or '.'.",
				problem.FieldError{Field: "queries", Code: "invalid", Message: "query id " + fmt.Sprintf("%q", id) + " is not 1 to 100 letters, digits, '_', '-' or '.'"})
		}
	}
	return nil
}

// metricRemoved refuses queries naming a metric the model lacks (422
// analytics.report_metric_removed naming it).
func metricRemoved(m *analytics.Model, queries map[string]analytics.Query) error {
	id, metric := analytics.RemovedMetric(m, queries)
	if metric == "" {
		return nil
	}
	return problem.Unprocessable("analytics.report_metric_removed", "Query "+id+" of the report names metric "+metric+
		", which the semantic model does not have; save the report again without it.")
}

// rerun runs the report's queries as the caller (Session.Rerun) under
// the active model: the specification as answered, with its results.
// A metric the model lacks is refused first, naming it; a specification
// the results do not support is 422 analytics.invalid_report naming each
// problem.
func (e *Endpoints) rerun(ctx context.Context, spec analytics.Spec, queries map[string]analytics.Query) (*analytics.Session, *analytics.Report, map[string]*analytics.Result, error) {
	runner, m, err := e.Queries.Runner(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := metricRemoved(m, queries); err != nil {
		return nil, nil, nil, err
	}
	sess := analytics.NewSession(m, nil, analytics.Principal{ID: "request"})
	sess.Runner = runner
	out, results, err := sess.Rerun(ctx, &analytics.Report{Spec: spec, Queries: queries})
	var invalid *analytics.SpecError
	if errors.As(err, &invalid) {
		return nil, nil, nil, invalidReport(invalid.Problems)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("analytics: run a report: %w", err)
	}
	return sess, out, results, nil
}

// invalidReport is a specification its results do not support.
func invalidReport(problems []string) *problem.Problem {
	fields := make([]problem.FieldError, len(problems))
	for i, p := range problems {
		fields[i] = problem.FieldError{Field: "spec", Code: "invalid", Message: p}
	}
	return problem.Unprocessable("analytics.invalid_report", "The report's specification is not supported by its queries' results.", fields...)
}
