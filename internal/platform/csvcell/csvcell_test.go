package csvcell_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/csvcell"
)

// TestSafe: numbers and dates are as they are; every other text is
// quoted, its quotes doubled, so neither a comma nor a semicolon nor a
// line break starts a new cell; a text beginning with a character OWASP
// names (= + - @, tab, carriage return, line feed and their full-width
// forms) is kept as text by a leading apostrophe; OWASP's own example.
func TestSafe(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": `"'=SUM(A1)"`, "+1": `"'+1"`, "-x": `"'-x"`, "@x": `"'@x"`, "\tx": "\"'\tx\"",
		"\rx": "\"'\rx\"", "\nx": "\"'\nx\"", "＝SUM(A1)": "\"'＝SUM(A1)\"", "＋1": "\"'＋1\"", "－x": "\"'－x\"",
		"＠x": "\"'＠x\"", "-2-3": `"'-2-3"`, "-119.00": "-119.00", "42": "42", "2026-04-09": "2026-04-09", "INV-1": `"INV-1"`, "": "",
		"a,=1+2": `"a,=1+2"`, "a;=1+2": `"a;=1+2"`, "a\n=1+2": "\"a\n=1+2\"", `say "hi"`: `"say ""hi"""`,
		`=1+2";=1+2`: `"'=1+2"";=1+2"`, "\xff=": "\"\xff=\""} {
		t.Run(in, func(t *testing.T) {
			if got := csvcell.Safe(in); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestWrite: the byte order mark, then each row's safe cells separated
// by commas, each line ended by a line feed; a writer's failure is
// returned.
func TestWrite(t *testing.T) {
	var b strings.Builder
	if err := csvcell.Write(&b, [][]string{{"name", "amount"}, {"=Acme", "-5.00"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), csvcell.ByteOrderMark+"\"name\",\"amount\"\n\"'=Acme\",-5.00\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if err := csvcell.Write(failing{}, [][]string{{"a"}}); err == nil || !strings.HasSuffix(err.Error(), ": gone") {
		t.Errorf("a failing writer: got %v, want its error", err)
	}
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("gone") }
