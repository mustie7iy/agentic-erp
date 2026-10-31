package procurement_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestRequisitionDateTimeZone: a requisition is dated, numbered and routed
// on its company's date in the company's time zone, not UTC's (T72n5,
// product owner 2026-10-31). At 22:00 on 31 December in Toronto it is
// already 1 January 2027 in UTC; at 01:00 on 17 April in Dubai it is still
// the 16th in UTC. The USD→EUR rate changes between the UTC day and the
// local one: at the local day's rate, 0.9, 11,111.12 USD is 10,000.008 EUR,
// over company 2000's 10,000.00 tier, two approval steps; at the UTC day's,
// 0.8, it would be 8,888.90 EUR, one step. In Toronto the number is drawn
// from 2026's range: 2027 has none (fixture.Years), so a UTC date fails.
func TestRequisitionDateTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone  string
		at    time.Time
		date  string
		rates [][2]string // valid from, USD→EUR
	}{
		{"America/Toronto", time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC), "2026-12-31", [][2]string{{"2020-01-01", "0.9"}, {"2027-01-01", "0.8"}}},
		{"Asia/Dubai", time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC), "2026-04-17", [][2]string{{"2020-01-01", "0.8"}, {"2026-04-17", "0.9"}}},
	} {
		t.Run(c.zone, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone }, append(perms, "ledger.rate.maintain")...)
			f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			e := env{Env: f, office: f.Account(t, "2000", "520000").String()}
			for _, r := range c.rates {
				e.Must(201, "POST", "/v1/exchange-rates", fmt.Sprintf(`{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR",
					"valid_from": %q, "rate": %q, "source": "ECB"}`, r[0], r[1]), "")
			}
			r, _ := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "USD", "items": %s}`,
				e.DE, items(item(e.office, "1", "EA", "11111.12", "USD", ""))), "")
			if r["requisition_date"] != c.date {
				t.Errorf("created: got requisition_date %v, want %s", r["requisition_date"], c.date)
			}
			var stored time.Time
			pgtest.Scan(t, `SELECT requisition_date FROM procurement.requisition WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, id(r)}, &stored)
			if got := stored.Format(time.DateOnly); got != c.date {
				t.Errorf("stored: got %s, want %s", got, c.date)
			}
			got, _ := e.Must(200, "GET", "/v1/requisitions/"+id(r), "", "")
			if got["requisition_date"] != c.date {
				t.Errorf("read: got requisition_date %v, want %s", got["requisition_date"], c.date)
			}
			e.act(t, "/v1/requisitions/"+id(r), "submit", "")
			if steps := len(e.Pending(t, id(r))["steps"].([]any)); steps != 2 {
				t.Errorf("approval steps: got %d, want 2, routed at %s's rate", steps, c.date)
			}
		})
	}
}
