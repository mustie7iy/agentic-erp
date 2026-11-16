package procurement_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestOrderDocumentTimeZone: the supplier's address an order's document
// prints is the one valid on the buyer company's date, in its time zone,
// not UTC's (ADR-0051 As built, review:batch75). The supplier's order
// desk moves on 11 March 2026: at 22:30 on 10 March in Toronto (02:30 UTC
// on the 11th) the document still goes to the legal address, and at
// 01:00 on 11 March in Dubai (21:00 UTC on the 10th) already to the new
// order desk; UTC's date would give the other address in each.
func TestOrderDocumentTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone string
		at   time.Time
		want string
	}{
		{"America/Toronto", time.Date(2026, 3, 11, 2, 30, 0, 0, time.UTC), "  1 Legal Way\n"},
		{"Asia/Dubai", time.Date(2026, 3, 10, 21, 0, 0, 0, time.UTC), "  2 Order Street\n"},
	} {
		t.Run(c.zone, func(t *testing.T) {
			e := setupWith(t, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone })
			e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			for _, a := range []struct{ purpose, line1, from string }{{"legal", "1 Legal Way", "2026-01-01"}, {"order_from", "2 Order Street", "2026-03-11"}} {
				pgtest.Exec(t, `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, city, country_code, valid_from, created_by)
					VALUES ($1, gen_random_uuid(), $2, $3, $4, 'Springfield', 'US', $5, $6)`, e.Tenant.ID, e.Supplier, a.purpose, a.line1, a.from, e.Tenant.Human.ID)
			}
			o := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
			e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
			code, body, _ := e.document(t, id(o), e.Tenant.Human)
			if code != 200 {
				t.Fatalf("document: got %d %s, want 200", code, body)
			}
			if !strings.Contains(body, "\nSupplier\n") || !strings.Contains(body, c.want) {
				t.Errorf("document: got\n%s\nwant the supplier's address %q", body, c.want)
			}
		})
	}
}

// TestOrderDocumentZoneLost: a document whose buyer company's time zone
// does not load (validation refuses one, so only a zone database that
// lost it) fails rather than choosing the supplier's address on UTC's
// date.
func TestOrderDocumentZoneLost(t *testing.T) {
	e := setup(t)
	o := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	_, err := renderWith(t, e, id(o), "zone", nil)
	if want := fmt.Sprintf("document of purchase order %s: company 2000", id(o)); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("document: got %v, want an error saying %q", err, want)
	}
}
