package modelalias_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/modelalias"
)

func TestKnown(t *testing.T) {
	for alias, want := range map[string]bool{"erp-default": true, "erp-extraction": true, "erp-analytics": true, "claude-opus-5-5": false, "": false} {
		if got := modelalias.Known(alias); got != want {
			t.Errorf("%q: got %v, want %v", alias, got, want)
		}
	}
}
