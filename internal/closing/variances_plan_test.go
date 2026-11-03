package closing_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// planPermissions are Carla's with the plan's, which she enters the
// budget with in these tests.
var planPermissions = append(append([]string{}, closePermissions...), "controlling.plan.read", "controlling.plan.write")

// budgeted is a tenant whose company 2000 keeps cost centers
// (fixture.CostCenters) and has a 2026 budget, BUDGET: for March, office
// supplies (520000) 1,000.00 on Admin and 200.00 on Sales, salaries
// (610000) 500.00 on Admin, and 5,000.00 of office supplies on profit
// center PC1; 999.00 of office supplies on Admin in April. March's
// actuals are office supplies 1,500.00 on Admin and 300.00 of revenue on
// Sales, projected, and 100.00 more of office supplies, still queued.
func budgeted(t *testing.T) (fixture.Env, string) {
	t.Helper()
	e := fixture.NewWith(t, nil, withTemplate, planPermissions...)
	c := e.CostCenters(t, nil)
	cat, _ := e.Must(201, "POST", "/v1/plan-categories", `{"code": "BUDGET", "name": "Budget 2026"}`, "")
	line := func(kind string, center uuid.UUID, account string, period int, amount string) string {
		return fmt.Sprintf(`{%q: %q, "account_id": %q, "fiscal_period": %d, "amount": {"amount": %q, "currency": "EUR"}}`, kind+"_center_id", center,
			e.Account(t, "2000", account), period, amount)
	}
	batch := func(key string, lines ...string) {
		e.Must(200, "POST", "/v1/plan-batches", fmt.Sprintf(`{"plan_category_id": %q, "legal_entity_id": %q, "fiscal_year": 2026, "batch_key": %q, "lines": [%s]}`,
			cat["id"], e.DE, key, strings.Join(lines, ", ")), "")
	}
	batch("cost", line("cost", c.Admin, "520000", 3, "1000.00"), line("cost", c.Sales, "520000", 3, "200.00"), line("cost", c.Admin, "610000", 3, "500.00"),
		line("cost", c.Admin, "520000", 4, "999.00"))
	batch("profit", line("profit", c.Profit, "520000", 3, "5000.00"))
	postOn(t, e, "2026-03-10", ledger.PostingLine{AccountID: e.Account(t, "2000", "520000"), Amount: 150000, CostCenterID: &c.Admin},
		ledger.PostingLine{AccountID: e.Account(t, "2000", "400000"), Amount: -30000, CostCenterID: &c.Sales},
		ledger.PostingLine{AccountID: e.Account(t, "2000", "100000"), Amount: -120000})
	fixture.Project(t)
	postOn(t, e, "2026-03-20", ledger.PostingLine{AccountID: e.Account(t, "2000", "520000"), Amount: 10000, CostCenterID: &c.Admin},
		ledger.PostingLine{AccountID: e.Account(t, "2000", "100000"), Amount: -10000})
	got, _ := open(t, e.Client, e.DE, 3, "")
	return e, apitest.String(t, got, "id")
}

// postOn posts the lines in company 2000 in EUR on the day, as a context
// does.
func postOn(t *testing.T, e fixture.Env, day string, lines ...ledger.PostingLine) {
	t.Helper()
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}.Post(ctx,
			ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: d, PostingDate: d, Currency: "EUR", SourceContext: "payables",
				SourceDocumentID: uuid.New(), Lines: lines})
		return err
	}); err != nil {
		t.Fatalf("post on %s: %v", day, err)
	}
}

// planBasis is company 2000's variance setting against BUDGET over the
// centers given, flagging 400.00 and 10 %.
func planBasis(centers string) func(*config.Config) {
	return func(doc *config.Config) {
		doc.Company("2000").Close.Variance = &config.CloseVariance{Bases: []string{config.BasisPlan}, ThresholdAmount: "400.00", ThresholdPercent: "10",
			PlanCategory: "BUDGET", PlanCenters: centers}
	}
}

// TestVariancesAgainstThePlan: March against BUDGET's March, summed over
// its cost center lines (Admin's and Sales' office supplies, Admin's
// salaries; not PC1's, not April's), then over its profit center lines
// alone, the actual read from projected and queued entries alike.
func TestVariancesAgainstThePlan(t *testing.T) {
	e, id := budgeted(t)
	cases := []struct {
		centers, head string
		rows          []string
	}{
		{config.PlanCentersCost,
			"2000 Acme GmbH 2026/3 0L EUR plan against 2026/3 threshold 400.00 10 BUDGET: actual 1300.00 comparison 1700.00 variance -400.00, 3 rows, 2 flagged",
			[]string{"400000 -300.00 0.00 -300.00 - false", "520000 1600.00 1200.00 400.00 33.33 true", "610000 0.00 500.00 -500.00 -100.00 true"}},
		{config.PlanCentersProfit,
			"2000 Acme GmbH 2026/3 0L EUR plan against 2026/3 threshold 400.00 10 BUDGET: actual 1300.00 comparison 5000.00 variance -3700.00, 2 rows, 1 flagged",
			[]string{"400000 -300.00 0.00 -300.00 - false", "520000 1600.00 5000.00 -3400.00 -68.00 true"}},
	}
	for _, tc := range cases {
		t.Run(tc.centers, func(t *testing.T) {
			activate(t, e.Tenant.ID, planBasis(tc.centers))
			head, rows := variances(t, e.In(t), id, "basis=plan")
			if head != tc.head {
				t.Errorf("report:\ngot  %q\nwant %q", head, tc.head)
			}
			wantLines(t, "rows", rows, tc.rows)
			page, _ := e.In(t).Must(200, "GET", "/v1/period-closes/"+id+"/variances?basis=plan", "", "")
			wantFields(t, "the plan", page, map[string]any{"plan_category_name": "Budget 2026", "plan_centers": tc.centers})
		})
	}
}

