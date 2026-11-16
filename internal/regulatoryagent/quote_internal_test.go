package regulatoryagent

import "testing"

// TestTakesEffect: the second line says when, quoted straight or
// typographically, or that the notice does not state it.
func TestTakesEffect(t *testing.T) {
	for content, want := range map[string]bool{
		"Paths: a\nTakes effect: \"1 July\"\nmore": true,
		"Paths: a\nTakes effect: “1 July”":         true,
		"Paths: a\nTakes effect: not stated":       true,
		"Paths: a\nTakes effect: 1 July":           false,
		"Paths: a\nIt takes effect: \"1 July\"":    false,
		"Paths: a":                                 false,
	} {
		t.Run(content, func(t *testing.T) {
			if got := takesEffect(content); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
