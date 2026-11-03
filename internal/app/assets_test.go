package app

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// TestDepreciateKind: the workflows dispatcher, which may not import the
// assets leaf (ADR-0057 §2), names a depreciation run's Operation kind as
// the assets context writes it, and the assets context implements the
// workflow's port.
func TestDepreciateKind(t *testing.T) {
	if workflows.DepreciateKind != assets.KindDepreciate {
		t.Errorf("got %s, want %s", workflows.DepreciateKind, assets.KindDepreciate)
	}
	var _ workflows.Depreciation = Assets(Config{})
}
