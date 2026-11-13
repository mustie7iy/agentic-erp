package recommend

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// Figures are what the run's reads returned that a recommendation may
// state (ADR-0072 §4). The check does not try to tell which numbers of a
// text are amounts: every number a content writes must be one the run
// read. A number with a decimal fraction must be an amount: a Money's
// amount (the text under the key amount) or a JSON number with a
// fraction under any key of an answer (a rate, a percentage). A whole number
// must be written somewhere in an answer or in the run's prompt (a count,
// a day of a date, a document number). No number the read's own input
// wrote is read from that read's answer, though the answer gives it
// back: the model could otherwise have any number it made up returned to
// it. Numbers are kept as written, without their sign.
type Figures struct {
	money   map[string]bool // Money amounts
	amounts map[string]bool // Money amounts and the answers' numbers with a fraction
	numbers map[string]bool // every number an answer or the prompt writes
}

func newFigures(prompt string) *Figures {
	f := &Figures{money: map[string]bool{}, amounts: map[string]bool{}, numbers: map[string]bool{}}
	for _, n := range Numbers(prompt) {
		f.numbers[n] = true
	}
	return f
}

// Returned reports whether a read of the run returned the number as a
// Money's amount: what an agent holds an amount it recommends to, since
// a whole number passes Unread by standing anywhere in what the run read.
func (f *Figures) Returned(n string) bool { return f.money[n] }

var (
	// decimal is a plain decimal as the API writes amounts.
	decimal = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	// fraction is a number written with a decimal fraction.
	fraction = regexp.MustCompile(`^\d+\.\d+$`)
	// identifier is a UUID: its digits are no numbers.
	identifier = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// written is a number as a text writes it: digits, with separators
	// and a fraction between them, wherever they stand.
	written = regexp.MustCompile(`\d(?:[\d.,]*\d)?`)
	// digits is a run of digits alone.
	digits = regexp.MustCompile(`\d+`)
	// grouped is a number written with thousands separators.
	grouped = regexp.MustCompile(`^\d{1,3}(?:,\d{3})+(?:\.\d+)?$`)
)

// Numbers are the numbers a text writes, in its order: every run of
// digits, with the separators and the fraction between them, whatever
// stands beside it (a currency, a letter, a sign), thousands separators
// taken out. The digits of a UUID are no numbers. A number written in
// words is not seen.
func Numbers(text string) []string {
	var out []string
	for _, n := range written.FindAllString(identifier.ReplaceAllString(text, " "), -1) {
		if grouped.MatchString(n) {
			n = strings.ReplaceAll(n, ",", "")
		}
		out = append(out, n)
	}
	return out
}

// input is the numbers a read's input wrote, wherever they stand in it
// (an amount to check, the days of a date, a list of days in one text):
// as written, and those with a fraction also without the zeros that end
// it, so that an amount asked about as 1617.1 is known again when the
// answer writes 1617.10.
type input struct {
	exact, plain map[string]bool
}

// plain is a number without the zeros that end its fraction.
func plain(n string) string {
	if strings.Contains(n, ".") {
		n = strings.TrimSuffix(strings.TrimRight(n, "0"), ".")
	}
	return n
}

// supplied are the numbers a call's input wrote. The input is the
// model's JSON, decoded by the tool with numbers as floats, so it is read
// again here with its numbers as written.
func supplied(in map[string]any) input {
	out := input{exact: map[string]bool{}, plain: map[string]bool{}}
	raw, _ := json.Marshal(in) // decoded from JSON, so it encodes again
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v) // its own encoding: it decodes
	out.leaves(v)
	return out
}

func (in input) leaves(v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			in.leaves(child)
		}
	case []any:
		for _, child := range x {
			in.leaves(child)
		}
	case json.Number:
		in.keep(x.String())
	case string:
		in.keep(x)
	}
}

func (in input) keep(value string) {
	for _, n := range Numbers(value) {
		in.exact[n] = true
		if fraction.MatchString(n) {
			in.plain[plain(n)] = true
		}
	}
	// A list written in one text ("30,1600") comes back as its numbers:
	// every run of digits alone is the input's too.
	for _, n := range digits.FindAllString(identifier.ReplaceAllString(value, " "), -1) {
		in.exact[n] = true
	}
}

// has reports whether the input wrote the number.
func (in input) has(n string) bool { return in.exact[n] || in.plain[plain(n)] }

// add keeps what an answer's body returned but what the call's input
// supplied; a body that is not JSON returned nothing.
func (f *Figures) add(body []byte, in input) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		f.walk("", v, in)
	}
}

func (f *Figures) walk(key string, v any, in input) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			f.walk(k, child, in)
		}
	case []any:
		for _, child := range x {
			f.walk(key, child, in)
		}
	case json.Number:
		f.keep(x.String(), asLiteral, in)
	case string:
		kind := asText
		if key == "amount" && decimal.MatchString(x) {
			kind = asMoney
		}
		f.keep(x, kind, in)
	}
}

// What a value of an answer is: a text, a number literal, or a Money's
// amount.
const (
	asText = iota
	asLiteral
	asMoney
)

// keep keeps the numbers a value writes but those the input wrote: each
// as a number; a Money's amount as money and as an amount; a number
// literal with a fraction as an amount. A text's numbers are numbers
// only, whatever they look like.
func (f *Figures) keep(value string, kind int, in input) {
	for _, n := range Numbers(value) {
		if in.has(n) {
			continue
		}
		f.numbers[n] = true
		if kind == asMoney {
			f.money[n] = true
		}
		if kind == asMoney || kind == asLiteral && fraction.MatchString(n) {
			f.amounts[n] = true
		}
	}
}

// Unread are the numbers the text writes that the run did not read, each
// once, in the text's order: a figure the agent computed, rounded or
// made up, and a count it made itself, which it writes in words.
func (f *Figures) Unread(text string) []string {
	var out []string
	for _, n := range Numbers(text) {
		read := f.amounts[n] || !fraction.MatchString(n) && f.numbers[n]
		if !read && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
