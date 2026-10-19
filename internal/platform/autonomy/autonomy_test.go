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
