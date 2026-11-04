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
	if runs := []string{CheckNoParked, CheckRevaluation, CheckDepreciation}; !slices.Equal(config.CloseChecks, runs) {
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
