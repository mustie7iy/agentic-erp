package i18n

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/text/language"
)

// keys are the Key constants of keys.go.
func keys(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "keys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if spec, ok := n.(*ast.ValueSpec); ok {
			for _, v := range spec.Values {
				lit := v.(*ast.BasicLit)
				k, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, k)
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

var verbRE = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)

// texts are a message's texts: the string, or each plural form.
func texts(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var forms map[string]string
	if err := json.Unmarshal(raw, &forms); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range forms {
		out = append(out, v)
	}
	return out
}

// TestCatalogsComplete: every shipped locale file has exactly the keys of
// keys.go, each with English's verbs (in any order), so no page shows a
// key or drops an argument in any language.
func TestCatalogsComplete(t *testing.T) {
	want := keys(t)
	if len(want) == 0 {
		t.Fatal("keys.go: no keys found")
	}
	names, err := fs.Glob(files, "locales/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"locales/en.json", "locales/fr-CA.json"}) {
		t.Fatalf("locale files: got %v, want English and French (Canada) (ADR-0052 P5)", names)
	}
	_, english, err := read(files, "locales/en.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, msgs, err := read(files, name)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for k := range msgs {
				got = append(got, k)
			}
			sort.Strings(got)
			if !slices.Equal(got, want) {
				t.Fatalf("keys: got %v, want %v", got, want)
			}
			for k, raw := range msgs {
				for _, text := range texts(t, raw) {
					// A language may put indexed verbs in its own order.
					g, w := verbRE.FindAllString(text, -1), verbRE.FindAllString(texts(t, english[k])[0], -1)
					slices.Sort(g)
					slices.Sort(w)
					if !slices.Equal(g, w) {
						t.Errorf("%s: verbs %v, want English's %v", k, g, w)
					}
				}
			}
		})
	}
}

func TestLoad(t *testing.T) {
	c, err := Load(true)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Shipped(); !slices.Equal(got, []language.Tag{language.English, language.CanadianFrench}) {
		t.Errorf("shipped: got %v, want en, fr-CA", got)
	}
	en, fr := c.Match("en"), c.Match("fr")
	for name, tc := range map[string]struct{ got, want string }{
		"English":           {en.T(NavHome), "Home"},
		"French":            {fr.T(NavHome), "Accueil"},
		"plural one":        {en.T(HomeJobsCount, 1), "1 job is planned for the pilot."},
		"plural other":      {en.T(HomeJobsCount, 7), "7 jobs are planned for the pilot."},
		"French plural one": {fr.T(HomeJobsCount, 1), "1 tâche est prévue pour le projet pilote."},
		"format":            {en.T(PageTitle, "A", "B"), "A – B"},
		"long text":         {c.Match("en-XA").T(NavHome), "⟦Ĥöɱé ··⟧"},
		"long text plural":  {c.Match("en-XA").T(HomeJobsCount, 2), "⟦2 ĵöƀš áŕé þļáññéð ƒöŕ ţĥé þîļöţ. ············⟧"},
		"right to left":     {c.Match("ar-XB").T(NavHome), RTLOpen + "Home" + RTLClose},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	on, err := Load(true)
	if err != nil {
		t.Fatal(err)
	}
	off, err := Load(false)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		c       *Catalog
		header  string
		tag     language.Tag
		dir     string
		comment string
	}{
		"empty":                {on, "", language.English, "ltr", "the default"},
		"malformed":            {on, "en;q=x;;", language.English, "ltr", "the default"},
		"French":               {on, "fr-FR,fr;q=0.9,en;q=0.5", language.CanadianFrench, "ltr", "French matches French (Canada)"},
		"unknown":              {on, "de-DE", language.English, "ltr", "no confidence: the default"},
		"long text":            {on, "en-XA", Long, "ltr", "exact pseudo tag"},
		"right to left":        {on, "ar-XB, en", RTL, "rtl", "exact pseudo tag"},
		"Arabic":               {on, "ar", language.English, "ltr", "Arabic never matches the pseudo-locale"},
		"pseudo off":           {off, "ar-XB", language.English, "ltr", "pseudo-locales are off in production"},
		"pseudo second choice": {on, "de, en-XA", language.English, "ltr", "only a first choice picks a pseudo-locale"},
	} {
		t.Run(name, func(t *testing.T) {
			l := tc.c.Match(tc.header)
			if l.Tag != tc.tag {
				t.Errorf("tag: got %v, want %v (%s)", l.Tag, tc.tag, tc.comment)
			}
			if l.Dir != tc.dir {
				t.Errorf("dir: got %v, want %v", l.Dir, tc.dir)
			}
		})
	}
}

