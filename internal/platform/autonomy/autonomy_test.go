package autonomy_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/autonomy"
)

func TestKnownType(t *testing.T) {
	for typ, want := range map[string]bool{"ap_exceptions": true, "analytics": true, "capture": false, "": false} {
		if got := autonomy.KnownType(typ); got != want {
			t.Errorf("%q: got %v, want %v", typ, got, want)
		}
	}
}

func TestKind(t *testing.T) {
	for typ, want := range map[string]string{"ap_exceptions": "ap_exceptions", "retired": autonomy.Untyped, "": autonomy.Untyped} {
		t.Run(typ, func(t *testing.T) {
			if got := autonomy.Kind(typ); got != want {
				t.Errorf("kind of %q: got %q, want %q", typ, got, want)
			}
		})
	}
	if got, want := len(autonomy.Kinds), len(autonomy.Types)+2; got != want {
		t.Errorf("kinds: got %d, want %d", got, want)
	}
}
