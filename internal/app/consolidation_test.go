package app

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// TestConsolidateKind: the workflows dispatcher, which may not import the
// consolidation leaf (ADR-0059 §2), names a consolidation run's Operation
// kind as the consolidation context writes it, and the context implements
// the workflow's port.
func TestConsolidateKind(t *testing.T) {
	if workflows.ConsolidateKind != consolidation.KindRun {
		t.Errorf("got %s, want %s", workflows.ConsolidateKind, consolidation.KindRun)
	}
	var _ workflows.Consolidation = Consolidation(Config{})
}
