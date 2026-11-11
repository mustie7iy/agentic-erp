package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestRunDayIsTheCompanysDay: a migration run's day, the cutover date on
// which a vendor's central address must be valid (po:2026-11-09, A1), is
// the company's date in its time zone, not UTC's (review:batch63; product
// owner 2026-10-31). The vendor's address changes at midnight between two
// days the zones disagree on: at 23:30 in Toronto, UTC already the next
// day, the run takes the address still valid that evening; at 01:00 in
// Dubai, UTC still the day before, the one valid from that morning.
// Before, both were chosen on UTC's day. The address starts on the
// platform on partner's day, UTC's, as before: partner refuses an earlier
// one, and a supplier's days are one rule for all its companies.
func TestRunDayIsTheCompanysDay(t *testing.T) {
	var utcDay time.Time
	pgtest.Scan(t, `SELECT current_date`, nil, &utcDay)
	for _, c := range []struct {
		zone    string
		hour    int // of the clock's day, in the zone
		minute  int
		clock   int // the clock's day in the zone, in days from UTC's
		changes int // the address's first day, in days from UTC's
		want    string
	}{
		{"America/Toronto", 23, 30, -1, 0, "Old street 1"},
		{"Asia/Dubai", 1, 0, 1, 1, "New street 1"},
	} {
		t.Run(c.zone, func(t *testing.T) {
			zone, err := time.LoadLocation(c.zone)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(utcDay.Year(), utcDay.Month(), utcDay.Day()+c.clock, c.hour, c.minute, 0, 0, zone)
			if got := at.UTC().Format(time.DateOnly); got != utcDay.Format(time.DateOnly) {
				t.Fatalf("UTC's day at %s: got %s, want %s, another day than the company's", at, got, utcDay.Format(time.DateOnly))
			}
			dir := t.TempDir()
			f := fixture.NewWith(t, nil, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone }, perms...)
			f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.ExtractDir, cfg.Now = dir, func() time.Time { return at } })
			e := env{Env: f, dir: dir}
			x := tiny()
			x.LFA1[0].ADRNR = "0000004711"
			first := utcDay.AddDate(0, 0, c.changes)
			x.ADRC = []sap.ADRC{
				{ADDRNUMBER: "0000004711", DATE_FROM: "00010101", DATE_TO: first.AddDate(0, 0, -1).Format("20060102"), STREET: "Old street", HOUSE_NUM1: "1", CITY1: "Bonn"},
				{ADDRNUMBER: "0000004711", DATE_FROM: first.Format("20060102"), DATE_TO: "99991231", STREET: "New street", HOUSE_NUM1: "1", CITY1: "Bonn"}}
			write(t, e.dir, "tiny.json", x)
			_, got := e.act(t, e.run(t, e.connector(t, "tiny.json"), "documents", false, e.DE.String()), "start")
			if got["status"] != "loaded" {
				t.Fatalf("run status: got %v, want loaded: %v", got["status"], got["report"])
			}
			var after time.Time
			if pgtest.Scan(t, `SELECT current_date`, nil, &after); !after.Equal(utcDay) {
				t.Skipf("UTC's day changed during the test, from %s to %s: the clock was set by the first", utcDay.Format(time.DateOnly), after.Format(time.DateOnly))
			}
			if quality := fmt.Sprint(got["report"].(map[string]any)["quality"]); strings.Contains(quality, "loads without an address") {
				t.Errorf("quality gate: got %s, want no address warning", quality)
			}
			var line1 string
			var validFrom time.Time
			pgtest.Scan(t, `SELECT a.line1, a.valid_from FROM partner.address a JOIN partner.business_partner p ON p.tenant_id = a.tenant_id AND p.id = a.partner_id
				WHERE a.tenant_id = $1 AND p.name = 'Tiny GmbH'`, []any{e.Tenant.ID}, &line1, &validFrom)
			if line1 != c.want {
				t.Errorf("address at %s (%s in UTC): got %q, want %q, the one valid on the company's day", at, at.UTC().Format(time.DateOnly), line1, c.want)
			}
			if !validFrom.Equal(utcDay) {
				t.Errorf("address valid_from: got %s, want %s, partner's day", validFrom.Format(time.DateOnly), utcDay.Format(time.DateOnly))
			}
		})
	}
}

// zoneLost is the configuration whose company 2000 names a zone the binary
// cannot load.
type zoneLost struct{ integration.Settings }

func (z zoneLost) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := z.Settings.Document(ctx)
	if err == nil {
		doc.Company("2000").TimeZone = "America/Nowhere"
	}
	return doc, n, err
}

// TestRunDayZoneLost: a company whose stored zone does not load fails the
// run's start rather than dating the run in UTC; nothing is loaded.
func TestRunDayZoneLost(t *testing.T) {
	e := setup(t)
	write(t, e.dir, "tiny.json", tiny())
	run := e.run(t, e.connector(t, "tiny.json"), "documents", false, e.DE.String())
	h := endpoints(e.dir)
	h.Settings = zoneLost{h.Settings}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.StartRun(ctx, uuid.MustParse(run["id"].(string)), func(int32) error { return nil })
		return err
	})
	if want := `company 2000: time zone "America/Nowhere"`; err == nil || !strings.Contains(err.Error(), "integration: migration run "+run["id"].(string)) ||
		!strings.Contains(err.Error(), want) {
		t.Errorf("start: got %v, want the run refused for %s", err, want)
	}
	if got, _ := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", ""); got["status"] != "queued" {
		t.Errorf("run status after the failed start: got %v, want queued", got["status"])
	}
	if n := count(t, `SELECT count(*) FROM partner.business_partner WHERE tenant_id = $1 AND name = 'Tiny GmbH'`, e.Tenant.ID); n != 0 {
		t.Errorf("suppliers after the failed start: got %d, want 0", n)
	}
}
