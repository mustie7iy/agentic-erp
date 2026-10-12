package nfr

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestBudgetScale: budgets are a cell's by default; ERP_BUDGET_SCALE
// multiplies them for smaller machines, and a bad value is refused loudly.
func TestBudgetScale(t *testing.T) {
	t.Setenv("ERP_BUDGET_SCALE", "")
	if BudgetScale() != 1 || Budget(50*time.Millisecond) != 50*time.Millisecond {
		t.Fatal("default scale is 1")
	}
	t.Setenv("ERP_BUDGET_SCALE", "4")
	if Budget(50*time.Millisecond) != 200*time.Millisecond || Budget(time.Second) != 4*time.Second {
		t.Fatalf("scaled: %v", Budget(50*time.Millisecond))
	}
	t.Setenv("ERP_BUDGET_SCALE", "0.5")
	if Budget(200*time.Millisecond) != 100*time.Millisecond {
		t.Fatalf("tightened: %v", Budget(200*time.Millisecond))
	}
	st := NewStats()
	for i := 0; i < 20; i++ {
		st.Add("read", Result{Status: 200, Took: 120 * time.Millisecond})
	}
	for _, op := range []string{"list", "post", "approve", "balance"} {
		st.Add(op, Result{Status: 200, Took: time.Millisecond})
	}
	t.Setenv("ERP_BUDGET_SCALE", "1")
	strict := &recorder{}
	st.CheckBudgets(strict)
	t.Setenv("ERP_BUDGET_SCALE", "3")
	lenient := &recorder{}
	st.CheckBudgets(lenient)
	if len(strict.errors) != 1 || !strings.Contains(strict.errors[0], "read: p95 120ms (budget 50ms)") || len(lenient.errors) != 0 {
		t.Fatalf("strict %v, lenient %v", strict.errors, lenient.errors)
	}
	for _, bad := range []string{"fast", "0", "-2"} {
		t.Setenv("ERP_BUDGET_SCALE", bad)
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "ERP_BUDGET_SCALE") {
					t.Errorf("%q: %v", bad, r)
				}
			}()
			BudgetScale()
		}()
	}
}

// recorder collects what CheckBudgets would report.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
