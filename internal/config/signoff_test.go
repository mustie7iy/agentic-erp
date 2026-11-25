package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSignOffSeparation: a company's closes separate the signer from the
// preparers unless it sets sign_off_separation false (ADR-0058 As built
// T64o): left out, as a company added to a configuration has it, it is
// on.
func TestSignOffSeparation(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{"left out", `accrual_account: "211000"`, true},
		{"on", `sign_off_separation: true`, true},
		{"off, as the pilot runs", `sign_off_separation: false`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c CompanyClose
			if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
				t.Fatalf("unmarshal %q: %v", tc.yaml, err)
			}
			if got := c.SeparatesSignOff(); got != tc.want {
				t.Errorf("SeparatesSignOff of %q: got %v, want %v", tc.yaml, got, tc.want)
			}
		})
	}
}

// TestSignOffSeparationTier: switching the separation off or on is an
// authorization rule: tier 3, as the dual release limit is.
func TestSignOffSeparationTier(t *testing.T) {
	off := false
	base := fixture(t)
	changed := fixture(t)
	changed.Company("1000").Close.SignOffSeparation = &off
	diffs, _ := Diff(base, changed) // Diff never fails
	if len(diffs) != 1 {
		t.Fatalf("Diff: got %v, want the separation alone", diffs)
	}
	if d := diffs[0]; d.Path != "companies[1000].close.sign_off_separation" || d.Tier != TierPostingRelated {
		t.Errorf("Diff: got %s tier %v, want companies[1000].close.sign_off_separation tier %v", d.Path, d.Tier, TierPostingRelated)
	}
}
