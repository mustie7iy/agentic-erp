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
		t.Run(header, func(t *testing.T) {
			got := 0
			if err := Check(header, 3); err != nil {
				got = problem.From(err).Status
			}
			if got != want {
				t.Errorf("Check(%q, 3): got %d, want %d", header, got, want)
			}
		})
	}
}

// TestIfMatch: the bound check answers as Check does for the header.
func TestIfMatch(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		version int32
		want    int // HTTP status; 0 means it matches
	}{
		{"matching version", `"3"`, 3, 0},
		{"other version", `"3"`, 4, 412},
		{"no header", "", 3, 428},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := 0
			if err := IfMatch(c.header)(c.version); err != nil {
				got = problem.From(err).Status
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestNoneMatch(t *testing.T) {
	for header, want := range map[string]bool{
		``:         true,
		`"3"`:      false,
		`W/"3"`:    false, // compared weakly
		`"1", "3"`: false,
		`*`:        false,
		`"2"`:      true,
		`3`:        true,
	} {
		t.Run(header, func(t *testing.T) {
			if got := NoneMatch(header, `"3"`); got != want {
				t.Errorf("NoneMatch(%q, \"3\"): got %v, want %v", header, got, want)
			}
		})
	}
}
