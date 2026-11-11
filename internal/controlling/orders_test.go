package controlling_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// orderBody is a new order of the company with the responsible cost center.
func orderBody(le uuid.UUID, code string, costCenter uuid.UUID) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "Order %s", "responsible_cost_center_id": %q}`, le, code, code, costCenter)
}

// order creates an order of company 2000 under cost center 1000 and
// returns its id.
func (p plan) order(t *testing.T, code string) string {
	t.Helper()
	o, _ := p.Must(201, "POST", "/v1/internal-orders", orderBody(p.DE, code, p.c.Admin), "")
	return o["id"].(string)
}

// orders lists the orders of a query as "code status cost center", then
// the next page's token if there is one.
func orders(t *testing.T, c apitest.Client, query string) []string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/internal-orders"+query, "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		o := it.(map[string]any)
		out = append(out, fmt.Sprint(o["code"], " ", o["status"], " ", o["responsible_cost_center_code"]))
	}
	if _, ok := page["next_page_token"].(string); ok {
		out = append(out, "next")
	}
	return out
}

// reason is the code of a problem's first field error.
func reason(t *testing.T, m map[string]any) string {
	t.Helper()
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("problem %v: no field errors", m)
	}
	e, _ := errs[0].(map[string]any)
	return fmt.Sprint(e["field"], " ", e["code"])
}

func TestInternalOrders(t *testing.T) {
	p := setup(t)
	created, tag := p.Must(201, "POST", "/v1/internal-orders", orderBody(p.DE, "FAIR-26", p.c.Admin), "")
	id := created["id"].(string)
	path := "/v1/internal-orders/" + id
	want(t, "new order's status", created["status"], any("created"))
	want(t, "new order's cost center code", created["responsible_cost_center_code"], any("1000"))
	want(t, "new order's etag", tag, `"1"`)
	got, tag := p.Must(200, "GET", path, "", "")
	want(t, "read order's cost center", got["responsible_cost_center_id"], any(p.c.Admin.String()))
	want(t, "read order's cost center code", got["responsible_cost_center_code"], any("1000"))
	want(t, "read order's etag", tag, `"1"`)

	t.Run("refusals of a create", func(t *testing.T) {
		p := p.in(t)
		p.Problem(409, "controlling.internal_order_exists", "POST", "/v1/internal-orders", orderBody(p.DE, "FAIR-26", p.c.Sales), "")
		p.Blank("name", "POST", "/v1/internal-orders", fmt.Sprintf(`{"legal_entity_id": %q, "code": "BLANK", "name": "  ", "responsible_cost_center_id": %q}`,
			p.DE, p.c.Admin), "")
		p.Problem(422, "request.reference_not_found", "POST", "/v1/internal-orders", orderBody(uuid.New(), "X", p.c.Admin), "")
		pending, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "9000", "name": "Pending", "responsible": "J",
			"category": "ADMIN", "valid_from": "2026-01-01", "reason": "r"}`, p.DE), "")
		for name, c := range map[string]struct {
			le         uuid.UUID
			costCenter string
			want       string
		}{
			"a cost center that is none":         {p.DE, uuid.NewString(), "responsible_cost_center_id unknown"},
			"a cost center of another company":   {p.US, p.c.Admin.String(), "responsible_cost_center_id not_in_company"},
			"a cost center waiting for approval": {p.DE, pending["id"].(string), "responsible_cost_center_id not_active"},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.internal_order_cost_center_invalid", "POST", "/v1/internal-orders",
					orderBody(c.le, "REFUSED", uuid.MustParse(c.costCenter)), "")
				want(t, "field error", reason(t, m), c.want)
			})
		}
	})

	t.Run("release", func(t *testing.T) {
		p := p.in(t)
		p.Problem(412, "request.version_mismatch", "POST", path+":release", "", `"9"`)
		p.Problem(404, "request.not_found", "POST", "/v1/internal-orders/"+uuid.NewString()+":release", "", `"1"`)
		released, tag := p.Must(200, "POST", path+":release", "", `"1"`)
		want(t, "released order's status", released["status"], any("released"))
		want(t, "released order's cost center code", released["responsible_cost_center_code"], any("1000"))
		want(t, "released order's etag", tag, `"2"`)
		p.Problem(409, "controlling.internal_order_released", "POST", path+":release", "", tag)
	})

	t.Run("list", func(t *testing.T) {
		p := p.in(t)
		p.Must(201, "POST", "/v1/internal-orders", orderBody(p.DE, "AUDIT-26", p.c.Sales), "")
		// The same code in another company: codes are unique per company.
		// Its id sorts after every other, so the two FAIR-26 list in one order.
		pgtest.Exec(t, `INSERT INTO controlling.internal_order (tenant_id, id, legal_entity_id, code, name, responsible_cost_center_id, created_by, updated_by)
			VALUES ($1, $5, $2, 'FAIR-26', 'Fair of 1000', $3, $4, $4)`, p.Tenant.ID, p.US, uuid.New(), p.Tenant.Human.ID, uuid.Max)
		for name, c := range map[string]struct {
			query string
			want  []string
		}{
			"every order by code":      {"", []string{"AUDIT-26 created 2000", "FAIR-26 released 1000", "FAIR-26 created "}},
			"of a company":             {"?legal_entity_id=" + p.DE.String(), []string{"AUDIT-26 created 2000", "FAIR-26 released 1000"}},
			"with a code":              {"?code=FAIR-26&legal_entity_id=" + p.DE.String(), []string{"FAIR-26 released 1000"}},
			"in a status":              {"?status=created&legal_entity_id=" + p.DE.String(), []string{"AUDIT-26 created 2000"}},
			"of a responsible center":  {"?responsible_cost_center_id=" + p.c.Sales.String(), []string{"AUDIT-26 created 2000"}},
			"a first page":             {"?page_size=1", []string{"AUDIT-26 created 2000", "next"}},
			"of a company without any": {"?legal_entity_id=" + uuid.NewString(), nil},
		} {
			t.Run(name, func(t *testing.T) { wantList(t, "orders", orders(t, p.Client, c.query), c.want) })
		}
		p.Problem(400, "request.invalid_order_by", "GET", "/v1/internal-orders?order_by=name", "", "")
		p.Problem(404, "request.not_found", "GET", "/v1/internal-orders/"+uuid.NewString(), "", "")
	})

	var changes []string
	for _, ev := range p.events(t, "internal_order_changed") {
		changes = append(changes, fmt.Sprint(ev["code"], " ", ev["change"], " ", ev["status"], " ", ev["responsible_cost_center_code"]))
	}
	wantList(t, "events", changes, []string{"FAIR-26 created created 1000", "FAIR-26 released released 1000", "AUDIT-26 created created 2000"})
	want(t, "audit", p.audited(t), "controlling.internal_order.create controlling.internal_order.release controlling.internal_order.create")
}

