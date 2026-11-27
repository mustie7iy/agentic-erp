package integration

import (
	"fmt"
	"strings"
	"testing"
)

// TestMaterialDescriptions (S1a2): a material's MAKT texts load as its
// default description in the picked text's language and further
// descriptions by language; a key with no tag, a second key of one
// language and a blank text are left out, the first two with a warning,
// and a default in a key with no tag is und.
func TestMaterialDescriptions(t *testing.T) {
	for name, c := range map[string]struct {
		texts    []sapText
		picked   string
		language string
		further  string
		warnings string
	}{
		"English with Arabic and German": {[]sapText{{lang: "E", name: "Bracket"}, {lang: "D", name: "Winkel"}, {lang: "A", name: " دعامة "}}, "E",
			"en", "ar دعامة; de Winkel", ""},
		"two letter keys": {[]sapText{{lang: "EN", name: "Bracket"}, {lang: "AR", name: "دعامة"}, {lang: "FR", name: "  "}}, "EN", "en", "ar دعامة", ""},
		"a key with no tag": {[]sapText{{lang: "E", name: "Bracket"}, {lang: "Z", name: "Zed"}}, "E", "en", "",
			"MAKT M-1/Z: language key Z has no language tag; the description \"Zed\" is left out"},
		"two keys of one language": {[]sapText{{lang: "E", name: "Bracket"}, {lang: "EN", name: "Bracket too"}}, "E", "en", "",
			"MAKT M-1/EN: language key EN is en, which another description has; the description \"Bracket too\" is left out"},
		"a default in a key with no tag": {[]sapText{{lang: "Z", name: "Zed"}}, "Z", "und", "",
			"MAKT M-1/Z: language key Z has no language tag; the default description loads in und (undetermined)"},
	} {
		t.Run(name, func(t *testing.T) {
			mp := &materialPlan{texts: map[string][]sapText{"M-1": c.texts}}
			var picked sapText
			for _, x := range c.texts {
				if x.lang == c.picked {
					picked = x
				}
			}
			language, ds := mp.materialDescriptions("M-1", picked)
			if language != c.language {
				t.Errorf("language: got %q, want %q", language, c.language)
			}
			var got []string
			for _, d := range ds {
				got = append(got, d.Language+" "+d.Text)
			}
			if g := strings.Join(got, "; "); g != c.further {
				t.Errorf("further: got %q, want %q", g, c.further)
			}
			var warnings []string
			for _, i := range mp.issues {
				warnings = append(warnings, fmt.Sprintf("%s %s: %s", i.Table, i.Key, i.Message))
			}
			if g := strings.Join(warnings, "\n"); g != c.warnings {
				t.Errorf("warnings: got %q, want %q", g, c.warnings)
			}
		})
	}
}
