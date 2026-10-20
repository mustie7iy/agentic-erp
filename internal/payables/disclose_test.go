package payables_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// usInvoice enters a clean invoice of 120.00 USD in legal entity 1000 on
// an order of 10 EA at 12.00 without a tax code, all of it received.
func (e env) usInvoice(t *testing.T) {
	t.Helper()
	po, gm, expense := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), e.Account(t, "1000", "520000")
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-US', $3, $4, 'USD', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		e.Tenant.ID, po, e.US, e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, valuation_price, valuation_price_unit) VALUES ($1, $2, 1, 'Paper', 10, 'EA', 1200, 1, $3, 1200, 1)`, e.Tenant.ID, po, expense)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
	pgtest.Exec(t, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date, posted_by)
		VALUES ($1, $2, $3, $4, '101', '2026-03-05', '2026-03-05', $5)`, e.Tenant.ID, gm, gm.String(), e.US, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code)
		VALUES ($1, $2, 1, $3, 1, 10, 'EA')`, e.Tenant.ID, gm, po)
	e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "US-1",
		"invoice_date": "2026-03-10", "gross_amount": {"amount": "120.00", "currency": "USD"}, "tax_amount": {"amount": "0.00", "currency": "USD"},
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}, "amount": {"amount": "120.00", "currency": "USD"}}]}`,
		e.US, e.Supplier, po), "")
}

// reader is a person holding the tenant's permissions in the legal entities
// alone (none: no company).
func (e env) reader(t *testing.T, name string, legalEntities ...uuid.UUID) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, 'active', $2, $2)`, e.Tenant.ID, a.ID, pgtest.Issuer(t), a.Subject, name)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, e.Tenant.ID, a.ID)
	for _, le := range legalEntities {
		pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, e.Tenant.ID, a.ID, le)
	}
	return a
}

// simulatedChange proposes lower auto-post limits in both companies and a
// tax rate of company 2000, and has the simulation worker record its
// evidence as of day; it returns the change request's path and the
// operation's.
func (e env) simulatedChange(t *testing.T) (string, string) {
	t.Helper()
	deps := fixture.Deps()
	deps.Simulator = payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates, Now: func() time.Time { return day }}
	deps.Regressor = payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates, Now: func() time.Time { return day }}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, deps, configuration.NewRegressionSuite{Name: "Baseline"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	doc := fixture.Document(t)
	doc.Companies[0].AutoPostLimit, doc.Companies[1].AutoPostLimit = "100.00", "100.00"
	doc.Companies[1].TaxCodes[0].RatePercent = "9"
	proposed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cr, _ := e.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Limits", "rationale": "Policy", "base_version": 1, "proposed": %s}`, proposed), "")
	path := "/v1/change-requests/" + id(cr)
	_, tag := e.Must(200, "POST", path+":submit", "", `"1"`)
	op, _ := e.Must(202, "POST", path+":simulate", "", tag)
	w := &configuration.SimulationWorker{Dependencies: deps, Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
	if did, err := w.Simulate(context.Background(), e.Tenant.ID, uuid.MustParse(id(op))); err != nil || !did {
		t.Fatalf("simulate: got %v %v, want it run", did, err)
	}
	return path, "/v1/operations/" + id(op)
}

// simulationOf is the simulation evidence of a change request.
func simulationOf(t *testing.T, cr map[string]any) (string, payables.Shown) {
	t.Helper()
	for _, ev := range apitest.List(t, cr, "evidence") {
		if m := ev.(map[string]any); m["kind"] == "simulation" {
			b, err := json.Marshal(m["detail"])
			if err != nil {
				t.Fatal(err)
			}
			var shown payables.Shown
			if err := json.Unmarshal(b, &shown); err != nil {
				t.Fatal(err)
			}
			if m["passed"] != true {
				t.Errorf("passed: got %v, want true, the change's verdict for every reader", m["passed"])
			}
			return m["summary"].(string), shown
		}
	}
	t.Fatalf("simulation evidence: got none in %v", cr["evidence"])
	return "", payables.Shown{}
}

// TestSimulationShownByCompany: one change request's simulation, read by
// people of different company access, shows each the invoices of their own
// companies alone, counted over those; a person of no company sees none.
// The summary recorded (in the operation's result too) has no count.
func TestSimulationShownByCompany(t *testing.T) {
	f := fixture.New(t, append(perms, "config.change.read", "config.change.propose", "config.regression.manage", "platform.operation.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	if !t.Run("invoices", func(t *testing.T) {
		simulatedInvoices(t, e.in(t)) // three of company 2000: one clean, two held
		e.in(t).usInvoice(t)          // one clean of company 1000
	}) {
		t.FailNow()
	}
	path, opPath := e.simulatedChange(t)
	cases := map[string]struct {
		as       pgtest.Actor
		summary  string
		invoices int
		changed  int
		postings int
		company  string // of every change and posting shown
	}{
		"every company": {e.Tenant.Human, "4 invoices of the last 90 days replayed: 2 decided differently, 1 posting rules move", 4, 2, 1, ""},
		"company 2000":  {e.reader(t, "DE", e.DE), "3 invoices of the last 90 days replayed: 1 decided differently, 1 posting rules move", 3, 1, 1, "2000"},
		"company 1000":  {e.reader(t, "US", e.US), "1 invoices of the last 90 days replayed: 1 decided differently, 0 posting rules move", 1, 1, 0, "1000"},
		"no company":    {e.reader(t, "None"), "0 invoices of the last 90 days replayed: 0 decided differently, 0 posting rules move", 0, 0, 0, "none"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			api := e.As(c.as)
			api.T = t
			got, _ := api.Must(200, "GET", path, "", "")
			summary, shown := simulationOf(t, got)
			checkShown(t, summary, shown, c.summary, c.invoices, c.changed, c.postings, c.company)
			list, _ := api.Must(200, "GET", "/v1/change-requests", "", "")
			if listed, _ := simulationOf(t, apitest.List(t, list, "items")[0].(map[string]any)); listed != c.summary {
				t.Errorf("listed summary: got %q, want %q", listed, c.summary)
			}
		})
	}
	t.Run("recorded", func(t *testing.T) {
		done, _ := e.Must(200, "GET", opPath, "", "")
		if got, want := apitest.Object(t, done, "result")["summary"], "Invoices on purchase orders of the last 90 days replayed under the proposal."; got != want {
			t.Errorf("operation result summary: got %v, want %q", got, want)
		}
	})
}

