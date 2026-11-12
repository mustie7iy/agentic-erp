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

// An order's settlement rule (T60d, ADR-0064 §3).

func ruleBody(receiver uuid.UUID) string {
	return fmt.Sprintf(`{"receiver_cost_center_id": %q}`, receiver)
}

// ruleLine is a rule response as "version receiver code", "-" for an
// order without a rule.
func ruleLine(m map[string]any) string {
	code, ok := m["receiver_cost_center_code"]
	if !ok {
		code = "-"
	}
	return fmt.Sprintf("v%v receiver %v", m["version"], code)
}

func TestInternalOrderSettlementRule(t *testing.T) {
	p := setup(t)
	id := p.order(t, "FAIR-26")
	order := "/v1/internal-orders/" + id
	path := order + "/settlement-rule"

	t.Run("an order without a rule", func(t *testing.T) {
		got, tag := p.in(t).Must(200, "GET", path, "", "")
		want(t, "rule", ruleLine(got), "v1 receiver -")
		want(t, "etag", tag, `"1"`)
		want(t, "the order named", fmt.Sprint(got["internal_order_id"], " ", got["internal_order_code"], " ", got["internal_order_name"], " ", got["legal_entity_id"]),
			fmt.Sprint(id, " FAIR-26 Order FAIR-26 ", p.DE))
		if _, ok := got["receiver_cost_center_id"]; ok {
			t.Errorf("receiver of an order without a rule: got %v, want none", got["receiver_cost_center_id"])
		}
	})
	t.Run("refusals", func(t *testing.T) {
		p := p.in(t)
		p.Problem(428, "request.precondition_required", "PATCH", path, ruleBody(p.c.Sales), "")
		p.Problem(412, "request.version_mismatch", "PATCH", path, ruleBody(p.c.Sales), `"9"`)
		p.Problem(404, "request.not_found", "PATCH", "/v1/internal-orders/"+uuid.NewString()+"/settlement-rule", ruleBody(p.c.Sales), `"1"`)
		p.Problem(404, "request.not_found", "GET", "/v1/internal-orders/"+uuid.NewString()+"/settlement-rule", "", "")
		pending, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "9000", "name": "Pending", "responsible": "J",
			"category": "ADMIN", "valid_from": "2026-01-01", "reason": "r"}`, p.DE), "")
		later, _ := p.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "9100", "name": "Later", "responsible": "J",
			"category": "ADMIN", "valid_from": "2999-01-01", "reason": "r"}`, p.DE), "")
		p.Approve(t, later["pending_change_id"].(string))
		// An order of company 1000, which has no cost center: company
		// 2000's is not one of its company's.
		foreign := uuid.New()
		pgtest.Exec(t, `INSERT INTO controlling.internal_order (tenant_id, id, legal_entity_id, code, name, responsible_cost_center_id, created_by, updated_by)
			VALUES ($1, $2, $3, 'US-26', 'Order of 1000', $4, $5, $5)`, p.Tenant.ID, foreign, p.US, uuid.New(), p.Tenant.Human.ID)
		for name, c := range map[string]struct {
			path, receiver, want string
		}{
			"a cost center that is none":         {path, uuid.NewString(), "receiver_cost_center_id unknown"},
			"a cost center of another company":   {"/v1/internal-orders/" + foreign.String() + "/settlement-rule", p.c.Sales.String(), "receiver_cost_center_id not_in_company"},
			"a cost center waiting for approval": {path, pending["id"].(string), "receiver_cost_center_id not_active"},
			"a cost center not yet valid today":  {path, later["id"].(string), "receiver_cost_center_id not_valid_today"},
		} {
			t.Run(name, func(t *testing.T) {
				m := p.in(t).Problem(422, "controlling.settlement_rule_receiver_invalid", "PATCH", c.path, ruleBody(uuid.MustParse(c.receiver)), `"1"`)
				want(t, "field error", reason(t, m), c.want)
			})
		}
		got, _ := p.Must(200, "GET", path, "", "")
		want(t, "rule after refusals", ruleLine(got), "v1 receiver -")
	})

	set, tag := p.Must(200, "PATCH", path, ruleBody(p.c.Sales), `"1"`)
	want(t, "the rule first set", ruleLine(set), "v2 receiver 2000")
	want(t, "the receiver's id", set["receiver_cost_center_id"], any(p.c.Sales.String()))
	want(t, "who set it", set["updated_by"], any(p.Tenant.Human.ID.String()))
	want(t, "etag of the rule first set", tag, `"2"`)
	_, orderTag := p.Must(200, "GET", order, "", "")
	want(t, "the order's etag moved with its rule", orderTag, `"2"`)

	t.Run("replaced with the order's version", func(t *testing.T) {
		p.Problem(412, "request.version_mismatch", "PATCH", path, ruleBody(p.c.Admin), `"1"`)
		replaced, tag := p.Must(200, "PATCH", path, ruleBody(p.c.Admin), orderTag)
		want(t, "the rule replaced", ruleLine(replaced), "v3 receiver 1000")
		want(t, "etag", tag, `"3"`)
		got, tag := p.Must(200, "GET", path, "", "")
		want(t, "the rule read", ruleLine(got), "v3 receiver 1000")
		want(t, "etag read", tag, `"3"`)
	})
	t.Run("a release moves the version the rule is replaced with, and a released order takes a rule", func(t *testing.T) {
		p.Must(200, "POST", order+":release", "", `"3"`)
		p.Problem(412, "request.version_mismatch", "PATCH", path, ruleBody(p.c.Sales), `"3"`)
		got, _ := p.Must(200, "PATCH", path, ruleBody(p.c.Sales), `"4"`)
		want(t, "the rule of a released order", ruleLine(got), "v5 receiver 2000")
	})

	var audited string
	pgtest.Scan(t, `SELECT string_agg(coalesce(detail->>'receiver_cost_center_code_before', '-') || ' -> ' || (detail->>'receiver_cost_center_code_after'), '; ' ORDER BY seq)
		FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.internal_order.set_settlement_rule' AND object_id = $2`, []any{p.Tenant.ID, id}, &audited)
	want(t, "audited receivers before and after", audited, "- -> 2000; 2000 -> 1000; 1000 -> 2000")
	var ids string
	pgtest.Scan(t, `SELECT (detail->>'receiver_cost_center_id_before') || ' -> ' || (detail->>'receiver_cost_center_id_after')
		FROM audit.audit_event WHERE tenant_id = $1 AND action = 'controlling.internal_order.set_settlement_rule' AND object_id = $2 ORDER BY seq DESC LIMIT 1`,
		[]any{p.Tenant.ID, id}, &ids)
	want(t, "audited receiver ids of the last change", ids, p.c.Admin.String()+" -> "+p.c.Sales.String())
}

