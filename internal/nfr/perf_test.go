package nfr

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// Budgets are about 10-25x the time measured on a laptop: tight enough to
// catch an accidental O(n^2) (the invoice pipeline once took 29s here
// before indexing), loose enough to ignore machine noise. Run
// with -short to skip them.
func budget(t *testing.T, name string, limit time.Duration, f func()) {
	t.Helper()
	start := time.Now()
	f()
	took := time.Since(start)
	t.Logf("%s took %v (budget %v)", name, took, limit)
	if took > limit {
		t.Fatalf("%s took %v, over the %v budget", name, took, limit)
	}
}

func skipShort(t *testing.T) {
	if testing.Short() {
		t.Skip("performance budget; skipped with -short")
	}
}

// 2,000 parked invoices are matched and posted against 20,000 posted ones.
func TestPerfInvoicePipeline(t *testing.T) {
	skipShort(t)
	s := LargeStore(500, 20_000, 2_000)
	audit, _ := governance.NewAuditLog("")
	svc := agent.NewService(s, audit)
	svc.Today = func() string { return "2026-10-09" }
	var sum agent.Summary
	budget(t, "invoice pipeline (2k parked / 20k history)", 2*time.Second, func() {
		sum = agent.ProcessInvoices(context.Background(), svc, nil, io.Discard)
	})
	if sum.Processed != 2_000 || len(sum.Errors) > 0 {
		t.Fatalf("summary: %d processed, %v", sum.Processed, sum.Errors)
	}
}

// A payment proposal over 20,000 open items.
func TestPerfPaymentProposal(t *testing.T) {
	skipShort(t)
	s := LargeStore(1_000, 20_000, 0)
	var run *core.PaymentRun
	budget(t, "payment proposal (20k open items)", time.Second, func() {
		var err error
		if run, err = s.ProposePayments("1000", "2027-01-31", 5, "u"); err != nil {
			t.Fatal(err)
		}
	})
	if len(run.Payments) != 1_000 {
		t.Fatalf("want 1,000 vendors paid, got %d", len(run.Payments))
	}
	budget(t, "payment execution (1k vendors)", time.Second, func() {
		if _, err := s.ExecutePaymentRun(run.ID, "100000", "2027-01-31"); err != nil {
			t.Fatal(err)
		}
	})
}

// Migration of a full mock year, five times over (company codes 1000/2000).
func TestPerfMigration(t *testing.T) {
	skipShort(t)
	e := sap.GenerateMock(42, "2026")
	budget(t, "5 migrations with reconciliation", 2*time.Second, func() {
		for i := 0; i < 5; i++ {
			s := core.NewStore()
			res, err := migration.Run(context.Background(), testkit.MemSource{E: e}, sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: "2026"}, migration.Options{}, s)
			if err != nil || !res.Report.Passed {
				t.Fatal(err)
			}
		}
	})
}

// Analytics over 60,000 journal lines plus POs and invoices.
func TestPerfAnalyticsQuery(t *testing.T) {
	skipShort(t)
	s := LargeStore(1_000, 20_000, 0)
	m, _ := analytics.DefaultModel()
	var facts *analytics.Facts
	budget(t, "fact build (60k journal lines)", 2*time.Second, func() {
		var err error
		if facts, err = analytics.BuildFacts(s, m, time.Now()); err != nil {
			t.Fatal(err)
		}
	})
	budget(t, "10 semantic queries", 3*time.Second, func() {
		for i := 0; i < 10; i++ {
			if _, err := analytics.Execute(m, facts, analytics.Principal{ID: "u"}, analytics.Query{
				Measures: []string{"invoice_value", "purchase_order_value"}, By: []string{"fiscal_month", "vendor"},
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// Validating and diffing a configuration with 2,000 users.
func TestPerfConfiguration(t *testing.T) {
	skipShort(t)
	c := testkit.Config(t)
	for i := 0; i < 2_000; i++ {
		c.Users = append(c.Users, config.User{ID: "load" + string(rune('a'+i%26)) + itoa(i), Name: "Load", Roles: []string{"Z_BUYER"}})
	}
	other := testkit.Config(t)
	budget(t, "validate + diff (2k users)", 3*time.Second, func() {
		if config.HasErrors(config.Validate(c)) {
			t.Fatal("unexpected errors")
		}
		changes, err := config.Diff(other, c)
		if err != nil || len(changes) != 2_000 {
			t.Fatalf("diff: %v %d", err, len(changes))
		}
	})
}

func itoa(i int) string {
	b := []byte{}
	for i > 0 || len(b) == 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func BenchmarkThreeWayMatch(b *testing.B) {
	s := LargeStore(200, 5_000, 1)
	inv := s.Invoices["QINV0000000"]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.ThreeWayMatch(inv, core.DefaultTolerance)
	}
}

func BenchmarkSemanticQuery(b *testing.B) {
	s := LargeStore(200, 5_000, 0)
	m, _ := analytics.DefaultModel()
	facts, _ := analytics.BuildFacts(s, m, time.Now())
	q := analytics.Query{Measures: []string{"expenses"}, By: []string{"fiscal_month"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		analytics.Execute(m, facts, analytics.Principal{ID: "u"}, q)
	}
}
