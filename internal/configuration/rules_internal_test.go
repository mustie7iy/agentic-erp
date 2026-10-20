package configuration

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestRequireRulesSimulated: a change of rules activates only with a
// passed simulation naming its exact proposal; other changes need none.
func TestRequireRulesSimulated(t *testing.T) {
	proposal := json.RawMessage(`{"rules": {}}`)
	detail := func(hash string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"proposal_sha256": hash})
		return b
	}
	ok := Evidence{Kind: "simulation", Passed: true, Detail: detail(proposalHash(proposal))}
	rulesItem := []Item{{Path: "rules.business_partner.x.expression"}}
	for name, c := range map[string]struct {
		items    []Item
		evidence []Evidence
		refused  bool
	}{
		"not a rules change":  {items: []Item{{Path: "companies[1000].tolerances"}}},
		"simulated":           {items: rulesItem, evidence: []Evidence{{Kind: "regression", Passed: true, Detail: ok.Detail}, ok}},
		"not simulated":       {items: rulesItem, refused: true},
		"simulation failed":   {items: rulesItem, evidence: []Evidence{{Kind: "simulation", Detail: ok.Detail}}, refused: true},
		"another proposal":    {items: rulesItem, evidence: []Evidence{{Kind: "simulation", Passed: true, Detail: detail("other")}}, refused: true},
		"detail not readable": {items: rulesItem, evidence: []Evidence{{Kind: "simulation", Passed: true, Detail: json.RawMessage(`[]`)}}, refused: true},
		"regression only":     {items: rulesItem, evidence: []Evidence{{Kind: "regression", Passed: true, Detail: ok.Detail}}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := requireRulesSimulated(ChangeRequest{Proposed: proposal, Items: c.items, Evidence: c.evidence})
			var p *problem.Problem
			if got := errors.As(err, &p) && p.Code == "config.rules_not_simulated"; got != c.refused {
				t.Errorf("refused: got %v, want %t", err, c.refused)
			}
		})
	}
}
