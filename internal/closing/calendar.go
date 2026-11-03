package closing

import (
	"context"
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
)

// maxDays bounds the search for a business day, as cash's does, so a
// calendar whose every day is a holiday still answers.
const maxDays = 366

// GetCloseCalendar implements GET /v1/close-calendar (ADR-0058 §3): the
// leading ledger's regular periods of the company's fiscal year, each with
// the planned first and last due dates of the tasks the active close
// template gives the company. A legal entity the caller may not act in, or
// that does not exist, has no rows, as the ledger's list reads answer.
func (e *Endpoints) GetCloseCalendar(ctx context.Context, params gen.GetCloseCalendarParams) (gen.GetCloseCalendarRes, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("closing: close calendar: %w", err)
	}
	out := &gen.CloseCalendar{LegalEntityID: params.LegalEntityID, FiscalYear: params.FiscalYear, LedgerCode: config.LeadingLedger,
		LedgerName: leadingName(doc), Periods: []gen.CloseCalendarPeriod{}}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("closing: close calendar: %w", err)
	}
	if !access.Allows(params.LegalEntityID) {
		return out, nil
	}
	co, ok, err := e.Companies.Company(ctx, params.LegalEntityID)
	if err != nil {
		return nil, fmt.Errorf("closing: close calendar of %s: %w", params.LegalEntityID, err)
	}
	if !ok {
		return out, nil
	}
	out.CompanyCode.SetTo(co.Code)
	out.CompanyName.SetTo(co.Name)
	out.Weekend = doc.Weekend(co.Code)
	// The contract bounds the year to 1900-2999, so it fits a smallint.
	periods, err := e.Ledger.LeadingPeriods(ctx, params.LegalEntityID, int16(params.FiscalYear))
	if err != nil {
		return nil, fmt.Errorf("closing: close calendar of %s: %w", params.LegalEntityID, err)
	}
	tasks := doc.CloseTasksOf(co.Code)
	for _, p := range periods {
		row := gen.CloseCalendarPeriod{FiscalPeriodID: p.ID, Period: int32(p.Number), StartDate: p.StartDate, EndDate: p.EndDate, Status: p.Status}
		if first, last, ok := dueRange(doc, co.Code, p.EndDate, tasks); ok {
			row.FirstDueOn.SetTo(first)
			row.LastDueOn.SetTo(last)
		}
		out.Periods = append(out.Periods, row)
	}
	return out, nil
}

// leadingName is the leading ledger's name in the document, which
// validation keeps in every document that lists ledgers.
func leadingName(doc *config.Config) string {
	name := config.LeadingLedger
	for _, l := range doc.LedgerList() {
		if l.Code == config.LeadingLedger {
			name = l.Name
		}
	}
	return name
}

// dueRange is the earliest and latest due date of the tasks in a period
// ending on end; ok false without tasks.
func dueRange(doc *config.Config, company string, end time.Time, tasks []config.CloseTask) (first, last time.Time, ok bool) {
	for i, t := range tasks {
		d := dueOn(doc, company, end, t.DueDay)
		if i == 0 || d.Before(first) {
			first = d
		}
		if i == 0 || d.After(last) {
			last = d
		}
	}
	return first, last, len(tasks) > 0
}

// dueOn is a task's due date in a period ending on end (ADR-0058 §3): the
// end itself for day 0, else the day-th business day of the company after
// it, or before it for a negative day. The search stops after maxDays
// calendar days, so a calendar without business days still answers.
func dueOn(doc *config.Config, company string, end time.Time, day int) time.Time {
	step := 1
	if day < 0 {
		step, day = -1, -day
	}
	d := end
	for i := 0; day > 0 && i < maxDays; i++ {
		d = d.AddDate(0, 0, step)
		if doc.BusinessDay(company, d) {
			day--
		}
	}
	return d
}
