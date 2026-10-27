package controlling_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// compared is setup's tenant with a 2026 budget and actuals in company
// 2000: plan 100.00 in May and 50.00 in June on the Admin cost center's
// office supplies (520000), 20.00 in June on Sales'; actual 120.00 in May
// (projected) and 30.00 in June (still queued) on Admin's, and in June
// 10.00 on Sales' office supplies and 80.00 of revenue (400000) on Sales,
// which has no plan.
type compared struct {
	plan
	budget string
}

func setupCompared(t *testing.T) compared {
	t.Helper()
	c := compared{plan: setup(t)}
	c.budget = c.category(t, "PLAN")
	c.batch(t, c.budget, "budget", line(c.c.Admin, c.office, 5, "100.00"), line(c.c.Admin, c.office, 6, "50.00"), line(c.c.Sales, c.office, 6, "20.00"))
	c.post(t, "2026-05-10", on(c.office, 12000, &c.c.Admin), on(c.bank, -12000, nil))
	project(t)
	c.post(t, "2026-06-15", on(c.office, 3000, &c.c.Admin), on(c.office, 1000, &c.c.Sales), on(c.revenue, -8000, &c.c.Sales), on(c.bank, 4000, nil))
	return c
}

// compare lists the rows of a query as "center/account plan actual
// variance percent", the next page's token last as "next:...".
func (c compared) compare(t *testing.T, query string) []string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+"&fiscal_year=2026"+query, "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		r := it.(map[string]any)
		amount := func(k string) any { return r[k].(map[string]any)["amount"] }
		pct, ok := r["variance_percent"].(string)
		if !ok {
			pct = "-"
		}
		out = append(out, fmt.Sprintf("%s/%s %v %v %v %s", c.code(r["cost_center_id"].(string)), c.code(r["account_id"].(string)), amount("plan"),
			amount("actual"), amount("variance"), pct))
	}
	if tok, ok := page["next_page_token"].(string); ok {
		out = append(out, "next:"+tok)
	}
	return out
}

func TestCostCenterPlanActual(t *testing.T) {
	c := setupCompared(t)
	var root, pcRoot string
	pgtest.Scan(t, `SELECT n.id::text FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'cost_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &root)
	pgtest.Scan(t, `SELECT n.id::text FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'profit_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &pcRoot)
	all := []string{"1000/520000 150.00 150.00 0.00 0.00", "2000/400000 0.00 -80.00 -80.00 -", "2000/520000 20.00 10.00 -10.00 -50.00"}
	for name, q := range map[string]struct {
		query string
		want  []string
	}{
		"every cost center":              {"", all},
		"June":                           {"&fiscal_period_from=6&fiscal_period_to=6", []string{"1000/520000 50.00 30.00 -20.00 -40.00", all[1], all[2]}},
		"May":                            {"&fiscal_period_to=5", []string{"1000/520000 100.00 120.00 20.00 20.00"}},
		"the cost center":                {"&cost_center_id=" + c.c.Admin.String(), all[:1]},
		"the standard hierarchy's root":  {"&hierarchy_node_id=" + root, all},
		"the root and the cost center":   {"&hierarchy_node_id=" + root + "&cost_center_id=" + c.c.Sales.String(), all[1:]},
		"a profit center hierarchy node": {"&hierarchy_node_id=" + pcRoot, nil},
		"a center not used":              {"&cost_center_id=" + c.c.Locked.String(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			wantList(t, "rows", sorted(compared{plan: c.in(t), budget: c.budget}.compare(t, q.query)), q.want)
		})
	}
	var pages []string
	query := "&page_size=1"
	for range 4 {
		page := c.compare(t, query)
		if n := len(page); n == 2 {
			pages, query = append(pages, page[0]), "&page_size=1&page_token="+strings.TrimPrefix(page[1], "next:")
			continue
		}
		pages = append(pages, page...)
		break
	}
	wantList(t, "pages of one row", sorted(pages), all)
	c.Problem(400, "request.invalid_parameter", "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+
		"&fiscal_year=2026&fiscal_period_from=7&fiscal_period_to=6", "", "")
	c.Problem(400, "request.invalid_page_token", "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+
		"&fiscal_year=2026&page_token=x", "", "")

	forecast := compared{plan: c.plan, budget: c.category(t, "FORECAST")}
	wantList(t, "a category without a plan", sorted(forecast.compare(t, "")),
		[]string{"1000/520000 0.00 150.00 150.00 -", "2000/400000 0.00 -80.00 -80.00 -", "2000/520000 0.00 10.00 10.00 -"})
	wantList(t, "an unknown category", compared{plan: c.plan, budget: uuid.NewString()}.compare(t, ""), nil)
	wantList(t, "a company the reader cannot act in", compared{plan: c.as(c.Reader(t, "outsider")), budget: c.budget}.compare(t, ""), nil)
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'ledger.entry.read'`, c.Tenant.ID)
	c.Problem(403, "identity.permission_missing", "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+
		"&fiscal_year=2026", "", "")
}
