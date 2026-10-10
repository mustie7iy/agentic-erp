package etag

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestCheck(t *testing.T) {
	if Format(3) != `"3"` {
		t.Fatal(Format(3))
	}
	for header, want := range map[string]int{
		``:         428,
		`"3"`:      0,
		`*`:        0,
		`"1", "3"`: 0,
		`"2"`:      412,
		`W/"3"`:    412, // weak tags never match a strong comparison
		`3`:        412,
	} {
		err := Check(header, 3)
		got := 0
		if err != nil {
			got = problem.From(err).Status
		}
		if got != want {
			t.Errorf("%q: %d, want %d", header, got, want)
		}
	}
}
