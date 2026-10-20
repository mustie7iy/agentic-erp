package partner_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// TestRulesOnPartnerWrites: the tenant's rules judge a business partner's
// creation and an update change as it would leave the partner; a refusal
// is 422 naming the rule, audited with source rule and the configuration
// version, against the partner an update names.
func TestRulesOnPartnerWrites(t *testing.T) {
	e := setup(t)
	p := e.active(t, "Muster", 0)
	row := func(country string) map[string]any {
		return map[string]any{"kind": "organization", "name": "N", "country_code": country, "roles": []any{"supplier"}}
	}
	version := fixture.ActivateRules(t, e.ten.ID, "business_partner", map[string]rules.Rule{"german_segment": {
		Expression: `row.country_code != "DE" || ext.segment.orValue("") != "" && row.legal_name.orValue("x") != "" && row.language.orValue("de") != ""`,
		Message:    "A German partner needs a segment.",
		Cases:      []rules.Case{{Row: row("FR"), Valid: true}, {Row: row("DE"), Valid: false}},
	}})
	refused := e.Problem(422, "request.rule_refused", "POST", "/v1/business-partners", e.supplier("Neu", 0), "")
	if errs, _ := refused["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "rules.german_segment" {
		t.Errorf("create errors: got %v, want one on rules.german_segment", refused["errors"])
	}
	got := fixture.RuleAudits(t, e.ten.ID, "partner.business_partner.create")
	if len(got) != 1 || got[0].Rule != "german_segment" || got[0].ConfigVersion != version || got[0].ObjectID != nil {
		t.Errorf("create audit: got %+v, want one refusal by german_segment in version %d", got, version)
	}
	e.Problem(422, "request.rule_refused", "POST", "/v1/partner-changes", e.update(id(p), `{"name": "Muster AG", "legal_name": "M", "language": "de"}`), "")
	got = fixture.RuleAudits(t, e.ten.ID, "partner.change.request")
	if len(got) != 1 || got[0].ObjectType != "partner.business_partner" || got[0].ObjectID == nil || got[0].ObjectID.String() != id(p) {
		t.Errorf("update audit: got %+v, want one naming partner %s", got, id(p))
	}
	e.Must(201, "POST", "/v1/partner-changes", e.update(id(p), `{"extensions": {"segment": "retail"}}`), "")
}
