package procurement_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestSubmissionOnTheCompanysDate: a requisition's assignments are checked
// on its company's date in its time zone as it is submitted, not UTC's
// (product owner 2026-10-31, T72n4). Cost center LATE is valid from 17
// April: at 22:00 on the 16th in Toronto (the 17th in UTC) it is not valid
// yet, and the refusal names the date; at 01:00 on the 17th in Dubai (the
// 16th in UTC) it is.
func TestSubmissionOnTheCompanysDate(t *testing.T) {
	for _, c := range []struct {
		zone   string
		at     time.Time
		reason string // the refusal's field error code, "" when submitted
		today  string
	}{
		{"America/Toronto", time.Date(2026, 4, 17, 2, 0, 0, 0, time.UTC), "not_valid_on_date", "2026-04-16"},
		{"Asia/Dubai", time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC), "", "2026-04-17"},
	} {
		t.Run(c.zone, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone }, perms...)
			f.CostCenters(t, nil)
			cc, _ := f.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "LATE", "name": "Late", "responsible": "J",
				"category": "ADMIN", "valid_from": "2026-04-17", "reason": "r"}`, f.DE), "")
			f.Approve(t, cc["pending_change_id"].(string))
			f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			e := env{Env: f, expense: f.Account(t, "2000", "510000").String()}
			r := e.requisition(t, items(centered(e.expense, "1.00", uuid.MustParse(cc["id"].(string)), "")), "")
			path := "/v1/requisitions/" + id(r)
			_, tag := e.Must(200, "GET", path, "", "")
			if c.reason == "" {
				if got, _ := e.Must(200, "POST", path+":submit", "", tag); got["status"] == "draft" {
					t.Errorf("submitted on %s: got %v, want submitted", c.today, got["status"])
				}
				return
			}
			p := e.Problem(422, "ledger.cost_center_invalid", "POST", path+":submit", "", tag)
			wantField(t, p, "items[0].cost_center_id", c.reason)
			if d, _ := p["detail"].(string); !strings.Contains(d, "checked on the company's date in its time zone as it is submitted: "+c.today) {
				t.Errorf("detail: got %q, want it to name %s", d, c.today)
			}
		})
	}
}
