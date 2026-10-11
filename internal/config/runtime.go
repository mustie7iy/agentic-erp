package config

// ApprovalSteps for a configuration change of the given tier. Tier 1 needs
// no approval; higher tiers need more, and more senior, approvers. The
// roles are identity roles with these codes.
func ApprovalSteps(t Tier) []ApprovalStep {
	switch t {
	case TierOperational:
		return []ApprovalStep{{Role: "process_owner", Approvals: 1}}
	case TierPostingRelated:
		return []ApprovalStep{{Role: "process_owner", Approvals: 1}, {Role: "controller", Approvals: 1}}
	case TierStructural:
		return []ApprovalStep{{Role: "solution_architect", Approvals: 1}, {Role: "controller", Approvals: 1}}
	}
	return nil
}

// RequiredEvidence lists what a tier needs before approval.
func RequiredEvidence(t Tier) []string {
	if t >= TierPostingRelated {
		return []string{"simulation", "regression"}
	}
	return nil
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
	for _, t := range c.Purchasing.RequisitionApproval {
		for _, s := range t.Steps {
			add(s.Role)
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
