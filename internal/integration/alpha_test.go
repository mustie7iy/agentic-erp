package integration

import "testing"

func TestAlpha(t *testing.T) {
	for in, want := range map[string]string{"0000100001": "100001", "0000000000": "0", "": "", "ABC": "ABC"} {
		if got := alpha(in); got != want {
			t.Errorf("alpha(%q) = %q, want %q", in, got, want)
		}
	}
}
