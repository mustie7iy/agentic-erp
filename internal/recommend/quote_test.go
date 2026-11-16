package recommend_test

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// TestQuoted: a quotation is in the notice as whole words and numbers,
// never a fragment of one, and never empty.
func TestQuoted(t *testing.T) {
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
			if got := recommend.Quoted(notice, q); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

// TestQuotations: the passages between straight or typographic double
// quotes, empty ones too, and the text without them; one left open runs
// to the end.
func TestQuotations(t *testing.T) {
	quoted, rest := recommend.Quotations(`It says "a 2.5 rate", “from July” and "" then "open`)
	if got, want := strings.Join(quoted, "|"), "a 2.5 rate|from July||open"; got != want {
		t.Errorf("quoted: got %q, want %q", got, want)
	}
	if want := "It says ,  and  then "; rest != want {
		t.Errorf("rest: got %q, want %q", rest, want)
	}
}

// TestUnnumbered: a list item's number at a line's start is taken out; a
// number of three digits there, or one inside a line, is kept.
func TestUnnumbered(t *testing.T) {
	got := recommend.Unnumbered("1. First\n  12) Twelfth\n100) Not a list\nAt 7. five")
	if want := " First\n Twelfth\n100) Not a list\nAt 7. five"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
