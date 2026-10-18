package redact_test

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/redact"
)

// FuzzText: any text, the model's prompts and tool results being
// untrusted, redacts without failing; a text left unchanged counts no
// replacement, and redacting the result again replaces nothing more.
func FuzzText(f *testing.F) {
	for _, s := range []string{"DE89 3704 0044 0532 0130 00", "4111-1111-1111-1111", "DE123456789", "12-3456789", "", strings.Repeat("1 ", 200), strings.Repeat("AA11 ", 200)} {
		f.Add(s)
	}
	r := redact.For([]string{"AE", "AU", "BR", "CA", "CH", "CN", "DE", "DK", "ES", "FR", "GB", "IE", "IN", "IT", "JP", "MX", "NL", "NO", "PL",
		"SA", "SE", "SG", "US", "ZA"})
	f.Fuzz(func(t *testing.T, s string) {
		out, n := r.Text(s)
		if n == 0 && out != s {
			t.Fatalf("no replacement counted, but %q became %q", s, out)
		}
		if n > 0 && !strings.Contains(out, "[") {
			t.Fatalf("%d replacements counted, but %q has no placeholder", n, out)
		}
		if again, m := r.Text(out); m != 0 {
			t.Fatalf("redacting %q again replaced %d more: %q", out, m, again)
		}
	})
}
