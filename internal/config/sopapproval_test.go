package config

import "testing"

// TestSopApprovalTier (ADR-0084 As built S2n2): who approves a company's
// S&OP plan versions posts nothing and is operational, as purchasing's
// approval tiers are.
func TestSopApprovalTier(t *testing.T) {
	base := withCash(t)
	c := withCash(t)
	c.Companies[0].SopApproval.Steps = []ApprovalStep{{Role: "controller", Approvals: 1}}
	changes, _ := Diff(base, c) // Diff never fails
	if len(changes) == 0 {
		t.Fatal("changes: got none, want the steps")
	}
	for _, ch := range changes {
		if ch.Tier != TierOperational {
			t.Errorf("%s: got tier %v, want operational", ch.Path, ch.Tier)
		}
	}
}
