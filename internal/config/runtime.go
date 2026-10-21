package config

import (
	"slices"
	"strings"
)

// ApprovalSteps for a configuration change of the given tier. Tier 1 needs
// no approval; higher tiers need more, and more senior, approvers. The
// roles are identity roles with these codes.
func ApprovalSteps(t Tier) []ApprovalStep {
	switch t {
	case TierCosmetic: // no approval
	case TierOperational:
		return []ApprovalStep{{Role: "process_owner", Approvals: 1}}
	case TierPostingRelated:
		return []ApprovalStep{{Role: "process_owner", Approvals: 1}, {Role: "controller", Approvals: 1}}
	case TierStructural:
		return []ApprovalStep{{Role: "solution_architect", Approvals: 1}, {Role: "controller", Approvals: 1}}
	}
	return nil
}

// RequiredEvidence lists what a tier needs before approval: posting-relevant
// and structural changes need a simulation of recent transactions and the
// regression suite replayed.
func RequiredEvidence(t Tier) []string {
	if t >= TierPostingRelated {
		return []string{"simulation", "regression"}
	}
	return nil
}

// Evidence is what a change set needs before approval: its tier's
// (RequiredEvidence), and a simulation when it changes what a metric
// means, which shows the approvers the affected metrics' values before
// and after on recent data (ADR-0044, T55g).
func Evidence(changes []Change) []string {
	out := RequiredEvidence(MaxTier(changes))
	if slices.Contains(out, "simulation") {
		return out
	}
	for _, c := range changes {
		if strings.HasPrefix(c.Path, "analytics.") && c.Tier > TierCosmetic {
			return append(out, "simulation")
		}
	}
	return out
}

// RoleNames are the roles the document's approval policies name, which
// must exist as identity roles for requests to be routed.
func (c *Config) RoleNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	tiers := [][]ApprovalTier{c.Purchasing.RequisitionApproval, c.Purchasing.OrderReleaseApproval}
	for _, co := range c.Companies {
		tiers = append(tiers, co.BankStatements.ApprovalTiers) // approvals.BankClearing (ADR-0047)
	}
	for _, tiers := range tiers {
		for _, t := range tiers {
			for _, s := range t.Steps {
				add(s.Role)
			}
		}
	}
	add(c.Payments.ReleaseRole)
	add(c.Payments.VendorApproverRole)
	for _, t := range []Tier{TierOperational, TierPostingRelated, TierStructural} {
		for _, s := range ApprovalSteps(t) {
			add(s.Role)
		}
	}
	return out
}
