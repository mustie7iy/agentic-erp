package langtag_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/langtag"
)

// TestCanonical: tags of a language, script and region with known
// subtags are kept as they are; others are answered in their canonical
// form, or refused when ill-formed, unknown or beyond the stored form.
func TestCanonical(t *testing.T) {
	for in, want := range map[string]struct {
		canonical string
		ok        bool
	}{
		"en":         {"en", true},
		"ar":         {"ar", true},
		"ar-AE":      {"ar-AE", true},
		"zh-Hant":    {"zh-Hant", true},
		"zh-Hant-TW": {"zh-Hant-TW", true},
		"es-419":     {"es-419", true},
		"und":        {"und", true},
		"EN":         {"en", true},
		"iw":         {"he", true},
		"zz":         {"", false},
		"xx-XX":      {"", false},
		"":           {"", false},
		"en_US":      {"en-US", true},
		"en-US-x-a":  {"en-US-x-a", false},
		"de-1996":    {"de-1996", false},
	} {
		t.Run(in, func(t *testing.T) {
			got, ok := langtag.Canonical(in)
			if got != want.canonical {
				t.Errorf("canonical: got %q, want %q", got, want.canonical)
			}
			if ok != want.ok {
				t.Errorf("ok: got %v, want %v", ok, want.ok)
			}
		})
	}
}

// TestLookup: the tag itself, then without its last subtag, until one is
// had; none had answers false.
func TestLookup(t *testing.T) {
	have := []string{"en", "ar", "zh-Hant", "fr-CA"}
	for in, want := range map[string]string{
		"ar":         "ar",
		"ar-AE":      "ar",
		"zh-Hant-TW": "zh-Hant",
		"zh-Hans":    "",
		"fr-CA":      "fr-CA",
		"fr":         "",
		"de":         "",
		"":           "",
	} {
		t.Run(in, func(t *testing.T) {
			got, ok := langtag.Lookup(in, have)
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
			if ok != (want != "") {
				t.Errorf("ok: got %v, want %v", ok, want != "")
			}
		})
	}
}