// TestInternalOrderCompanyAccess: a person sees and changes only the
// orders of the companies they may act in; another company's order is
// answered as missing, so its id cannot be probed.
func TestInternalOrderCompanyAccess(t *testing.T) {
	p := setup(t)
	path := "/v1/internal-orders/" + p.order(t, "FAIR-26")
	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	wantList(t, "orders a person of another company lists", orders(t, other.Client, ""), nil)
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "POST", path+":release", "", `"1"`)
	other.Problem(422, "request.reference_not_found", "POST", "/v1/internal-orders", orderBody(p.DE, "THEIRS", p.c.Admin), "")
	own := p.as(p.Reader(t, "Reader of 2000", p.DE))
	wantList(t, "orders a person of the company lists", orders(t, own.Client, ""), []string{"FAIR-26 created 1000"})
	own.Must(200, "POST", path+":release", "", `"1"`)
}

// TestInternalOrderWritesAreHuman: agents read orders but may not create
// or release one (x-agent: false; controlling.order.release is an approval
// permission), and a person without the release permission may not release.
func TestInternalOrderWritesAreHuman(t *testing.T) {
	p := setup(t)
	path := "/v1/internal-orders/" + p.order(t, "FAIR-26")
	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path, "", "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/internal-orders", orderBody(p.DE, "AGENT", p.c.Admin), "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":release", "", `"1"`)
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'controlling.order.release'`, p.Tenant.ID)
	p.Problem(403, "identity.permission_missing", "POST", path+":release", "", `"1"`)
}

// TestInternalOrderReleasedOnce: two releases at the version both read
// serialize on the order's row lock; the second finds the version moved
// and is refused, and one release is published.
func TestInternalOrderReleasedOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := uuid.MustParse(p.order(t, "FAIR-26"))
	release := func(ctx context.Context) error {
		_, err := h.ReleaseInternalOrder(ctx, gen.ReleaseInternalOrderParams{ID: id, IfMatch: `"1"`})
		return err
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, release, release)
	if first != nil {
		t.Fatalf("first release: %v", first)
	}
	want(t, "second release", problem.From(second).Code, "request.version_mismatch")
	var released int
	for _, ev := range p.events(t, "internal_order_changed") {
		if ev["change"] == "released" {
			released++
		}
	}
	want(t, "releases published", released, 1)
}
