package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestRegressionSuites captures a stratified suite through the API and
// replays it under proposals.
func TestRegressionSuites(t *testing.T) {
	fx := fixture.New(t, append(perms, "config.regression.manage", "config.change.read")...)
	e := env{Env: fx, expense: fx.Account(t, "2000", "520000"), tax: fx.Account(t, "2000", "140000"), ap: fx.Account(t, "2000", "211000")}
	// An empty window has nothing to capture.
	e.Problem(422, "config.no_cases", "POST", "/v1/regression-suites", `{"name": "Empty"}`, "")

	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	posted := e.clean(t, po, "INV-1") // auto-post
	e.act(t, 200, id(posted), "match", "")
	e.act(t, 200, id(posted), "post", "")
	po2 := e.order(t, e.Supplier, "PO-2")
	e.receive(t, po2, 1, "10")
	held, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "154.70", "24.70", poItem(po2, 1, "10", "EA", "130.00")), "")
	e.act(t, 200, id(held), "match", "") // exception: price variance
	twice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-3", "185.64", "29.64", poItem(po2, 1, "12", "EA", "156.00")), "")
	e.act(t, 200, id(twice), "match", "") // exception: price variance and quantity
	// Near the auto-post limit of 50,000.00: 48,000 net on a large order.
	big := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-BIG', $3, $4, 'EUR', '2026-03-01', 'NT30', 'released', $5, $5)`,
		e.Tenant.ID, big, e.DE, e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code) VALUES ($1, $2, 1, 'Machine', 1, 'EA', 4800000, 1, $3, 'V1')`, e.Tenant.ID, big, e.expense)
	e.receive(t, big, 1, "1")
	large, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-4", "57120.00", "9120.00", poItem(big, 1, "1", "EA", "48000.00")), "")
	e.act(t, 200, id(large), "match", "")

	// The invoices are dated March: a window long enough to reach them.
	suite, tag := e.Must(201, "POST", "/v1/regression-suites", `{"name": "Baseline", "max_cases": 3, "window_days": 300}`, "")
	cases := suite["cases"].([]any)
	if suite["number"] != "RS-0001" {
		t.Errorf("suite number: got %v, want RS-0001", suite["number"])
	}
	if suite["config_version"] != float64(1) {
		t.Errorf("suite config version: got %v, want 1", suite["config_version"])
	}
	if suite["window_days"] != float64(300) {
		t.Errorf("suite window: got %v, want 300", suite["window_days"])
	}
	if suite["case_count"] != float64(3) {
		t.Errorf("suite case count: got %v, want 3", suite["case_count"])
	}
	if len(cases) != 3 {
		t.Errorf("suite cases: got %d, want 3", len(cases))
	}
	if tag != `"1"` {
		t.Errorf("suite etag: got %s, want \"1\"", tag)
	}
	whys := []string{}
	for _, c := range cases {
		whys = append(whys, c.(map[string]any)["why"].(string))
	}
	// Strata first, newest invoice first within each: one auto-post, one review (the large invoice is above the
	// 50,000.00 limit at 57,120 gross and near it), one exception, which also covers both issue kinds.
	if strings.Join(whys, " | ") != "decision auto_post | decision review; near the auto-post limit | decision exception; issue price_variance; issue quantity_exceeds_received" {
		t.Fatalf("strata: %v", whys)
	}
	full, _ := e.Must(201, "POST", "/v1/regression-suites", `{"name": "Everything", "window_days": 300}`, "")
	if full["number"] != "RS-0002" {
		t.Errorf("full suite number: got %v, want RS-0002", full["number"])
	}
	if full["case_count"] != float64(4) {
		t.Errorf("full suite case count: got %v, want 4", full["case_count"])
	}
	if got := len(full["strata"].([]any)); got != 7 {
		t.Errorf("full suite strata: got %d, want 7", got)
	}
	got, _ := e.Must(200, "GET", "/v1/regression-suites/"+suite["id"].(string), "", "")
	if got["name"] != "Baseline" {
		t.Errorf("get name: got %v, want Baseline", got["name"])
	}
	if n := len(got["cases"].([]any)); n != 3 {
		t.Errorf("get cases: got %d, want 3", n)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/regression-suites/"+uuid.NewString(), "", "")
	page, _ := e.Must(200, "GET", "/v1/regression-suites?config_version=1&page_size=1", "", "")
	if n := len(page["items"].([]any)); n != 1 {
		t.Fatalf("list items: got %d, want 1: %v", n, page)
	}
	if got := page["items"].([]any)[0].(map[string]any)["number"]; got != "RS-0002" {
		t.Errorf("list first: got %v, want RS-0002", got)
	}
	if page["next_page_token"] == nil {
		t.Fatalf("list next page token: got none, want one: %v", page)
	}
	rest, _ := e.Must(200, "GET", "/v1/regression-suites?config_version=1&page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if len(rest["items"].([]any)) != 1 || rest["items"].([]any)[0].(map[string]any)["number"] != "RS-0001" {
		t.Fatalf("list page 2: %v", rest)
	}
	if none, _ := e.Must(200, "GET", "/v1/regression-suites?config_version=7", "", ""); len(none["items"].([]any)) != 0 {
		t.Fatalf("other version: %v", none)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/regression-suites?order_by=name", "", "")

	// Replay: unchanged under the base, the held invoice changes under a wider tolerance.
	base := fixture.Document(t)
	tolerant := fixture.Document(t)
	tolerant.Companies[1].InvoiceTolerance.PricePercent = 10
	var captured []configuration.RegressionCase
	for _, c := range full["cases"].([]any) {
		m := c.(map[string]any)
		var issues []string
		for _, i := range m["expected_issues"].([]any) {
			issues = append(issues, i.(string))
		}
		captured = append(captured, configuration.RegressionCase{CaseNo: int32(m["case_no"].(float64)), InvoiceID: uuid.MustParse(m["supplier_invoice_id"].(string)),
			DocumentNumber: m["document_number"].(string), Company: m["company"].(string), ExpectedDecision: m["expected_decision"].(string), ExpectedIssues: issues})
	}
	replay := func(doc *config.Config) []configuration.RegressionFailure {
		t.Helper()
		var failures []configuration.RegressionFailure
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			failures, err = payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}}.Replay(ctx, doc, captured)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return failures
	}
	if f := replay(base); len(f) != 0 {
		t.Fatalf("baseline: %+v", f)
	}
	// In suite order: the exception case keeps its quantity issue, the sampled held invoice posts.
	f := replay(tolerant)
	if len(f) != 2 {
		t.Fatalf("tolerant failures: got %d, want 2: %+v", len(f), f)
	}
	if f[0].Case.DocumentNumber != twice["document_number"] {
		t.Errorf("first failure: got %s, want %v", f[0].Case.DocumentNumber, twice["document_number"])
	}
	if got := strings.Join(f[0].Issues, ","); got != payables.IssueQtyReceived {
		t.Errorf("first failure issues: got %s, want %s", got, payables.IssueQtyReceived)
	}
	if f[1].Case.DocumentNumber != held["document_number"] {
		t.Errorf("second failure: got %s, want %v", f[1].Case.DocumentNumber, held["document_number"])
	}
	if f[1].Decision != payables.DecisionAutoPost {
		t.Errorf("second failure decision: got %s, want %s", f[1].Decision, payables.DecisionAutoPost)
	}
	// A case whose invoice vanished is a failure of the replay, not a silent pass.
	missing := append([]configuration.RegressionCase(nil), captured...)
	missing[0].InvoiceID = uuid.New()
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}}.Replay(ctx, base, missing)
		return err
	}); err == nil {
		t.Fatal("missing invoice replayed")
	}

	today := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	for name, fn := range map[string]func(ctx context.Context) error{
		"capture": func(ctx context.Context) error {
			_, err := payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Now: func() time.Time { return today }}.Capture(ctx, base, 90*24*time.Hour, 0)
			return err
		},
		"replay": func(ctx context.Context) error {
			_, err := payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}}.Replay(ctx, tolerant, captured)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if fn(context.Background()) == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	// Without a maximum every invoice is a case, strata first.
	var all []configuration.RegressionCase
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		all, err = payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Now: func() time.Time { return today }}.Capture(ctx, base, 90*24*time.Hour, 0)
		return err
	}); err != nil {
		t.Fatalf("capture without a maximum: %v", err)
	}
	if len(all) != 4 || all[3].Why != "sample" {
		t.Fatalf("all cases: %+v", all)
	}
	// A configuration without the company decides its invoices without tolerances or limit, and is never near a limit.
	gone := fixture.Document(t)
	gone.Companies = gone.Companies[:1]
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		all, err = payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Now: func() time.Time { return today }}.Capture(ctx, gone, 90*24*time.Hour, 0)
		return err
	}); err != nil {
		t.Fatalf("capture without the company: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("without the company, cases: got %d, want 4: %+v", len(all), all)
	}
	if all[0].ExpectedDecision != payables.DecisionReview {
		t.Errorf("without the company, first decision: got %s, want %s", all[0].ExpectedDecision, payables.DecisionReview)
	}
	for i, c := range all {
		if strings.Contains(c.Why, "near") {
			t.Errorf("without the company, case %d why: got %q, want no mention of near", i, c.Why)
		}
	}
	_ = fmt.Sprint
}
