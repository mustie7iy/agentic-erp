package cash_test

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestDerive: a flow's amount is split over the items of its offsetting
// lines' accounts in proportion, adding up to it; what no account rule
// covers takes the planning group's item, then the posting rule's, then
// the direction's default (ADR-0051 §4).
func TestDerive(t *testing.T) {
	d := config.Derivation{
		Accounts: []config.AccountDerivation{{From: "6100", To: "6100", Item: "RENT"}, {From: "1500", To: "1599", Item: "CAPEX"},
			{From: "1500", To: "1599", Item: "SHADOWED"}},
		PlanningGroups: []config.PlanningGroupDerivation{{Group: "TAX_AUTHORITY", Item: "TAX"}},
		PostingRules:   []config.PostingRuleDerivation{{Rule: "NCHG", Item: "BANK_CHARGES"}},
		Defaults:       config.DerivationDefaults{Outflow: "SUPPLIERS", Inflow: "RECEIPTS"},
	}
	out := config.DirectionOutflow
	for _, c := range []struct {
		name   string
		amount money.Minor
		basis  cash.Basis
		want   string
	}{
		{"one account", -1000, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 900}}}, "[RENT:-1000]"},
		{"the first covering rule", -1000, cash.Basis{Direction: out, Lines: []cash.Line{{"1510", 900}}}, "[CAPEX:-1000]"},
		{"split in proportion", -1000, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 300}, {"1510", 600}}},
			"[RENT:-333 CAPEX:-667]"},
		{"lines of one item together", -100, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 1}, {"1510", 1}, {"6100", 1}}},
			"[RENT:-67 CAPEX:-33]"},
		{"lines of a credit memo", 500, cash.Basis{Direction: config.DirectionInflow, Lines: []cash.Line{{"6100", -100}, {"1510", -400}}},
			"[RENT:100 CAPEX:400]"},
		{"a line no rule covers takes the fallback", -1000, cash.Basis{Direction: out, PlanningGroup: "TAX_AUTHORITY",
			Lines: []cash.Line{{"6100", 500}, {"5100", 500}}}, "[RENT:-500 TAX:-500]"},
		{"the planning group", -1000, cash.Basis{Direction: out, PlanningGroup: "TAX_AUTHORITY", PostingRule: "NCHG"}, "[TAX:-1000]"},
		{"a planning group without a rule", -1000, cash.Basis{Direction: out, PlanningGroup: "DOMESTIC"}, "[SUPPLIERS:-1000]"},
		{"the posting rule", -25, cash.Basis{Direction: out, PostingRule: "NCHG"}, "[BANK_CHARGES:-25]"},
		{"a posting rule without a rule", -25, cash.Basis{Direction: out, PostingRule: "NINT"}, "[SUPPLIERS:-25]"},
		{"the outflow default", -1000, cash.Basis{Direction: out, Lines: []cash.Line{{"5100", 900}}}, "[SUPPLIERS:-1000]"},
		{"the inflow default", 1000, cash.Basis{Direction: config.DirectionInflow}, "[RECEIPTS:1000]"},
		{"a credit line takes its item's share down", -800, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 1000}, {"1510", -200}}},
			"[RENT:-1000 CAPEX:200]"},
		{"lines that cancel out", -10, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 5}, {"1510", -5}}}, "[SUPPLIERS:-10]"},
		{"lines of nothing", -1000, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 0}}}, "[SUPPLIERS:-1000]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := cash.Derive(d, c.amount, c.basis)
			if err != nil {
				t.Fatal(err)
			}
			if s := shares(got); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
		})
	}
	t.Run("no default configured", func(t *testing.T) {
		got, err := cash.Derive(config.Derivation{}, -10, cash.Basis{Direction: out})
		if err != nil || shares(got) != "[:-10]" {
			t.Errorf("got %v, %v, want one share without an item", got, err)
		}
	})
	t.Run("an amount out of range", func(t *testing.T) {
		_, err := cash.Derive(d, math.MinInt64, cash.Basis{Direction: out, Lines: []cash.Line{{"6100", 1}, {"1510", 1}}})
		if !errors.Is(err, money.ErrOverflow) {
			t.Errorf("got %v, want money.ErrOverflow", err)
		}
	})
}

// shares formats shares as [ITEM:amount ...].
func shares(ss []cash.Share) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%s:%d", s.Item, int64(s.Amount))
	}
	return fmt.Sprint(out)
}
