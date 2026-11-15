package regulatoryagent

import "testing"

// TestInNotice: a quotation is in the notice as whole words and numbers,
// never a fragment of one, and never empty.
func TestInNotice(t *testing.T) {
	const notice = "From 1 July 2099 tax code V0, input tax exempt, rises to 2.5 percent. Its 1,200 cases."
	for q, want := range map[string]bool{
		"From 1 July 2099":     true,
		"rises to 2.5 percent": true,
		"2.5 percent.":         true,
		"V0":                   true,
		"tax":                  true,
		"Its 1,200 cases.":     true,
		"5 percent":            false,
		".5 percent":           false,
		"2.":                   false,
		"9":                    false,
		"July 20":              false,
		"ises to":              false,
		"200 cases":            false,
		"1,":                   false,
		"":                     false,
		"rises to 3 percent":   false,
	} {
		t.Run(q, func(t *testing.T) {
			if got := inNotice(notice, q); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

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
