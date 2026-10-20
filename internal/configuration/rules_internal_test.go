package configuration

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// TestRequireRulesSimulated: a change of rules activates only with a
// passed simulation naming its exact proposal, as does a change of the
// custom fields of an entity the proposal's rules judge; other changes
// need none.
func TestRequireRulesSimulated(t *testing.T) {
	proposal := json.RawMessage(`{"rules": {}}`)
	detail := func(hash string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"proposal_sha256": hash})
		return b
	}
	ok := Evidence{Kind: "simulation", Passed: true, Detail: detail(proposalHash(proposal))}
	rulesItem := []Item{{Path: "rules.business_partner.x.expression"}}
	proposed := &config.Config{Rules: map[string]map[string]rules.Rule{"business_partner": {"x": {}}}}
	for name, c := range map[string]struct {
		items    []Item
		evidence []Evidence
		refused  bool
	}{
		"not a rules change":   {items: []Item{{Path: "companies[1000].tolerances"}}},
		"fields without rules": {items: []Item{{Path: "extensions.purchase_order.properties.x"}}},
		"fields rules read":    {items: []Item{{Path: "extensions.business_partner.properties.x"}}, refused: true},
		"simulated":            {items: rulesItem, evidence: []Evidence{{Kind: "regression", Passed: true, Detail: ok.Detail}, ok}},
		"not simulated":        {items: rulesItem, refused: true},
		"simulation failed":    {items: rulesItem, evidence: []Evidence{{Kind: "simulation", Detail: ok.Detail}}, refused: true},
		"another proposal":     {items: rulesItem, evidence: []Evidence{{Kind: "simulation", Passed: true, Detail: detail("other")}}, refused: true},
		"detail not readable":  {items: rulesItem, evidence: []Evidence{{Kind: "simulation", Passed: true, Detail: json.RawMessage(`[]`)}}, refused: true},
		"regression only":      {items: rulesItem, evidence: []Evidence{{Kind: "regression", Passed: true, Detail: ok.Detail}}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := requireRulesSimulated(ChangeRequest{Proposed: proposal, Items: c.items, Evidence: c.evidence}, proposed)
			var p *problem.Problem
			if got := errors.As(err, &p) && p.Code == "config.rules_not_simulated"; got != c.refused {
				t.Errorf("refused: got %v, want %t", err, c.refused)
			}
		})
	}
}

// TestReplaySummary: a sample with no writable row says the replay shows
// nothing, so that "0 newly refused" does not read as harmless.
func TestReplaySummary(t *testing.T) {
	for name, c := range map[string]struct {
		r       rules.Replay
		nothing bool
	}{
		"all historical": {rules.Replay{Entity: "supplier_invoice", Rows: 2, Historical: 2}, true},
		"empty":          {rules.Replay{Entity: "supplier_invoice"}, true},
		"some writable":  {rules.Replay{Entity: "supplier_invoice", Rows: 2, Historical: 1}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := replaySummary(c.r)
			if got := strings.Contains(s, "shows nothing"); got != c.nothing {
				t.Errorf("summary %q says nothing was shown: got %t, want %t", s, got, c.nothing)
			}
		})
	}
}