// TestVarianceSettingsCheck: the configuration's closing section accepts
// a plan basis naming a category the tenant has, and refuses one it does
// not have on the field; prior bases name none.
func TestVarianceSettingsCheck(t *testing.T) {
	e, _ := budgeted(t)
	var setup configuration.Section
	for _, s := range app.Configuration(app.Config{}).Sections {
		if _, ok := s.(closing.Setup); ok {
			setup = s
		}
	}
	if setup == nil {
		t.Fatal("the configuration's sections: got no closing.Setup")
	}
	doc := fixture.Document(t)
	check := func(ctx context.Context) error {
		_, err := setup.Check(ctx, nil, doc)
		return err
	}
	cases := []struct {
		name string
		set  func(*config.Config)
		want string
	}{
		{"a category the tenant has", planBasis(config.PlanCentersCost), ""},
		{"prior bases alone", func(d *config.Config) {
			d.Company("2000").Close.Variance = &config.CloseVariance{Bases: []string{config.BasisPriorYear}, ThresholdAmount: "1.00"}
		}, ""},
		{"a category the tenant lacks", func(d *config.Config) {
			planBasis(config.PlanCentersCost)(d)
			d.Company("2000").Close.Variance.PlanCategory = "FORECAST"
		}, "companies[2000].close.variance.plan_category"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc = fixture.Document(t)
			tc.set(doc)
			var got []config.Finding
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				var err error
				got, err = setup.Check(ctx, nil, doc)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("findings: got %v, want none", got)
			case tc.want != "" && (len(got) != 1 || got[0].Path != tc.want):
				t.Errorf("findings: got %v, want one at %s", got, tc.want)
			}
		})
	}
	planBasis(config.PlanCentersCost)(doc)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "check", check)
	if err := check(context.Background()); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	if err := (closing.Setup{}).Apply(context.Background(), doc); err != nil {
		t.Errorf("Apply: got %v, want nothing to do", err)
	}
}

// TestVarianceDatabaseFailures calls the report against the plan and the
// prior period with each of its database statements failing in turn, and
// outside a unit of work; a page token of another report is refused and a
// page size of zero too.
func TestVarianceDatabaseFailures(t *testing.T) {
	e, id := budgeted(t)
	activate(t, e.Tenant.ID, func(doc *config.Config) {
		planBasis(config.PlanCentersCost)(doc)
		doc.Company("2000").Close.Variance.Bases = append(doc.Company("2000").Close.Variance.Bases, config.BasisPriorPeriod)
	})
	h := endpoints(t)
	for _, basis := range []string{config.BasisPlan, config.BasisPriorPeriod} {
		t.Run(basis, func(t *testing.T) {
			call := func(ctx context.Context) error {
				_, err := h.ListPeriodCloseVariances(ctx, gen.ListPeriodCloseVariancesParams{ID: uuid.MustParse(id), Basis: basis})
				return err
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, basis, call)
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListPeriodCloseVariances(ctx, gen.ListPeriodCloseVariancesParams{ID: uuid.MustParse(id), Basis: config.BasisPlan, PageSize: gen.NewOptInt32(0)})
		return err
	})
	if got := problem.From(err).Code; got != "request.invalid_page_size" {
		t.Errorf("page size 0: got %s, want request.invalid_page_size", got)
	}
}

// TestVarianceBrokenData: settings that only a document written past the
// configuration context has (a threshold or percent that is no amount, a
// plan category the tenant lacks), and a close whose legal entity the
// tenancy context does not hold, fail the report rather than show it on
// other figures.
func TestVarianceBrokenData(t *testing.T) {
	e, id := budgeted(t)
	h := endpoints(t)
	report := func(close uuid.UUID) error {
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListPeriodCloseVariances(ctx, gen.ListPeriodCloseVariancesParams{ID: close, Basis: config.BasisPlan})
			return err
		})
	}
	cases := []struct {
		name string
		set  func(*config.CloseVariance)
		want string
	}{
		{"a threshold that is no amount", func(v *config.CloseVariance) { v.ThresholdAmount = "lots" }, "threshold of company 2000"},
		{"a percent that is no number", func(v *config.CloseVariance) { v.ThresholdPercent = "ten" }, "percent threshold of company 2000"},
		{"a category the tenant lacks", func(v *config.CloseVariance) { v.PlanCategory = "GONE" }, "not held by the controlling context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activate(t, e.Tenant.ID, func(doc *config.Config) {
				planBasis(config.PlanCentersCost)(doc)
				tc.set(doc.Company("2000").Close.Variance)
			})
			if err := report(uuid.MustParse(id)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
	activate(t, e.Tenant.ID, planBasis(config.PlanCentersCost))
	orphan := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), 2026, 1, '2026-01-31', 1, $3, $3)`, e.Tenant.ID, orphan, e.Tenant.Human.ID)
	if err := report(orphan); err == nil || !strings.Contains(err.Error(), "not held by the tenancy context") {
		t.Errorf("an unknown legal entity: got %v, want the tenancy context's failure", err)
	}
}
