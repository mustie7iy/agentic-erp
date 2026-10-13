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
	if got := BudgetScale(); got != 1 {
		t.Fatalf("default scale: got %v, want 1", got)
	}
	if got := Budget(50 * time.Millisecond); got != 50*time.Millisecond {
		t.Fatalf("default budget: got %v, want %v", got, 50*time.Millisecond)
	}
	t.Setenv("ERP_BUDGET_SCALE", "4")
	if got := Budget(50 * time.Millisecond); got != 200*time.Millisecond {
		t.Fatalf("scaled: got %v, want %v", got, 200*time.Millisecond)
	}
	if got := Budget(time.Second); got != 4*time.Second {
		t.Fatalf("scaled: got %v, want %v", got, 4*time.Second)
	}
	t.Setenv("ERP_BUDGET_SCALE", "0.5")
	if got := Budget(200 * time.Millisecond); got != 100*time.Millisecond {
		t.Fatalf("tightened: got %v, want %v", got, 100*time.Millisecond)
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
	if len(strict.errors) != 1 {
		t.Fatalf("strict: got %v, want one error", strict.errors)
	}
	if want := "read: p95 120ms (budget 50ms)"; !strings.Contains(strict.errors[0], want) {
		t.Errorf("strict: got %q, want it to contain %q", strict.errors[0], want)
	}
	if len(lenient.errors) != 0 {
		t.Errorf("lenient: got %v, want no errors", lenient.errors)
	}
	for _, bad := range []string{"fast", "0", "-2"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("ERP_BUDGET_SCALE", bad)
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "ERP_BUDGET_SCALE") {
					t.Errorf("got %v, want a panic naming ERP_BUDGET_SCALE", r)
				}
			}()
			BudgetScale()
		})
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
