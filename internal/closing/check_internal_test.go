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
	if runs := []string{CheckNoParked, CheckRevaluation}; !slices.Equal(config.CloseChecks, runs) {
		t.Errorf("config.CloseChecks: got %v, want %v, the checks run runs", config.CloseChecks, runs)
	}
}
