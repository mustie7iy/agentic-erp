package recommend

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// Figures are the amounts the run's reads returned (ADR-0072 §4): what a
// recommendation may state. An amount is kept as the API wrote it,
// without its sign: a Money's amount (the text under the key amount), and
// a number with a decimal fraction wherever it stands in an answer (a
// rate, a percentage). A text a person typed is no figure, whatever it
// looks like, and neither is a value the request itself supplied, which
// an answer may echo: the model could otherwise have any amount it made
// up returned to it.
type Figures struct {
	amounts map[string]bool
}

// decimal is a plain decimal as the API writes amounts.
var decimal = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// unsigned is a decimal without its sign, as figures are kept and
// compared.
func unsigned(s string) string { return strings.TrimPrefix(s, "-") }

// plain is a decimal as it is compared with what an input supplied:
// unsigned and without the zeros that end its fraction, so that an amount
// asked about as 1617.1 is known again when the answer writes 1617.10.
func plain(s string) string {
	s = unsigned(s)
	if strings.Contains(s, ".") {
		s = strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// leaves are the texts and numbers of a decoded JSON value, plain: what
// a call's input supplied.
func leaves(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			leaves(child, into)
		}
	case []any:
		for _, child := range x {
			leaves(child, into)
		}
	case json.Number:
		into[plain(x.String())] = true
	case string:
		into[plain(x)] = true
	}
}

// supplied are the values a call's input supplied. The input is the
// model's JSON, decoded by the tool with numbers as floats, so it is read
// again here with its numbers as written.
func supplied(input map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, _ := json.Marshal(input) // decoded from JSON, so it encodes again
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v) // its own encoding: it decodes
	leaves(v, out)
	return out
}

// add keeps the amounts of an answer's body but those the call's input
// supplied; a body that is not JSON holds none.
func (f *Figures) add(body []byte, supplied map[string]bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		f.walk("", v, supplied)
	}
}

func (f *Figures) walk(key string, v any, supplied map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			f.walk(k, child, supplied)
		}
	case []any:
		for _, child := range x {
			f.walk(key, child, supplied)
		}
	case json.Number:
		if s := unsigned(x.String()); strings.Contains(s, ".") && decimal.MatchString(s) && !supplied[plain(s)] {
			f.amounts[s] = true
		}
	case string:
		if s := unsigned(x); key == "amount" && decimal.MatchString(s) && !supplied[plain(s)] {
			f.amounts[s] = true
		}
	}
}

// stated finds the numbers of a text with what marks them as money: a
// currency code or one of the signs $ € £ ¥ ₹ before or after, with a
// space or without, or a currency's name after. A number is written with
// thousands separators or without.
var stated = regexp.MustCompile(`(?:\b([A-Z]{3}) ?|([$€£¥₹]) ?|\b)(\d{1,3}(?:,\d{3})+|\d+)(\.\d+)?(?: ?([A-Z]{3})\b| ?([$€£¥₹])| (dollars?|euros?|pounds?|dirhams?|yen|rupees?)\b)?`)

// Amounts are the amounts a text states, without thousands separators, in
// the order it states them: a number with a decimal fraction or with
// thousands separators, and a whole number beside a currency code ("CAD
// 500", "500JPY") or a currency sign ("$500", "500 €"), or before a
// currency's name ("500 dollars"). Other whole numbers (counts, days,
// years, document numbers) are not amounts.
func Amounts(text string) []string {
	var out []string
	for _, m := range stated.FindAllStringSubmatch(text, -1) {
		if m[4] != "" || strings.Contains(m[3], ",") || m[2] != "" || m[6] != "" || m[7] != "" || isCurrency(m[1]) || isCurrency(m[5]) {
			out = append(out, strings.ReplaceAll(m[3], ",", "")+m[4])
		}
	}
	return out
}

func isCurrency(code string) bool {
	_, ok := currency.SeededExponent(code)
	return ok
}

// Unread are the amounts the text states that no read of the run
// returned, each once, in the text's order: a figure the agent computed,
// rounded or made up.
func (f *Figures) Unread(text string) []string {
	var out []string
	for _, a := range Amounts(text) {
		if !f.amounts[a] && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}