// TestInternalOrderSettlementRuleAccess: the rule is read and set only in
// the companies a person may act in, and set only by a person holding
// controlling.order.write; agents read it.
func TestInternalOrderSettlementRuleAccess(t *testing.T) {
	p := setup(t)
	path := "/v1/internal-orders/" + p.order(t, "FAIR-26") + "/settlement-rule"
	body := ruleBody(p.c.Sales)
	other := p.as(p.Reader(t, "Reader of 1000", p.US))
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "PATCH", path, body, `"1"`)
	agent := p.As(p.Tenant.Agent)
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent.Must(200, "GET", path, "", "")
	agent.Problem(403, "identity.agent_not_allowed", "PATCH", path, body, `"1"`)
	for _, c := range []struct{ permission, method string }{{"controlling.order.write", "PATCH"}, {"controlling.order.read", "GET"}} {
		t.Run(c.method+" without "+c.permission, func(t *testing.T) {
			p := p.in(t)
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = $2`, p.Tenant.ID, c.permission)
			p.Problem(403, "identity.permission_missing", c.method, path, body, `"1"`)
		})
	}
}

// TestInternalOrderSettlementRuleSetOnce: two changes at the order's
// version both read serialize on the order's row lock; the second finds
// the version moved and is refused, and the first's receiver stays.
func TestInternalOrderSettlementRuleSetOnce(t *testing.T) {
	p := setup(t)
	h := endpoints(t)
	id := uuid.MustParse(p.order(t, "FAIR-26"))
	set := func(receiver uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.UpdateInternalOrderSettlementRule(ctx, &gen.InternalOrderSettlementRuleUpdate{ReceiverCostCenterID: receiver},
				gen.UpdateInternalOrderSettlementRuleParams{ID: id, IfMatch: `"1"`})
			return err
		}
	}
	first, second := apitest.Serialized(t, p.Tenant, p.Tenant.Human, set(p.c.Sales), set(p.c.Admin))
	if first != nil {
		t.Fatalf("first change: %v", first)
	}
	want(t, "second change", problem.From(second).Code, "request.version_mismatch")
	got, _ := p.Must(200, "GET", "/v1/internal-orders/"+id.String()+"/settlement-rule", "", "")
	want(t, "the rule kept", ruleLine(got), "v2 receiver 2000")
}
