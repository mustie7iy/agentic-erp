package i18n

import (
	"strings"
	"unicode/utf8"
)

// accents replaces each ASCII letter with an accented one of its shape,
// as Android's en-XA does, so a string left in English stands out.
var accents = map[rune]rune{
	'a': 'á', 'b': 'ƀ', 'c': 'ç', 'd': 'ð', 'e': 'é', 'f': 'ƒ', 'g': 'ĝ', 'h': 'ĥ', 'i': 'î', 'j': 'ĵ', 'k': 'ķ', 'l': 'ļ', 'm': 'ɱ',
	'n': 'ñ', 'o': 'ö', 'p': 'þ', 'q': 'ǫ', 'r': 'ŕ', 's': 'š', 't': 'ţ', 'u': 'û', 'v': 'ṽ', 'w': 'ŵ', 'x': 'ẋ', 'y': 'ý', 'z': 'ž',
	'A': 'Å', 'B': 'Ɓ', 'C': 'Ç', 'D': 'Ð', 'E': 'É', 'F': 'Ƒ', 'G': 'Ĝ', 'H': 'Ĥ', 'I': 'Î', 'J': 'Ĵ', 'K': 'Ķ', 'L': 'Ļ', 'M': 'Ṁ',
	'N': 'Ñ', 'O': 'Ö', 'P': 'Þ', 'Q': 'Ǫ', 'R': 'Ŕ', 'S': 'Š', 'T': 'Ţ', 'U': 'Û', 'V': 'Ṽ', 'W': 'Ŵ', 'X': 'Ẋ', 'Y': 'Ý', 'Z': 'Ž',
}

// Pseudo-locale markers: a long-text message is bracketed, a right-to-left
// one wrapped in a right-to-left override.
const (
	LongOpen  = "⟦"
	LongClose = "⟧"
	RTLOpen   = "\u200f\u202e" // right-to-left mark, right-to-left override
	RTLClose  = "\u202c\u200f" // pop directional formatting, right-to-left mark
)

// longText is the en-XA form of an English format string: accented,
// padded by about a third and bracketed, with its verbs (%s, %[1]d, %%)
// left as they are so it formats the same arguments.
func longText(s string) string {
	var b strings.Builder
	b.WriteString(LongOpen)
	verb := false
	for _, r := range s {
		switch {
		case verb:
			b.WriteRune(r)
			// A verb ends at its letter (or the second % of %%).
			verb = r != '%' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
		case r == '%':
			b.WriteRune(r)
			verb = true
		default:
			if a, ok := accents[r]; ok {
				r = a
			}
			b.WriteRune(r)
		}
	}
	if pad := (utf8.RuneCountInString(s) + 2) / 3; pad > 0 {
		b.WriteString(" " + strings.Repeat("·", pad))
	}
	b.WriteString(LongClose)
	return b.String()
}

// rightToLeft is the ar-XB form of an English format string: shown right
// to left, as Arabic text would sit in the mirrored layout.
func rightToLeft(s string) string { return RTLOpen + s + RTLClose }
