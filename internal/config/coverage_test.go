package config

import (
	"strings"
	"testing"
)

func TestTierNamesAndDecimals(t *testing.T) {
	if TierCosmetic.String() != "tier 1 (cosmetic)" || TierOperational.String() != "tier 2 (operational)" || TierPostingRelated.String() != "tier 3 (posting-relevant)" {
		t.Fatal("tier names")
	}
	if decimalsFor("JPY") != 0 || decimalsFor("KWD") != 3 || decimalsFor("USD") != 2 {
		t.Fatal("decimals")
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
