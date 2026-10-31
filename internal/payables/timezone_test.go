package payables_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestPostingDateTimeZone: an invoice posted, and a posted one reversed,
// without a posting date is dated the company's date in its time zone,
// not UTC's (product owner 2026-10-31, ADR-0051 as built), and falls in
// that date's period. At 22:00 on 31 March in Toronto (02:00 UTC on 1
// April) it is the 31st, period 3; at 01:00 on 1 April in Dubai (21:00
// UTC on 31 March) it is the 1st, period 4. In UTC each would take the
// other month. The invoice gives the date used, and its reversal entry
// gives its own.
func TestPostingDateTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone   string
		at     time.Time
		date   string
		period float64
	}{
		{"America/Toronto", time.Date(2026, 4, 1, 2, 0, 0, 0, time.UTC), "2026-03-31", 3},
		{"Asia/Dubai", time.Date(2026, 3, 31, 21, 0, 0, 0, time.UTC), "2026-04-01", 4},
	} {
		t.Run(c.zone, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone }, perms...)
			f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			e := env{Env: f, expense: f.Account(t, "2000", "520000")}
			inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "TZ-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
			posted := e.act(t, 200, id(inv), "post", "")
			if posted["posting_date"] != c.date {
				t.Errorf("posted: got posting_date %v, want %s", posted["posting_date"], c.date)
			}
			e.wantEntry(t, "posted", posted["journal_entry_id"], c.date, c.period)
			reversed := e.act(t, 200, id(inv), "reverse", `{"reason": "wrong"}`)
			e.wantEntry(t, "reversal", reversed["reversal_entry_id"], c.date, c.period)
		})
	}
}

// wantEntry checks a journal entry's posting date and fiscal period.
func (e env) wantEntry(t *testing.T, what string, entry any, date string, period float64) {
	t.Helper()
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", entry), "", "")
	if je["posting_date"] != date {
		t.Errorf("%s entry: got posting_date %v, want %s", what, je["posting_date"], date)
	}
	if je["fiscal_period"] != period {
		t.Errorf("%s entry: got fiscal_period %v, want %v", what, je["fiscal_period"], period)
	}
}