// checkShown checks what one reader is shown.
func checkShown(t *testing.T, summary string, shown payables.Shown, wantSummary string, invoices, changed, postings int, company string) {
	t.Helper()
	if summary != wantSummary {
		t.Errorf("summary: got %q, want %q", summary, wantSummary)
	}
	if shown.Invoices != invoices {
		t.Errorf("invoices: got %d, want %d", shown.Invoices, invoices)
	}
	if shown.Decisions.Changed != changed {
		t.Errorf("changed: got %d, want %d", shown.Decisions.Changed, changed)
	}
	if got := shown.Decisions.Unchanged; got != invoices-changed {
		t.Errorf("unchanged: got %d, want %d", got, invoices-changed)
	}
	if len(shown.Changes) != changed {
		t.Errorf("changes: got %d, want %d", len(shown.Changes), changed)
	}
	if len(shown.Postings) != postings {
		t.Errorf("postings: got %d, want %d", len(shown.Postings), postings)
	}
	total := 0
	for _, n := range shown.Decisions.Base {
		total += n
	}
	if total != invoices {
		t.Errorf("base decisions: got %d, want %d", total, invoices)
	}
	if company == "" {
		return
	}
	for _, c := range shown.Changes {
		if c.Company != company {
			t.Errorf("change of company %s: got one, want only %s", c.Company, company)
		}
	}
	for _, p := range shown.Postings {
		if p.Company != company {
			t.Errorf("posting of company %s: got one, want only %s", p.Company, company)
		}
	}
}

