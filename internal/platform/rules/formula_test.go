package rules

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestFormula: an amount from other amounts (ADR-0049 §11), exact, over
// the inputs it names only; anything else is refused when compiled, and
// an overflow, a division by zero or the cost limit when evaluated.
func TestFormula(t *testing.T) {
	inputs := []string{"box_105", "box_108"}
	f, err := CompileFormula("box_105 - box_108", inputs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Eval(context.Background(), map[string]money.Minor{"box_105": 12_345, "box_108": 20_000})
	if err != nil || got != -7_655 {
		t.Errorf("105 - 108: got %v, %v, want -7655 minor", got, err)
	}

	for name, c := range map[string]struct{ expr, want string }{
		"a later box":     {"box_105 - box_109", "undeclared reference"},
		"a float":         {"double(box_105) * 0.5", "floating-point"},
		"not an amount":   {"box_105 > 0", "must be an amount"},
		"a syntax error":  {"box_105 -", "Syntax error"},
		"the clock":       {"timestamp('2026-01-01T00:00:00Z').getFullYear()", "undeclared reference"},
		"too long a text": {strings.Repeat("box_105 + ", 200) + "0", "size"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileFormula(c.expr, inputs)
			if !errors.Is(err, ErrFormula) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want ErrFormula naming %q", err, c.want)
			}
		})
	}

	for name, c := range map[string]struct {
		expr string
		in   map[string]money.Minor
	}{
		"an overflow":        {"box_105 + box_108", map[string]money.Minor{"box_105": math.MaxInt64, "box_108": 1}},
		"the smallest int":   {"box_105 - box_108", map[string]money.Minor{"box_105": -math.MaxInt64, "box_108": 1}},
		"a division by zero": {"box_105 / box_108", map[string]money.Minor{"box_105": 1, "box_108": 0}},
		"the cost limit":     {"[" + strings.Repeat("box_105,", 30) + "box_108].map(x, [1,2,3,4,5,6,7,8,9,10].map(y, [1,2,3,4,5,6,7,8,9,10].map(z, x * y * z).size())).size()", map[string]money.Minor{"box_105": 1, "box_108": 1}},
		"an input not given": {"box_105 + box_108", map[string]money.Minor{"box_105": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			f, err := CompileFormula(c.expr, inputs)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := f.Eval(context.Background(), c.in); err == nil {
				t.Errorf("got %v, want an error", got)
			}
		})
	}
}
