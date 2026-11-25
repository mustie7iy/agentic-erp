// Package langtag checks and matches the language tags (BCP 47, RFC 5646)
// the API keeps texts by: a material's descriptions (ADR-0082 As built
// S1a2). A stored tag is a language with an optional script and region, in
// its canonical form, its subtags known to the IANA language subtag
// registry as golang.org/x/text/language holds it, as the web workspace's
// locales are (internal/web/i18n). A business partner's ISO 639-1
// correspondence language is such a tag.
package langtag

import (
	"regexp"
	"strings"

	"golang.org/x/text/language"
)

// Pattern is a stored tag's form: a language subtag of two or three
// letters, then an optional script and an optional region, each in its
// canonical case (ar, ar-AE, zh-Hant, es-419). The contract and the
// database check it too.
const Pattern = `^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`

var form = regexp.MustCompile(Pattern)

// Canonical is the tag in its canonical form, and whether that form is one
// a text is kept by: of Pattern's form, every subtag known. A deprecated
// subtag is replaced (iw is he), the case is canonical (EN is en), and an
// extension or private use subtag is no such form; an ill-formed tag or an
// unknown subtag answers "" and false.
func Canonical(tag string) (string, bool) {
	t, err := language.Parse(tag)
	if err != nil {
		return "", false
	}
	c := t.String()
	return c, form.MatchString(c)
}

// Lookup is RFC 4647 §3.4's lookup of the tag among have: the tag itself,
// then the tag with its last subtag dropped, and so on, the first in have;
// false when none is (zh-Hant-TW finds zh-Hant, then zh).
func Lookup(tag string, have []string) (string, bool) {
	for t := tag; t != ""; {
		for _, h := range have {
			if h == t {
				return h, true
			}
		}
		i := strings.LastIndexByte(t, '-')
		if i < 0 {
			break
		}
		t = t[:i]
	}
	return "", false
}
