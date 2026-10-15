package config

import (
	"strings"
	"testing"
)

func TestTierNamesAndChanges(t *testing.T) {
	for tier, want := range map[Tier]string{TierCosmetic: "tier 1 (cosmetic)", TierOperational: "tier 2 (operational)", TierPostingRelated: "tier 3 (posting-relevant)"} {
		t.Run(want, func(t *testing.T) {
			if got := tier.String(); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	if c := (Change{Path: "p", New: "x"}); !strings.HasPrefix(c.String(), "+") {
		t.Fatal("added change")
	}
	if c := (Change{Path: "p", Old: "a", New: "b"}); !strings.HasPrefix(c.String(), "~") {
		t.Fatal("modified change")
	}
	// Diff against nothing lists every setting as added.
	changes, err := Diff(nil, fixture(t))
	if err != nil || len(changes) == 0 {
		t.Fatal("diff against nil")
	}
}
