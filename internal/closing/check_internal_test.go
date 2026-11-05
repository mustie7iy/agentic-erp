package closing

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestChecksRun: the checks a template may name (config.CloseChecks) are
// the ones run runs, so no template accepts a check task that then fails
// as one this build does not run.
func TestChecksRun(t *testing.T) {
	if runs := []string{CheckNoParked, CheckRevaluation, CheckVariances, CheckDepreciation}; !slices.Equal(config.CloseChecks, runs) {
		t.Errorf("config.CloseChecks: got %v, want %v, the checks run runs", config.CloseChecks, runs)
	}
}

// TestAssetCount: the depreciation check's summary counts one asset in
// the singular and more in the plural.
func TestAssetCount(t *testing.T) {
	for n, want := range map[int]string{1: "1 asset to depreciate", 2: "2 assets to depreciate"} {
		t.Run(want, func(t *testing.T) {
			if got := assetCount(n); got != want {
				t.Errorf("assetCount(%d): got %q, want %q", n, got, want)
			}
		})
	}
}

// TestLateMessage: a depreciation_catch_up warning names the periods an
// asset has no depreciation booked for and its catch-up; without a first
// missing period (an acquisition reversed after depreciation posted) it
// says what is booked differs from the plan and names the difference.
func TestLateMessage(t *testing.T) {
	w := foundWarning{Code: warnCatchUp, LedgerCode: "0L", AssetNumber: "7", AssetDescription: "Drill", MissingThrough: [2]int16{2026, 2},
		CatchUp: "20.00", Currency: "EUR"}
	from := w
	from.MissingFrom = &[2]int16{2025, 12}
	w.CatchUp = "-5.00"
	cases := map[string]struct {
		w    foundWarning
		want string
	}{
		"missing from a period": {from, "Asset 7 (Drill) in ledger 0L has no depreciation booked from 2025/12 through 2026/2, whose run posted " +
			"before it; a run of 2026/2 or later catches up 20.00 EUR."},
		"booked differs from the plan": {w, "Asset 7 (Drill) in ledger 0L differs from its depreciation planned through 2026/2, whose run posted " +
			"before it; a run of 2026/2 or later posts the difference, -5.00 EUR."},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.w.message(); got != c.want {
				t.Errorf("message: got %q, want %q", got, c.want)
			}
		})
	}
}
