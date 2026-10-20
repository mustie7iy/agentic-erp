package rules

import (
	"context"
	"fmt"
	"testing"
)

// FuzzCompile feeds the compiler arbitrary expressions: it must never
// panic, and a rule that compiles must give one result for one input.
func FuzzCompile(f *testing.F) {
	for _, e := range []string{vatRule, `row.roles.exists(r, r == "supplier")`, `ext.credit_days.orValue(0) < 90`,
		`[1,2,3].map(x, x * 2).size() == 3`, `now()`, `row.name`, `"a" + `, `ext.limit.amount`} {
		f.Add(e)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		r := vat()
		r.Expression = expr
		s, _ := Compile("business_partner", partnerSchema(), map[string]Rule{"r": r})
		if len(s.rules) == 0 {
			return
		}
		row, ext := partner("DE"), map[string]any{"vip": true, "credit_days": 30}
		first, err1 := s.Check(context.Background(), row, ext)
		again, err2 := s.Check(context.Background(), row, ext)
		if fmt.Sprint(first, err1) != fmt.Sprint(again, err2) {
			t.Errorf("%q: got %v %v then %v %v, want one result", expr, first, err1, again, err2)
		}
	})
}
