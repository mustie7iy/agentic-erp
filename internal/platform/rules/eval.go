package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// Violation is a rule that refused a row.
type Violation struct {
	Rule    string
	Message string
	failure string // a held write's rule that could not be evaluated: the kind of failure
}

// Check evaluates every rule of the set against a row and its extensions
// and returns the rules that refused it, in name order. An input of the
// wrong type, or a rule that fails, exceeds its cost or its time, is an
// error: the write cannot be shown to pass.
func (s *Set) Check(ctx context.Context, row, ext map[string]any) ([]Violation, error) {
	act, err := s.bind(row, ext)
	if err != nil {
		return nil, fmt.Errorf("rules: %s: %w", s.Entity, err)
	}
	var out []Violation
	for _, r := range s.rules {
		ok, err := r.eval(ctx, act)
		if err != nil {
			return nil, &UnevaluableError{Entity: s.Entity, Rule: r.name, Err: err}
		}
		if !ok {
			out = append(out, Violation{Rule: r.name, Message: r.message})
		}
	}
	return out, nil
}

// UnevaluableError is a rule that failed on an input, or exceeded its cost
// or its time: the input can be neither shown to pass nor refused by the
// rule, so the write fails closed (ADR-0046).
type UnevaluableError struct {
	Entity, Rule string
	Err          error
}

func (e *UnevaluableError) Error() string {
	return fmt.Sprintf("rules: %s.%s: %v", e.Entity, e.Rule, e.Err)
}

func (e *UnevaluableError) Unwrap() error { return e.Err }

// failure is what a report or a log line says of a rule's failure: its
// kind only, since the evaluator's text can repeat an input value (a key
// looked up, a pattern) that may be personal data.
func failure(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "cost limit exceeded"):
		return "exceeds its cost limit"
	case strings.Contains(msg, "operation interrupted"):
		return "exceeds its time limit"
	default:
		return "fails to evaluate on this input"
	}
}

// eval runs one rule within the timeout of ADR-0046. The checker proved
// the result boolean.
func (r compiled) eval(ctx context.Context, act interpreter.Activation) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, _, err := r.prg.ContextEval(ctx, act)
	if err != nil {
		return false, fmt.Errorf("evaluation: %w", err)
	}
	return out == types.True, nil
}

// bind turns a row and its extensions into the rule inputs: every
// declared input once, an absent optional one as optional.none(). A key
// that is not declared is refused, so a case cannot test a field that
// does not exist.
func (s *Set) bind(row, ext map[string]any) (interpreter.Activation, error) {
	given := make(map[string]any, len(row)+len(ext))
	for k, v := range row {
		given["row."+k] = v
	}
	for k, v := range ext {
		given["ext."+k] = v
	}
	vars := make(inputs, len(s.inputs))
	for _, in := range s.inputs {
		v, ok := given[in.name]
		delete(given, in.name)
		val, err := value(s.adapter, in, v, ok && v != nil)
		if err != nil {
			return nil, err
		}
		vars[in.name] = val
	}
	if len(given) > 0 {
		return nil, fmt.Errorf("%s is not an input", strings.Join(slices.Sorted(maps.Keys(given)), ", "))
	}
	return vars, nil
}

// inputs are a rule's bound inputs by qualified name (row.x, ext.y).
type inputs map[string]any

func (in inputs) ResolveName(name string) (any, bool) {
	v, ok := in[name]
	return v, ok
}

func (inputs) Parent() interpreter.Activation { return nil }

func value(a types.Adapter, in input, v any, present bool) (ref.Val, error) {
	if !present {
		if in.optional {
			return types.OptionalNone, nil
		}
		return nil, fmt.Errorf("%s is required", in.name)
	}
	val, ok := convert(a, in.t, v)
	if !ok {
		return nil, fmt.Errorf("%s: want %s", in.name, typeNames[in.t])
	}
	if in.optional {
		return types.OptionalOf(val), nil
	}
	return val, nil
}

var typeNames = map[typ]string{
	tString: "a string", tInt: "a whole number", tBool: "a boolean",
	tStrings: "a list of strings", tMoney: `{"amount": "<decimal>", "currency": "<code>"} with the currency's minor-unit digits`,
}

// convert reads a decoded JSON or YAML value as the input's type; a
// adapts a Money to its CEL object.
func convert(a types.Adapter, t typ, v any) (ref.Val, bool) {
	switch t {
	case tInt:
		n, ok := whole(v)
		return types.Int(n), ok
	case tBool:
		b, ok := v.(bool)
		return types.Bool(b), ok
	case tStrings:
		l, ok := strings_(v)
		return types.NewStringList(types.DefaultTypeAdapter, l), ok
	case tMoney:
		m, ok := moneyOf(v)
		return a.NativeToValue(m), ok
	default:
		if d, ok := v.(time.Time); ok && d.Equal(d.Truncate(24*time.Hour)) && d.Location() == time.UTC {
			return types.String(d.Format(time.DateOnly)), true // yaml reads an unquoted date in a case so
		}
		s, ok := v.(string)
		return types.String(s), ok
	}
}

// maxExact is the largest whole number a JSON number holds exactly.
const maxExact = 1<<53 - 1

// whole reads a whole number as the custom field write path does: from
// its JSON spelling, whatever Go type decoded it.
func whole(v any) (int64, bool) {
	b, _ := json.Marshal(v) // what does not encode (NaN, a map keyed by any) leaves no digits and is refused below
	n, err := strconv.ParseInt(string(b), 10, 64)
	return n, err == nil && n >= -maxExact && n <= maxExact
}

func strings_(v any) ([]string, bool) {
	switch l := v.(type) {
	case []string:
		return l, true
	case []any:
		out := make([]string, len(l))
		for i, e := range l {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	default:
		return nil, false
	}
}

// moneyOf reads a money input: a Money a write path built from the amount
// it parsed, or the API's {"amount", "currency"} of a case or a custom
// field, whose minor units come from the currency's exponent. An amount
// without exactly the currency's minor-unit digits, or a currency the
// reference data does not hold, is refused: the minor units would be
// wrong.
func moneyOf(v any) (Money, bool) {
	if m, ok := v.(Money); ok {
		return m, true
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 {
		return Money{}, false
	}
	amount, okA := m["amount"].(string)
	code, okC := m["currency"].(string)
	// The seeded exponents, not ref.currency: a case has no database. They
	// are the same while TestSeededMatchesTheDatabase holds, so a custom
	// field the write path checked against ref.currency reads alike here.
	exp, okE := currency.SeededExponent(code)
	if !okA || !okC || !okE {
		return Money{}, false
	}
	minor, err := money.ParseAmount(amount, exp)
	return Money{Amount: amount, Currency: code, Minor: minor}, err == nil
}
