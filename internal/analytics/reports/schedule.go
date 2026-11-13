package reports

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// frequencies are how often a report may run (ADR-0076 §4).
var frequencies = []string{"daily", "weekly", "monthly"}

// ScheduleSet is a schedule as a client sets it.
type ScheduleSet struct {
	Frequency  string
	Company    string
	Weekday    *int32
	DayOfMonth *int32
}

// check refuses a schedule whose frequency the contract does not list,
// or that gives the weekday or day of the month where it does not apply
// or leaves it out where it does.
func (in ScheduleSet) check() error {
	invalid := func(field, message string) error {
		return problem.Unprocessable("request.validation_failed", "The schedule is not valid: "+message+".",
			problem.FieldError{Field: field, Code: "invalid", Message: message})
	}
	switch {
	case !slices.Contains(frequencies, in.Frequency):
		return invalid("frequency", "one of daily, weekly, monthly")
	case (in.Weekday != nil) != (in.Frequency == "weekly"):
		return invalid("weekday", "given for a weekly schedule alone")
	case (in.DayOfMonth != nil) != (in.Frequency == "monthly"):
		return invalid("day_of_month", "given for a monthly schedule alone")
	}
	return nil
}

// errNoObjects refuses what needs object storage on a server without it.
func errNoObjects() error {
	return problem.Unavailable("Scheduled runs of reports are kept in object storage, which this server does not have.", 0)
}

// SetSchedule gives one of the caller's active reports a schedule, or
// replaces its schedule, with the caller's version of it (ADR-0076 §4):
// its runs are due from the company's date today.
func (e *Endpoints) SetSchedule(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in ScheduleSet) (Report, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Report{}, err
	}
	if e.Objects == nil {
		return Report{}, errNoObjects()
	}
	r, err := locked(ctx, tx, p.ID, id, ifMatch)
	if err != nil {
		return Report{}, err
	}
	if err := in.check(); err != nil {
		return Report{}, err
	}
	day, found, err := e.companyDay(ctx, in.Company)
	if err != nil {
		return Report{}, fmt.Errorf("analytics: schedule report %s: %w", id, err)
	}
	if !found {
		return Report{}, problem.Invalid("analytics.unknown_company", "company", "The active configuration has no company "+in.Company+".")
	}
	if _, err := tx.Exec(ctx, `UPDATE analytics.report SET schedule_frequency = $2, schedule_company = $3, schedule_weekday = $4, schedule_day = $5,
			scheduled_from = $6, version = version + 1 WHERE id = $1 AND status = 'active'`,
		r.ID, in.Frequency, in.Company, in.Weekday, in.DayOfMonth, day.Date); err != nil {
		return Report{}, fmt.Errorf("analytics: schedule report %s: %w", id, err)
	}
	after, err := report(ctx, tx, p.ID, id, false)
	if err != nil {
		return Report{}, err
	}
	return after, record(ctx, "schedule", after, map[string]any{"frequency": in.Frequency, "company": in.Company, "weekday": in.Weekday,
		"day_of_month": in.DayOfMonth, "from": day.Date.Format("2006-01-02")})
}

// Unschedule ends the schedule of one of the caller's reports, with the
// caller's version of it; the runs kept stay.
func Unschedule(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Report, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Report{}, err
	}
	r, err := locked(ctx, tx, p.ID, id, ifMatch)
	if err != nil {
		return Report{}, err
	}
	if r.Schedule == nil {
		return Report{}, problem.Conflict("analytics.report_not_scheduled", "Report "+r.Title+" has no schedule to end.")
	}
	if _, err := tx.Exec(ctx, `UPDATE analytics.report SET schedule_frequency = NULL, schedule_company = NULL, schedule_weekday = NULL,
			schedule_day = NULL, scheduled_from = NULL, version = version + 1 WHERE id = $1`, id); err != nil {
		return Report{}, fmt.Errorf("analytics: unschedule report %s: %w", id, err)
	}
	after, err := report(ctx, tx, p.ID, id, false)
	if err != nil {
		return Report{}, err
	}
	return after, record(ctx, "unschedule", after, map[string]any{"title": after.Title})
}

// locked reads one of the owner's active reports locked for a change and
// compares the caller's version.
func locked(ctx context.Context, tx pgx.Tx, owner, id uuid.UUID, ifMatch func(int32) error) (Report, error) {
	r, err := report(ctx, tx, owner, id, true)
	if err != nil {
		return Report{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Report{}, err
	}
	if r.Status != Active {
		return Report{}, errRemoved(r.Title)
	}
	return r, nil
}

// companyDay is the company's date now in its time zone by the active
// configuration; found is false when it has no such company, whose date
// is then UTC's.
func (e *Endpoints) companyDay(ctx context.Context, code string) (config.CompanyDay, bool, error) {
	doc, _, err := e.Configs.Document(ctx)
	if err != nil {
		return config.CompanyDay{}, false, err
	}
	day, err := doc.CompanyDay(code, clock.Now(e.Now)) // UTC's date for a company it does not have
	return day, doc.Company(code) != nil, err
}
