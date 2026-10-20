package procurement_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// noSecretProject is a tenant's rule on purchase orders: a custom field
// may not name the secret project, and orders are dated this year.
func noSecretProject() map[string]rules.Rule {
	row := map[string]any{"legal_entity_id": "le", "supplier_id": "s", "currency": "EUR", "order_date": "2026-03-10"}
	return map[string]rules.Rule{"no_secret_project": {
		Expression: `ext.project.orValue("") != "secret" && row.order_date >= "2026-01-01" && row.payment_term_code.orValue("x") != ""`,
		Message:    "The secret project is not ordered through purchase orders.",
		Cases:      []rules.Case{{Row: row, Valid: true}, {Row: row, Extensions: map[string]any{"project": "secret"}, Valid: false}},
	}}
}

// ruleRefused checks a 422 request.rule_refused names the rule.
func ruleRefused(t *testing.T, p map[string]any) {
	t.Helper()
	errs, _ := p["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["field"] != "rules.no_secret_project" {
		t.Errorf("errors: got %v, want one on rules.no_secret_project", p["errors"])
	}
}

// audited checks one refusal of action is audited with source rule in the
// configuration version.
func audited(t *testing.T, e fixture.Env, action string, version int32) {
	t.Helper()
	got := fixture.RuleAudits(t, e.Tenant.ID, action)
	if len(got) != 1 || got[0].Outcome != "refused" || got[0].Rule != "no_secret_project" || got[0].ConfigVersion != version {
		t.Errorf("%s audit: got %+v, want one refusal by no_secret_project in version %d", action, got, version)
	}
}

// TestRulesOnOrderWrites: the tenant's rules judge a purchase order's
// creation and update, and an order change's creation and update, as each
// leaves the header; a refusal is 422 naming the rule, audited.
func TestRulesOnOrderWrites(t *testing.T) {
	e := setup(t)
	path := e.releasedOrder(t)
	version := fixture.ActivateRules(t, e.Tenant.ID, "purchase_order", noSecretProject())
	its := items(item(e.office, "1", "EA", "1.00", "EUR", ""))
	ruleRefused(t, e.Problem(422, "request.rule_refused", "POST", "/v1/purchase-orders", `{"legal_entity_id": "`+e.DE.String()+`", "supplier_id": "`+
		e.Supplier.String()+`", "currency": "EUR", "order_date": "2026-03-10", "items": `+its+`, "extensions": {"project": "secret"}}`, ""))
	audited(t, e.Env, "procurement.purchase_order.create", version)

	o := e.order(t, its, `, "extensions": {"project": "open"}`)
	ruleRefused(t, e.Problem(422, "request.rule_refused", "PATCH", "/v1/purchase-orders/"+id(o), `{"extensions": {"project": "secret"}}`, `"1"`))
	audited(t, e.Env, "procurement.purchase_order.update", version)

	ruleRefused(t, e.Problem(422, "request.rule_refused", "POST", path+"/changes", `{"reason": "Secret", "extensions": {"project": "secret"}}`, ""))
	audited(t, e.Env, "procurement.purchase_order_change.create", version)
	ch, tag := e.Must(201, "POST", path+"/changes", `{"reason": "Later", "order_date": "2026-03-20"}`, "")
	ruleRefused(t, e.Problem(422, "request.rule_refused", "PATCH", path+"/changes/"+id(ch), `{"extensions": {"project": "secret"}}`, tag))
	audited(t, e.Env, "procurement.purchase_order_change.update", version)
}