func TestDirection(t *testing.T) {
	for tag, want := range map[string]string{"ar": "rtl", "he-IL": "rtl", "fr-CA": "ltr", "en": "ltr", "ar-XB": "rtl"} {
		t.Run(tag, func(t *testing.T) {
			if got := direction(language.MustParse(tag)); got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}

func TestLoadRefuses(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	en := file(`{"a": "A"}`)
	for name, tc := range map[string]struct {
		fsys fstest.MapFS
		want string
	}{
		"bad name":        {fstest.MapFS{"locales/en.json": en, "locales/not a tag!.json": file(`{}`)}, "not named for a language tag"},
		"bad JSON":        {fstest.MapFS{"locales/en.json": file(`{`)}, "locales/en.json"},
		"bad message":     {fstest.MapFS{"locales/en.json": file(`{"a": 1}`)}, "a: want a string or plural forms"},
		"plural no other": {fstest.MapFS{"locales/en.json": file(`{"a": {"one": "x"}}`)}, "a: want a string or plural forms"},
		"no English":      {fstest.MapFS{"locales/fr.json": en}, "no English catalog"},
		"form not CLDR's": {fstest.MapFS{"locales/en.json": file(`{"a": {"few": "x", "other": "y"}}`)}, "a: plural"},
		"unreadable":      {fstest.MapFS{"locales/en.json": &fstest.MapFile{Mode: fs.ModeDir}}, "i18n:"},
		"bad pattern":     {nil, "i18n:"},
	} {
		t.Run(name, func(t *testing.T) {
			var fsys fs.FS = tc.fsys
			if tc.fsys == nil {
				fsys = badGlob{}
			}
			_, err := load(fsys, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// badGlob fails every Glob.
type badGlob struct{ fs.FS }

func (badGlob) Glob(string) ([]string, error) { return nil, fs.ErrInvalid }

func TestLongText(t *testing.T) {
	for in, want := range map[string]string{
		"":             "⟦⟧",
		"Ab":           "⟦Åƀ ·⟧",
		"%s – %s":      "⟦%s – %s ···⟧",
		"%[2]d of %%x": "⟦%[2]d öƒ %%ẋ ····⟧",
	} {
		t.Run(in, func(t *testing.T) {
			if got := longText(in); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestContext(t *testing.T) {
	c, err := Load(false)
	if err != nil {
		t.Fatal(err)
	}
	if got := T(context.Background(), NavHome); got != string(NavHome) {
		t.Errorf("without a locale: got %q, want the key", got)
	}
	ctx := With(context.Background(), c.Match("fr"))
	if got := T(ctx, NavHome); got != "Accueil" {
		t.Errorf("with French: got %q, want Accueil", got)
	}
	if l, ok := From(ctx); !ok || l.Tag != language.CanadianFrench {
		t.Errorf("From: got %v %v, want fr-CA", l.Tag, ok)
	}
}

// TestMoney: amounts in each locale's digits, grouping and decimal sign,
// with the currency's code in the locale's order, from the API's decimal
// strings; anything else as it is.
func TestMoney(t *testing.T) {
	c, err := Load(true)
	if err != nil {
		t.Fatal(err)
	}
	en, fr, rtl := c.Match("en"), c.Match("fr-CA"), c.Match("ar-XB")
	for name, tc := range map[string]struct{ got, want string }{
		"English":                {en.Money("1284330.00", "EUR"), "EUR 1,284,330.00"},
		"French (Canada)":        {fr.Money("1284330.05", "CAD"), "1 284 330,05 CAD"},
		"no minor unit":          {en.Money("15000", "JPY"), "JPY 15,000"},
		"three places":           {en.Money("1.250", "KWD"), "KWD 1.250"},
		"negative":               {en.Money("-5.07", "EUR"), "EUR -5.07"},
		"the largest":            {en.Decimal("999999999999999999.9999"), "999,999,999,999,999,999.9999"},
		"right to left's digits": {rtl.Decimal("1234.05"), "١٬٢٣٤٫٠٥"},
		"not a decimal":          {en.Decimal("1e5"), "1e5"},
		"no whole part":          {en.Decimal(".5"), ".5"},
		"no fraction":            {en.Decimal("5."), "5."},
		"a sign inside":          {en.Decimal("+5"), "+5"},
		"too many digits":        {en.Decimal("1234567890123456789"), "1234567890123456789"},
		"too long a fraction":    {en.Decimal("1.1234567890123456789"), "1.1234567890123456789"},
		"empty":                  {en.Decimal(""), ""},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}
