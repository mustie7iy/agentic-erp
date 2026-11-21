package einvoicing

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClipDetail: a hold's detail is kept within the column's 2,000
// characters, a shorter one as it is.
func TestClipDetail(t *testing.T) {
	for name, c := range map[string]struct {
		in   string
		want int
	}{"short": {"No order.", 9}, "at the limit": {strings.Repeat("é", 2000), 2000}, "too long": {strings.Repeat("é", 2500), 2000}} {
		t.Run(name, func(t *testing.T) {
			if got := utf8.RuneCountInString(clipDetail(c.in)); got != c.want {
				t.Errorf("got %d characters, want %d", got, c.want)
			}
		})
	}
}
