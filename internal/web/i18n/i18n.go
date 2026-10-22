// Package i18n is the web workspace's message catalog (ADR-0052 §9). Every
// string a page shows is a Key, looked up for the person's locale with
// golang.org/x/text/message and CLDR plural rules; the catalogs are the
// JSON files in locales/, one per shipped language, and a test holds each to
// the full list of keys.
//
// Outside production two pseudo-locales are offered as well, made from the
// English catalog: en-XA (accented and about a third longer, so text that
// bypasses the catalog or does not fit stands out) and ar-XB (right to
// left, so the layout is proven mirrored before Arabic is translated). They
// are chosen only by their exact tags, never matched from a real language.
package i18n

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/message/catalog"
)

// Key names a message.
type Key string

//go:embed locales/*.json
var files embed.FS

var (
	// Long is the long-text pseudo-locale.
	Long = language.MustParse("en-XA")
	// RTL is the right-to-left pseudo-locale.
	RTL = language.MustParse("ar-XB")
)

// rtl are the base languages written right to left that the workspace may
// ship (ADR-0052 P5: Arabic when the UAE pilot asks for it).
var rtl = []string{"ar", "fa", "he", "ur"}

// Locale is one language a page is rendered in.
type Locale struct {
	Tag     language.Tag
	Dir     string // "ltr" or "rtl", for the page's dir attribute
	printer *message.Printer
}

// T is the message for k, formatted with args.
func (l Locale) T(k Key, args ...any) string { return l.printer.Sprintf(string(k), args...) }

// Catalog holds the shipped languages and, outside production, the
// pseudo-locales.
type Catalog struct {
	locales map[language.Tag]Locale
	shipped []language.Tag // the first is the default
	matcher language.Matcher
	pseudo  bool
}

// Load builds the catalog from the embedded locale files, with the
// pseudo-locales when pseudo is true (never in production).
func Load(pseudo bool) (*Catalog, error) { return load(files, pseudo) }

// load builds the catalog from fsys's locales/*.json, English first.
func load(fsys fs.FS, pseudo bool) (*Catalog, error) {
	names, err := fs.Glob(fsys, "locales/*.json")
	if err != nil {
		return nil, fmt.Errorf("i18n: %w", err)
	}
	b := catalog.NewBuilder(catalog.Fallback(language.English))
	c := &Catalog{locales: map[language.Tag]Locale{}, pseudo: pseudo}
	var english map[string]json.RawMessage
	for _, name := range names {
		tag, msgs, err := read(fsys, name)
		if err != nil {
			return nil, err
		}
		if err := set(b, tag, msgs, nil); err != nil {
			return nil, fmt.Errorf("i18n: %s: %w", name, err)
		}
		if tag == language.English {
			english = msgs
		}
		c.shipped = append(c.shipped, tag)
	}
	i := slices.Index(c.shipped, language.English)
	if i < 0 {
		return nil, errors.New("i18n: no English catalog (locales/en.json), the default and the pseudo-locales' source")
	}
	c.shipped[0], c.shipped[i] = c.shipped[i], c.shipped[0]
	tags := slices.Clone(c.shipped)
	if pseudo {
		// English parsed above, so its messages convert as well.
		_ = set(b, Long, english, longText)
		_ = set(b, RTL, english, rightToLeft)
		tags = append(tags, Long, RTL)
	}
	for _, tag := range tags {
		c.locales[tag] = Locale{Tag: tag, Dir: direction(tag), printer: message.NewPrinter(tag, message.Catalog(b))}
	}
	c.matcher = language.NewMatcher(c.shipped)
	return c, nil
}

// read decodes one locale file, named for its BCP 47 tag.
func read(fsys fs.FS, name string) (language.Tag, map[string]json.RawMessage, error) {
	tag, err := language.Parse(strings.TrimSuffix(path.Base(name), ".json"))
	if err != nil {
		return language.Und, nil, fmt.Errorf("i18n: %s is not named for a language tag: %w", name, err)
	}
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return language.Und, nil, fmt.Errorf("i18n: %w", err)
	}
	var msgs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return language.Und, nil, fmt.Errorf("i18n: %s: %w", name, err)
	}
	return tag, msgs, nil
}

// set adds msgs to b for tag, each a string or a plural on its first
// argument ({"one": …, "other": …}, CLDR's categories), passed through
// convert when it is not nil.
func set(b *catalog.Builder, tag language.Tag, msgs map[string]json.RawMessage, convert func(string) string) error {
	if convert == nil {
		convert = func(s string) string { return s }
	}
	for key, raw := range msgs {
		msg, err := compose(raw, convert)
		if err == nil {
			// The language's CLDR rules decide which plural forms are
			// valid: English has one and other, not few.
			err = b.Set(tag, key, msg)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// compose is one message of a locale file: a string, or plural forms on
// the first argument in CLDR's order.
func compose(raw json.RawMessage, convert func(string) string) (catalog.Message, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return catalog.String(convert(s)), nil
	}
	var forms map[string]string
	if err := json.Unmarshal(raw, &forms); err != nil || forms["other"] == "" {
		return nil, errors.New(`want a string or plural forms with "other"`)
	}
	var cases []any
	for _, form := range []string{"zero", "one", "two", "few", "many", "other"} {
		if text, ok := forms[form]; ok {
			cases = append(cases, form, convert(text))
		}
	}
	return plural.Selectf(1, "%d", cases...), nil
}

// direction is a language's writing direction.
func direction(tag language.Tag) string {
	if tag == RTL {
		return "rtl"
	}
	base, _ := tag.Base() // every tag has a base, guessed when not given
	if slices.Contains(rtl, base.String()) {
		return "rtl"
	}
	return "ltr"
}

// Shipped are the languages offered to people, the default first.
func (c *Catalog) Shipped() []language.Tag { return slices.Clone(c.shipped) }

// Default is the default locale, English.
func (c *Catalog) Default() Locale { return c.locales[c.shipped[0]] }

// Match is the locale for an Accept-Language header: a pseudo-locale when
// the first language asked for is exactly one and they are on, else the
// best shipped language, else the default.
func (c *Catalog) Match(acceptLanguage string) Locale {
	tags, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(tags) == 0 {
		return c.Default()
	}
	if c.pseudo && (tags[0] == Long || tags[0] == RTL) {
		return c.locales[tags[0]]
	}
	_, i, confidence := c.matcher.Match(tags...)
	if confidence == language.No {
		return c.Default()
	}
	return c.locales[c.shipped[i]]
}

type localeKey struct{}

// With attaches the request's locale.
func With(ctx context.Context, l Locale) context.Context {
	return context.WithValue(ctx, localeKey{}, l)
}

// From is the request's locale; ok is false outside a request that has one.
func From(ctx context.Context) (Locale, bool) {
	l, ok := ctx.Value(localeKey{}).(Locale)
	return l, ok
}

// T is the message for k in the request's locale. Outside a request with
// a locale it is the key itself, which the pseudo-locale tests catch as
// text that did not go through the catalog.
func T(ctx context.Context, k Key, args ...any) string {
	l, ok := From(ctx)
	if !ok {
		return string(k)
	}
	return l.T(k, args...)
}
