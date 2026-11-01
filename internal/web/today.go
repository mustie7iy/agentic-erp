package web

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
)

// A form's or report's default date is its company's date today in the
// company's time zone, as the API tells it (getLegalEntityToday; ADR-0051
// as built, T72n1), never the workspace's clock in UTC: at 22:00 in
// Toronto, UTC is already on the next day, and a form dated by it would
// post in the next day's period, or the next year's (review:batch27). The
// API's day is the one it defaults a date left out to, so the form and
// the API agree.

// companyDay is the company's date today, by its id as the form or query
// holds it; ok is false when there is no company, the person may not read
// companies, or the API does not tell the date (logged). The form then
// leaves the date for the person to type rather than offer another day.
func (s *Server) companyDay(ctx context.Context, m me, company string) (time.Time, bool) {
	id, err := uuid.Parse(company)
	if err != nil || !m.may(permReadCompanies) {
		return time.Time{}, false
	}
	res, err := s.API.GetLegalEntityToday(ctx, gen.GetLegalEntityTodayParams{ID: id})
	day, ok := res.(*gen.LegalEntityToday)
	if !ok {
		s.Log.WarnContext(ctx, "web: the company's date could not be read", "answer", res, "error", err)
		return time.Time{}, false
	}
	return day.Date, true
}

// today is the company's date today as a form holds a date, "" when it
// cannot be told (companyDay).
func (s *Server) today(ctx context.Context, m me, company string) string {
	day, ok := s.companyDay(ctx, m, company)
	if !ok {
		return ""
	}
	return day.Format(time.DateOnly)
}

// year is the year of the company's date today, a report's default
// fiscal year. When the company's date cannot be told (companyDay) it is
// the workspace's clock's year: a filter shown on the page, which differs
// from the company's only in the hours around the new year, where leaving
// it empty would answer the page with a filter error and hide the API's
// own answer to the report (a 403, a 503) behind it. A date a form sends
// is never defaulted so (today).
func (s *Server) year(ctx context.Context, m me, company string) string {
	day, ok := s.companyDay(ctx, m, company)
	if !ok {
		day = s.now()
	}
	return strconv.Itoa(day.Year())
}

// yearOfCompany is year for a ledger report's query, which chooses its
// company itself (parseLedgerQuery).
func (s *Server) yearOfCompany(ctx context.Context, m me) func(company string) string {
	return func(company string) string { return s.year(ctx, m, company) }
}
