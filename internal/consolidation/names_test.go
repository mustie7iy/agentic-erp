package consolidation_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestConsolidationBalancesNameTradingPartnersInAccess: a translated
// row's trading partner is named (company code and name) only to a caller
// who may act in that company; a caller who may read the run (every
// company of its group) but not the trading partner, a company outside
// the group, gets its id alone, listed or filtered on (product owner
// 2026-11-05, ADR-0059 As built).
func TestConsolidationBalancesNameTradingPartnersInAccess(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	outside := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '9000', 'Outside Holdings', 'US', 'USD', gen_random_uuid(), 'active', $3, $3)`, e.Tenant.ID, outside, e.Tenant.Human.ID)
	post(t, e, e.US, "1000", "USD", "2026-11-10", "100000", "400000", "300.00", &outside)
	post(t, e, e.US, "1000", "USD", "2026-11-11", "100000", "400000", "50.00", &e.DE)
	run, _ := start(t, e)
	carryOut(t, e, app.Consolidation(app.Config{}), run)
	path := runsPath + "/" + run.String() + "/balances"
	parent := e.DE.String() + " 2000 Acme GmbH"
	for name, c := range map[string]struct {
		companies []uuid.UUID
		outside   string
	}{
		"a caller who may act in every company": {[]uuid.UUID{e.US, e.DE, outside}, outside.String() + " 9000 Outside Holdings"},
		"a caller who may act in the group's":   {[]uuid.UUID{e.US, e.DE}, outside.String() + " <nil> <nil>"},
	} {
		t.Run(name, func(t *testing.T) {
			reader := e.As(e.Reader(t, "reader", c.companies...)).In(t)
			page, _ := reader.Must(200, "GET", path, "", "")
			if got, want := tradingPartners(page), parent+" | "+c.outside; got != want {
				t.Errorf("trading partners: got %q, want %q", got, want)
			}
			filtered, _ := reader.Must(200, "GET", path+"?trading_partner_id="+outside.String(), "", "")
			if got, want := tradingPartners(filtered), c.outside; got != want {
				t.Errorf("filtered on the outside company: got %q, want %q", got, want)
			}
		})
	}
}

// tradingPartners are a page's trading partners as "id code name", in
// the page's order, joined by " | "; rows without one are left out.
func tradingPartners(page map[string]any) string {
	var out []string
	for _, it := range page["items"].([]any) {
		if p, ok := it.(map[string]any)["trading_partner"].(map[string]any); ok {
			out = append(out, fmt.Sprintf("%v %v %v", p["id"], p["code"], p["name"]))
		}
	}
	return strings.Join(out, " | ")
}
