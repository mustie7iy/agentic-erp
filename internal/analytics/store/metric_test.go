package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/endpoint"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// codes names legal entities by their company codes, as erp-server does.
type codes struct{}

func (codes) CompanyCode(ctx context.Context, id uuid.UUID) (string, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return le.Code, ok, err
}

// metricChange: an agent proposes that operating expenses count account
// 610000 alone; the simulation shows the affected metrics before and
// after; the agent cannot activate it; once approved and activated a
// person's query answers under the new definition, and under the old one
// when it names the earlier configuration version.
func (p parity) metricChange(t *testing.T) {
	p.Tenant.AllCompanies(t, p.Tenant.Agent)
	agent, human := p.As(p.Tenant.Agent).In(t), p.In(t)
	m := model(t)
	doc := fixture.Document(t)
	doc.Analytics = &m.Analytics
	for i := range doc.Analytics.Metrics {
		if doc.Analytics.Metrics[i].Name == "expenses" {
			doc.Analytics.Metrics[i].Filter = map[string][]string{"account_type": {"expense"}, "account": {"610000"}}
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cr, _ := agent.Must(201, "POST", "/v1/change-requests", `{"title": "Expenses: 610000 only", "base_version": 1, "proposed": `+string(b)+`}`, "")
	path := "/v1/change-requests/" + apitest.String(t, cr, "id")
	submitted, tag := agent.Must(200, "POST", path+":submit", "", `"1"`)
	if submitted["status"] != configuration.AwaitingEvidence {
		t.Fatalf("submitted: got status %v, want %s", submitted["status"], configuration.AwaitingEvidence)
	}
	agent.Must(202, "POST", path+":simulate", "", tag)
	deps := fixture.Deps()
	deps.Simulator = configuration.Simulators{deps.Simulator, endpoint.Simulation{Endpoints: &endpoint.Endpoints{Reader: direct{p.r}, Companies: codes{}}}}
	w := &configuration.SimulationWorker{Dependencies: deps, Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("simulation: got %d %v, want a job run", n, err)
	}
	got, tag := agent.Must(200, "GET", path, "", "")
	if got["status"] != configuration.PendingApproval {
		t.Fatalf("simulated: got status %v, want %s: %v", got["status"], configuration.PendingApproval, got)
	}
	p.simulated(t, apitest.List(t, got, "evidence"))
	p.disclosed(t, path)
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":activate", "", tag)
	env := p.Env
	env.Client = human
	env.Approve(t, apitest.String(t, cr, "id"))
	_, tag = human.Must(200, "GET", path, "", "")
	if active, _ := human.Must(200, "POST", path+":activate", "", tag); active["status"] != configuration.Active {
		t.Fatalf("activated: got %v, want active", active)
	}

	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	c := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: p.Tenant.Human}
	for version, want := range map[string]string{"": "2 [[2000 120.00]]", `, "config_version": 1`: "1 [[1000 45.50] [2000 120.00]]"} {
		res, _ := c.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["expenses"], "by": ["company"], "order_by": "company"`+version+`}`, "")
		if got := fmt.Sprint(res["config_version"], " ", values(res)); got != want || res["source"] != analytics.SourceStore {
			t.Errorf("expenses by company%s: got %s from %v, want %s from the store", version, got, res["source"], want)
		}
	}
	c.Problem(422, "config.unknown_version", "POST", "/v1/analytics-queries", `{"measures": ["expenses"], "config_version": 9}`, "")
	p.unanswerable(t, agent, w, doc)
}

// unanswerable: a proposed metric the semantic layer refuses on the data
// (EUR and USD summed, no company to keep them apart) fails its
// simulation's operation for good rather than retry it.
func (p parity) unanswerable(t *testing.T, agent apitest.Client, w *configuration.SimulationWorker, doc *config.Config) {
	t.Helper()
	doc.Analytics.Metrics = append(doc.Analytics.Metrics, config.Metric{Name: "aa_total", Label: "Total", Type: analytics.Money, Source: "gl_lines",
		Aggregate: "sum", Dimensions: []string{"fiscal_year"}})
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cr, _ := agent.Must(201, "POST", "/v1/change-requests", `{"title": "Total", "base_version": 2, "proposed": `+string(b)+`}`, "")
	path := "/v1/change-requests/" + apitest.String(t, cr, "id")
	_, tag := agent.Must(200, "POST", path+":submit", "", `"1"`)
	op, _ := agent.Must(202, "POST", path+":simulate", "", tag)
	if n, err := w.RunOnce(context.Background()); err != nil || n < 1 {
		t.Fatalf("simulation: got %d %v, want a job run", n, err)
	}
	got, _ := agent.Must(200, "GET", "/v1/operations/"+apitest.String(t, op, "id"), "", "")
	if got["status"] != "failed" || !strings.Contains(fmt.Sprint(got["error"]), "analytics.invalid_query") {
		t.Errorf("operation: got %v, want failed with analytics.invalid_query", got)
	}
}

// simulated checks the simulation evidence: the affected metrics, the
// expenses of 1000 moving from 45.50 to nothing.
func (p parity) simulated(t *testing.T, evidence []any) {
	t.Helper()
	for _, e := range evidence {
		ev := e.(map[string]any)
		if ev["kind"] != "simulation" {
			continue
		}
		if ev["passed"] != true {
			t.Errorf("simulation: got not passed, want passed: %v", ev)
		}
		var metrics []string
		var expenses []any
		for _, mc := range ev["detail"].(map[string]any)["metrics"].([]any) {
			mc := mc.(map[string]any)
			metrics = append(metrics, mc["metric"].(string))
			if mc["metric"] == "expenses" {
				expenses = mc["values"].([]any)
			}
		}
		if want := []string{"expenses", "operating_margin", "operating_profit"}; !reflect.DeepEqual(metrics, want) {
			t.Errorf("affected metrics: got %v, want %v", metrics, want)
		}
		want := []any{map[string]any{"company": "1000", "fiscal_year": "2026", "before": "45.50", "after": ""},
			map[string]any{"company": "2000", "fiscal_year": "2026", "before": "120.00", "after": "120.00"}}
		if !reflect.DeepEqual(expenses, want) {
			t.Errorf("expenses before and after: got %v, want %v", expenses, want)
		}
		return
	}
	t.Fatalf("evidence: got %v, want a simulation", evidence)
}

// values are a result's rows as their cells' values.
func values(res map[string]any) [][]any {
	out := [][]any{}
	for _, row := range res["rows"].([]any) {
		var cells []any
		for _, c := range row.([]any) {
			cells = append(cells, c.(map[string]any)["value"])
		}
		out = append(out, cells)
	}
	return out
}

// disclosed: a reader of the change request is shown the simulated values
// of their own companies alone, whatever the requester's access: a reader
// of 1000 its values, one of 2000 its, one of no company none; and the
// summary counts those alone, so a reader of 2000 (whose values do not
// move) is not told that 1000's do.
func (p parity) disclosed(t *testing.T, path string) {
	t.Helper()
	for company, summary := range map[string]string{"1000": "3 of 3 values", "2000": "0 of 3 values", "": "0 of 0 values"} {
		t.Run("reader of "+company, func(t *testing.T) {
			companies := []string{}
			if company != "" {
				companies = append(companies, company)
			}
			got, _ := p.As(p.person(t, "", companies)).In(t).Must(200, "GET", path, "", "")
			shown := map[string]int{}
			for _, e := range apitest.List(t, got, "evidence") {
				e := e.(map[string]any)
				if e["kind"] != "simulation" {
					continue
				}
				if !strings.Contains(e["summary"].(string), summary) {
					t.Errorf("summary: got %q, want it to count %s", e["summary"], summary)
				}
				for _, mc := range apitest.List(t, e["detail"].(map[string]any), "metrics") {
					for _, v := range apitest.List(t, mc.(map[string]any), "values") {
						shown[v.(map[string]any)["company"].(string)]++
					}
				}
			}
			want := map[string]int{}
			if company != "" {
				want[company] = 3 // expenses, operating_margin and operating_profit in 2026
			}
			if !reflect.DeepEqual(shown, want) {
				t.Errorf("values shown by company: got %v, want %v", shown, want)
			}
		})
	}
}
