package partner_test

import (
	"testing"

	"github.com/google/uuid"

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
	errs, _ := refused["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("create errors: got %v, want one", refused["errors"])
	}
	if got := errs[0].(map[string]any)["field"]; got != "rules.german_segment" {
		t.Errorf("create error field: got %v, want rules.german_segment", got)
	}
	got := fixture.RuleAudits(t, e.ten.ID, "partner.business_partner.create")
	if len(got) != 1 {
		t.Fatalf("create audit records: got %v, want one", got)
	}
	fixture.WantRuleAudit(t, got[0], fixture.RuleAudit{ObjectType: "partner.business_partner", Outcome: "refused", Rule: "german_segment", ConfigVersion: version})
	e.Problem(422, "request.rule_refused", "POST", "/v1/partner-changes", e.update(id(p), `{"name": "Muster AG", "legal_name": "M", "language": "de"}`), "")
	got = fixture.RuleAudits(t, e.ten.ID, "partner.change.request")
	if len(got) != 1 {
		t.Fatalf("update audit records: got %v, want one", got)
	}
	partnerID := uuid.MustParse(id(p))
	fixture.WantRuleAudit(t, got[0], fixture.RuleAudit{ObjectType: "partner.business_partner", Outcome: "refused", Rule: "german_segment", ObjectID: &partnerID, ConfigVersion: version})
	e.Must(201, "POST", "/v1/partner-changes", e.update(id(p), `{"extensions": {"segment": "retail"}}`), "")
}
