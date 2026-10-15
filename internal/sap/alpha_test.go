package sap

import "testing"

func TestAlpha(t *testing.T) {
	for in, want := range map[string]string{"0000100001": "100001", "0000000000": "0", "": "", "ABC": "ABC"} {
		t.Run(in, func(t *testing.T) {
			if got := Alpha(in); got != want {
				t.Errorf("got Alpha(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
