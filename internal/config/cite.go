package config

import "regexp"

// Citation is a setting a text cites as `path` = `value` (the support and
// enablement agents' replies, ADR-0072 As built), and the citation as the
// text writes it.
type Citation struct {
	Path, Value, Text string
}

// citation is a setting cited as `path` = `value`: a path holds no space
// or backtick, a value no backtick.
var citation = regexp.MustCompile("`([^`\\s]+)` = `([^`]*)`")

// Cited are the settings a text cites as `path` = `value`, in its order. A
// setting written any other way is not seen: an agent that cites settings
// is told this form, and its check holds every citation to the
// configuration (Setting).
func Cited(text string) []Citation {
	var out []Citation
	for _, m := range citation.FindAllStringSubmatch(text, -1) {
		out = append(out, Citation{Path: m[1], Value: m[2], Text: m[0]})
	}
	return out
}

// Uncited is the text with every citation replaced by a space: what an
// agent's check reads for the numbers it writes outside them.
func Uncited(text string) string { return citation.ReplaceAllString(text, " ") }

// Unheld is the first setting a text cites that the configuration does not
// hold with that value, and whether there is one: an agent's check refuses
// a text citing it (the support agent's reply, the enablement agent's
// description and help).
func Unheld(c *Config, text string) (Citation, bool) {
	for _, s := range Cited(text) {
		if v, ok := Setting(c, s.Path); !ok || v != s.Value {
			return s, true
		}
	}
	return Citation{}, false
}
