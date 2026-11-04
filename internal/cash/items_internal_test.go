package cash

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestOffsetting: tax lines follow their code's base lines in proportion,
// rounded once with the residue by largest remainder (ties to the earlier
// line), so the lines still add up to the document; self-assessed pairs
// are left out; a tax line with no base line to follow stays on its own
// account (ADR-0051 §4, T53e3).
func TestOffsetting(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []DocumentLine
		want  string
	}{
		{"in proportion", []DocumentLine{{"530000", 500, "V1", ""}, {"510000", 300, "V1", ""}, {"140000", 152, "V1", "input"}},
			"[{530000 595} {510000 357}]"},
		{"the residue to the larger remainder", []DocumentLine{{"A", 1, "V1", ""}, {"B", 2, "V1", ""}, {"T", 1, "V1", "input"}}, "[{A 1} {B 3}]"},
		{"a tie to the earlier line", []DocumentLine{{"A", 1, "V1", ""}, {"B", 1, "V1", ""}, {"T", 1, "V1", "input"}}, "[{A 2} {B 1}]"},
		{"two tax lines of a code, each by the base amounts", []DocumentLine{{"A", 1, "V1", ""}, {"B", 2, "V1", ""}, {"T", 2, "V1", "input"},
			{"U", 1, "V1", "input"}}, "[{A 2} {B 4}]"},
		{"per code", []DocumentLine{{"A", 100, "V1", ""}, {"B", 100, "V0", ""}, {"T", 13, "V1", "input"}}, "[{A 113} {B 100}]"},
		{"a self-assessed pair left out", []DocumentLine{{"A", 100, "RC", ""}, {"T", 5, "RC", "self_input"}, {"U", -5, "RC", "self_output"}}, "[{A 100}]"},
		{"tax without a base line", []DocumentLine{{"A", 100, "", ""}, {"T", 5, "IM", "input"}}, "[{A 100} {T 5}]"},
		{"base lines that cancel out", []DocumentLine{{"A", 100, "V1", ""}, {"B", -100, "V1", ""}, {"T", 5, "V1", "input"}}, "[{A 100} {B -100} {T 5}]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := offsetting(c.lines)
			if err != nil {
				t.Fatal(err)
			}
			if render(got) != c.want {
				t.Errorf("got %v, want %s", got, c.want)
			}
		})
	}
	t.Run("a share out of range", func(t *testing.T) {
		_, err := offsetting([]DocumentLine{{"A", math.MaxInt64, "V1", ""}, {"B", -math.MaxInt64 + 1, "V1", ""}, {"T", math.MaxInt64, "V1", "input"}})
		if !errors.Is(err, money.ErrOverflow) {
			t.Errorf("got %v, want money.ErrOverflow", err)
		}
	})
	t.Run("a line out of range", func(t *testing.T) {
		_, err := offsetting([]DocumentLine{{"A", math.MaxInt64, "V1", ""}, {"T", 1, "V1", "input"}})
		if !errors.Is(err, money.ErrOverflow) {
			t.Errorf("got %v, want money.ErrOverflow", err)
		}
	})
}

// TestClassify: a flow's parts are derived each on its own and their
// shares of one item summed, in the direction of the flow; a part that
// cannot be split fails it.
func TestClassify(t *testing.T) {
	d := config.Derivation{Accounts: []config.AccountDerivation{{From: "530000", To: "530000", Item: "RENT"}},
		Defaults: config.DerivationDefaults{Outflow: "SUPPLIERS", Inflow: "RECEIPTS"}}
	for _, c := range []struct {
		name   string
		amount money.Minor
		parts  []part
		want   string
	}{
		{"an outflow, a credit memo in it", -900, []part{{amount: -1000, lines: []DocumentLine{{Account: "530000", Amount: 1000}}},
			{amount: 100, lines: []DocumentLine{{Account: "530000", Amount: -100}}}, {amount: 0}}, "[{RENT -900} {SUPPLIERS 0}]"},
		{"an inflow", 50, []part{{amount: 50}}, "[{RECEIPTS 50}]"},
		// A line in transit of part of a payment: its items' parts are
		// shared out over the line's amount (T53e4).
		{"parts of a payment, a line of it", -10, []part{{amount: -80, lines: []DocumentLine{{Account: "530000", Amount: 80}}}, {amount: -20}},
			"[{RENT -8} {SUPPLIERS -2}]"},
		{"parts adding up to nothing", -10, []part{{amount: -5}, {amount: 5}}, "[{SUPPLIERS -10}]"},
		{"no parts", -10, nil, "[]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := classify(newRules(d), &sums{}, c.amount, c.parts)
			if err != nil {
				t.Fatal(err)
			}
			if render(got) != c.want {
				t.Errorf("got %s, want %s", render(got), c.want)
			}
		})
	}
	for _, c := range []struct {
		name   string
		amount money.Minor
		parts  []part
	}{
		{"lines out of range", -1, []part{{amount: -1, lines: []DocumentLine{{"A", math.MaxInt64, "V1", ""}, {"T", 1, "V1", "input"}}}}},
		{"an amount out of range", math.MinInt64, []part{{amount: math.MinInt64, lines: []DocumentLine{{Account: "530000", Amount: 1}, {Account: "1", Amount: 1}}}}},
		{"parts shared out of range", math.MaxInt64, []part{{amount: 1}, {amount: -2}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := classify(newRules(d), &sums{}, c.amount, c.parts); !errors.Is(err, money.ErrOverflow) {
				t.Errorf("got %v, want money.ErrOverflow", err)
			}
		})
	}
}

// render shows lines or shares as [{account-or-item amount} ...], amounts
// in minor units.
func render[T Line | Share](xs []T) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		switch v := any(x).(type) {
		case Line:
			out[i] = fmt.Sprintf("{%s %d}", v.Account, int64(v.Amount))
		case Share:
			out[i] = fmt.Sprintf("{%s %d}", v.Item, int64(v.Amount))
		}
	}
	return fmt.Sprint(out)
}

// TestSharesOutOfRange: shares whose classification's sums went out of the
// range of an amount are refused, never given saturated.
func TestSharesOutOfRange(t *testing.T) {
	ar := actualsRead{r: reader{doc: &config.Config{}, sums: &sums{overflow: true}}}
	if _, err := ar.shares(t.Context()); err == nil || !strings.Contains(err.Error(), "add up to more than an amount can hold") {
		t.Errorf("shares: got %v, want cash.amount_too_large", err)
	}
}
