package cash

import (
	"errors"
	"fmt"
	"math"
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
	s := &sums{}
	got, err := classify(d, s, -900, []part{{amount: -1000, lines: []DocumentLine{{Account: "530000", Amount: 1000}}},
		{amount: 100, lines: []DocumentLine{{Account: "530000", Amount: -100}}}, {amount: 0}})
	if err != nil || render(got) != "[{RENT -900} {SUPPLIERS 0}]" {
		t.Errorf("an outflow: got %v, %v", got, err)
	}
	if got, err := classify(d, s, 50, []part{{amount: 50}}); err != nil || render(got) != "[{RECEIPTS 50}]" {
		t.Errorf("an inflow: got %v, %v", got, err)
	}
	if _, err := classify(d, s, -1, []part{{amount: -1, lines: []DocumentLine{{"A", math.MaxInt64, "V1", ""}, {"T", 1, "V1", "input"}}}}); err == nil {
		t.Error("lines out of range: got no error")
	}
	if _, err := classify(d, s, math.MinInt64, []part{{amount: math.MinInt64, lines: []DocumentLine{{Account: "530000", Amount: 1}, {Account: "1", Amount: 1}}}}); err == nil {
		t.Error("an amount out of range: got no error")
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
