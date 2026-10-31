package procurement_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestReversalDateTimeZone: a goods receipt reversed without a posting
// date is dated the company's date in its time zone, not UTC's (product
// owner 2026-10-31, ADR-0051 as built), and its WE entry falls in that
// date's period. At 22:00 on 31 March in Toronto (02:00 UTC on 1 April) it
// is the 31st, period 3; at 01:00 on 1 April in Dubai (21:00 UTC on 31
// March) it is the 1st, period 4. In UTC each would take the other month.
// The reversal returned gives the date used.
func TestReversalDateTimeZone(t *testing.T) {
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
			e := env{Env: f, office: f.Account(t, "2000", "520000").String()}
			e.GRIR(t) // the receipt and its reversal post WE entries
			code, gr := e.receipt(t, e.grirOrder(t, "EUR"), "1:4:EA")
			if code != 201 {
				t.Fatalf("receipt: got %d %v, want 201", code, gr)
			}
			rev := e.act(t, "/v1/goods-receipts/"+id(gr), "reverse", "")
			if rev["posting_date"] != c.date || rev["document_date"] != c.date {
				t.Errorf("reversal: got posting_date %v, document_date %v, want %s", rev["posting_date"], rev["document_date"], c.date)
			}
			je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", rev["journal_entry_id"]), "", "")
			if je["posting_date"] != c.date {
				t.Errorf("reversal entry: got posting_date %v, want %s", je["posting_date"], c.date)
			}
			if je["fiscal_period"] != c.period {
				t.Errorf("reversal entry: got fiscal_period %v, want %v", je["fiscal_period"], c.period)
			}
		})
	}
}
