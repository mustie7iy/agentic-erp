package recommend

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Quoted reports whether the source (a notice, a recommendation, the
// contract's description) writes the quotation, not empty, at some place
// where it neither starts nor ends inside a word or a number: "5 percent"
// is no quotation of "2.5 percent", nor ".5 percent", nor "9" of "2099".
// The regulatory agent holds its quotations of a notice to it (T107a), the
// enablement agent its quotations of the contract (T109b).
func Quoted(source, q string) bool {
	if q == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(source[from:], q)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(q)
		before := joined(last(source[:start], 2), first(q, 2))
		after := joined(first(source[end:], 2), last(q, 2))
		if !before && !after {
			return true
		}
		_, size := utf8.DecodeRuneInString(source[start:])
		from = start + size
	}
}

// joined reports whether a joint continues a word or a number across it:
// out are the source's two characters beside the quotation, nearest
// first, in its two characters at that end, nearest first (0 where there
// is none). A letter or digit meets a letter or digit, or a decimal point
// or separator stands between digits on either side of the joint.
func joined(out, in [2]rune) bool {
	sep := func(r rune) bool { return r == '.' || r == ',' }
	return word(out[0]) && word(in[0]) ||
		sep(out[0]) && unicode.IsDigit(out[1]) && unicode.IsDigit(in[0]) ||
		sep(in[0]) && unicode.IsDigit(in[1]) && unicode.IsDigit(out[0])
}

// first are a text's first two characters, 0 where it has none.
func first(s string, n int) [2]rune {
	var out [2]rune
	for k := 0; k < n && s != ""; k++ {
		r, size := utf8.DecodeRuneInString(s)
		out[k], s = r, s[size:]
	}
	return out
}

// last are a text's last two characters, the last first, 0 where it has
// none.
func last(s string, n int) [2]rune {
	var out [2]rune
	for k := 0; k < n && s != ""; k++ {
		r, size := utf8.DecodeLastRuneInString(s)
		out[k], s = r, s[:len(s)-size]
	}
	return out
}

// word reports whether a character is part of a word or a number.
func word(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// Quotations are the passages a content writes between double quotes,
// straight ("…") or typographic (“…”), empty ones too, and the content
// without them. A quotation left open runs to the content's end.
func Quotations(content string) (quoted []string, rest string) {
	var b strings.Builder
	for {
		i := strings.IndexAny(content, "\"“")
		if i < 0 {
			b.WriteString(content)
			return quoted, b.String()
		}
		b.WriteString(content[:i])
		opener, closer := `"`, `"`
		if strings.HasPrefix(content[i:], "“") {
			opener, closer = "“", "”"
		}
		content = content[i+len(opener):]
		j := strings.Index(content, closer)
		if j < 0 {
			j = len(content)
		}
		quoted = append(quoted, content[:j])
		content = content[min(j+len(closer), len(content)):]
	}
}
