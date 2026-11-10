package integration

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// A migration run's day (review:batch63; ADR-0051 As built). The day a run
// loads is the cutover date of the master data it migrates: the central
// address of a vendor valid on it is the one taken (po:2026-11-09, A1). It
// is each company's date in its time zone (config.CompanyDay, product
// owner 2026-10-31), not UTC's: at 23:30 on the cutover evening in Toronto
// UTC is already tomorrow, and at 01:00 in Dubai still yesterday. What the
// run writes with a date of the platform's keeps partner's own rule, UTC's
// day (target.today): a supplier's bank account and address start on it
// (the bank account the day before, product owner 2026-11-02), since a
// supplier serves companies in several zones and partner refuses an
// earlier start.

// companyDays is the date at instant now of each of the run's company
// codes in its time zone. A stored zone the binary cannot load fails the
// run rather than dating it in UTC.
func companyDays(doc *config.Config, entities map[string]tenancy.Entity, now time.Time) (map[string]time.Time, error) {
	days := map[string]time.Time{}
	for code := range entities {
		d, err := doc.CompanyDay(code, now)
		if err != nil {
			return nil, fmt.Errorf("the run's day: %w", err)
		}
		days[code] = d.Date
	}
	return days, nil
}

// cutoverDays are the run's days for a vendor: the days of the company
// codes it is set up in (LFB1), each once, in order; of every company of
// the run for a vendor set up in none. They are one day unless the
// vendor's companies are in time zones whose dates differ at the run's
// instant.
func (t target) cutoverDays(companies []sap.LFB1) []time.Time {
	var codes []string
	for _, c := range companies {
		codes = append(codes, c.BUKRS)
	}
	if len(codes) == 0 {
		codes = sortedKeys(t.days)
	}
	var days []time.Time
	for _, code := range codes {
		if d := t.days[code]; !slices.Contains(days, d) {
			days = append(days, d)
		}
	}
	slices.SortFunc(days, time.Time.Compare)
	return days
}

// vendorCompanies are the extract's company data (LFB1) by vendor.
func vendorCompanies(e *sap.Extract) map[string][]sap.LFB1 {
	out := map[string][]sap.LFB1{}
	for _, c := range e.LFB1 {
		out[c.LIFNR] = append(out[c.LIFNR], c)
	}
	return out
}

// daysText names days as a finding does: one, or several joined by "or".
func daysText(days []time.Time) string {
	var out []string
	for _, d := range days {
		out = append(out, d.Format(time.DateOnly))
	}
	return strings.Join(out, " or ")
}