// TestDisclosureOfOtherEvidence: evidence other than this simulation is
// shown as it is; a simulation recorded with counts over every company,
// before they were kept by company, is shown whole to a reader of every
// company and to no other; a replay of no legal entity is shown to a
// reader of every company alone; a detail that does not read, or company
// access that cannot be read, fails the read rather than show it whole.
func TestDisclosureOfOtherEvidence(t *testing.T) {
	f := fixture.New(t, perms...)
	e := env{Env: f}
	narrow := e.reader(t, "DE", e.DE)
	legacy := configuration.Evidence{Kind: "simulation", Summary: "3 invoices of the last 90 days replayed: 1 decided differently, 0 posting rules move",
		Detail: json.RawMessage(`{"invoices": 3, "decisions": {"changed": 1}, "changes": [], "breaks": [], "postings": [], "window_days": 90, "other": 1}`)}
	nowhere := configuration.Evidence{Kind: "simulation", Summary: "Invoices on purchase orders of the last 90 days replayed under the proposal.",
		Detail: json.RawMessage(`{"window_days": 90, "invoices_by_company": [{"company": "9000", "invoices": 2, "base": {"review": 2}, "proposed": {"review": 2}}]}`)}
	broken := configuration.Evidence{Kind: "simulation", Summary: nowhere.Summary, Detail: json.RawMessage(fmt.Sprintf(`{"window_days": 90, "invoices_by_company": [
		{"company": "1000", "legal_entity_id": %q, "invoices": 2, "broken": 2, "breaks": [{"document_number": "SI-US1", "company": "1000", "reason": "r"}, {"document_number": "SI-US2", "company": "1000", "reason": "r"}]},
		{"company": "2000", "legal_entity_id": %q, "invoices": 1, "broken": 1, "breaks": [{"document_number": "SI-DE", "company": "2000", "reason": "r"}]}]}`, e.US, e.DE))}
	cases := map[string]struct {
		as          pgtest.Actor
		in          configuration.Evidence
		wantSummary string
		wantDetail  string
	}{
		"regression": {narrow, configuration.Evidence{Kind: "regression", Summary: "Suite RS-0001", Detail: json.RawMessage(`{"cases": 3}`)},
			"Suite RS-0001", `{"cases": 3}`},
		"another context's": {narrow, configuration.Evidence{Kind: "simulation", Summary: "Metrics.", Detail: json.RawMessage(`{"metrics": []}`)},
			"Metrics.", `{"metrics": []}`},
		"legacy, every company": {e.Tenant.Human, legacy, legacy.Summary, string(legacy.Detail)},
		"legacy, narrower": {narrow, legacy,
			"Invoices replayed under the proposal; recorded before the results were kept by company, so shown only to readers of every company.", `{"other":1}`},
		"no legal entity, every company": {e.Tenant.Human, nowhere, "2 invoices of the last 90 days replayed: 0 decided differently, 0 posting rules move", ""},
		"no legal entity, narrower": {narrow, nowhere, "0 invoices of the last 90 days replayed: 0 decided differently, 0 posting rules move",
			`{"breaks":[],"changes":[],"decisions":{"changed":0,"unchanged":0,"base":{},"proposed":{}},"invoices":0,"postings":[],"window_days":90}`},
		"breaks of another company": {narrow, broken, "1 invoices of the last 90 days replayed: 0 decided differently, 0 posting rules move; 1 cannot be evaluated under the proposal",
			`{"breaks":[{"invoice_id":"00000000-0000-0000-0000-000000000000","document_number":"SI-DE","company":"2000","reason":"r"}],"changes":[],"decisions":{"changed":0,"unchanged":0,"base":{},"proposed":{}},"invoices":1,"postings":[],"window_days":90}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var got configuration.Evidence
			if err := apitest.InRequest(t, e.Tenant, c.as, func(ctx context.Context) (err error) {
				got, err = payables.Disclosure{}.Disclose(ctx, c.in)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got.Summary != c.wantSummary {
				t.Errorf("summary: got %q, want %q", got.Summary, c.wantSummary)
			}
			if c.wantDetail != "" && string(got.Detail) != c.wantDetail {
				t.Errorf("detail: got %s, want %s", got.Detail, c.wantDetail)
			}
		})
	}
	for name, detail := range map[string]string{"not an object": `[1]`, "replays not a list": `{"invoices_by_company": 1}`} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, narrow, func(ctx context.Context) error {
				_, err := payables.Disclosure{}.Disclose(ctx, configuration.Evidence{Kind: "simulation", Detail: json.RawMessage(detail)})
				return err
			})
			pgtest.CheckErr(t, err, "payables: simulated invoices")
		})
	}
	t.Run("every statement failing", func(t *testing.T) {
		apitest.EveryFailure(t, e.Tenant, narrow, "disclose", func(ctx context.Context) error {
			_, err := payables.Disclosure{}.Disclose(ctx, nowhere)
			return err
		})
	})
	t.Run("outside a request", func(t *testing.T) {
		if _, err := (payables.Disclosure{}).Disclose(context.Background(), nowhere); err == nil {
			t.Error("disclosed outside a request: got no error, want one")
		}
	})
}
