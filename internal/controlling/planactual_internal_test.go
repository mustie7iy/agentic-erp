package controlling

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestVariancePercent: the variance in percent of the plan is exact,
// rounded to two decimals half away from zero, without a sign on zero,
// and absent on a zero plan.
func TestVariancePercent(t *testing.T) {
	q := paQuery{currency: "EUR", exp: 2}
	for name, c := range map[string]struct {
		plan, actual money.Minor
		want         string
	}{
		"over the plan":                   {10000, 12000, "20.00"},
		"under the plan":                  {10000, 5000, "-50.00"},
		"a half up":                       {80000, 80004, "0.01"},  // 0.005 %
		"a half down":                     {80000, 79996, "-0.01"}, // -0.005 %
		"just below a half down":          {100000, 99996, "0.00"}, // -0.004 %: no sign on zero
		"a third":                         {300, 301, "0.33"},
		"revenue above its negative plan": {-100000, -120000, "20.00"},
		"no plan":                         {0, 100, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := q.toAPI(planActual{plan: c.plan, actual: c.actual})
			if err != nil {
				t.Fatal(err)
			}
			if s := string(got.VariancePercent.Value); s != c.want {
				t.Errorf("percent: got %q, want %q", s, c.want)
			}
		})
	}
}

// TestMerge: plan and actual rows join by key, in key order, up to n.
func TestMerge(t *testing.T) {
	a, b := uuid.MustParse("00000000-0000-0000-0000-00000000000a"), uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	key := func(c, acct uuid.UUID) ledger.CenterAccount {
		return ledger.CenterAccount{CostCenterID: c, AccountID: acct}
	}
	plans := []planActual{{CenterAccount: key(a, a), plan: 1}, {CenterAccount: key(a, b), plan: 2}, {CenterAccount: key(b, b), plan: 3}}
	actuals := []ledger.AssignmentActual{{CenterAccount: key(a, b), Amount: 20}, {CenterAccount: key(b, a), Amount: 30}}
	for name, c := range map[string]struct {
		plans   []planActual
		actuals []ledger.AssignmentActual
		n       int
		want    []planActual
	}{
		"all": {plans, actuals, 9, []planActual{{CenterAccount: key(a, a), plan: 1}, {CenterAccount: key(a, b), plan: 2, actual: 20},
			{CenterAccount: key(b, a), actual: 30}, {CenterAccount: key(b, b), plan: 3}}},
		"the first two": {plans, actuals, 2, []planActual{{CenterAccount: key(a, a), plan: 1}, {CenterAccount: key(a, b), plan: 2, actual: 20}}},
		"an actual of an earlier center": {[]planActual{{CenterAccount: key(b, a), plan: 1}}, []ledger.AssignmentActual{{CenterAccount: key(a, b), Amount: 2}}, 9,
			[]planActual{{CenterAccount: key(a, b), actual: 2}, {CenterAccount: key(b, a), plan: 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			got := merge(c.plans, c.actuals, c.n)
			if len(got) != len(c.want) {
				t.Fatalf("rows: got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("row %d: got %v, want %v", i, got[i], c.want[i])
				}
			}
		})
	}
}
