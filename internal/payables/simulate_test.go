package payables_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestSimulationReplaysInvoices replays the window's invoices under
// proposed configurations: tolerances and the auto-post limit change
// decisions, account determination and tax codes move postings, and a
// proposal that drops something an invoice needs cannot be evaluated.
func TestSimulationReplaysInvoices(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	posted := e.clean(t, po, "INV-1") // 157.80 gross, 22.80 tax: clean, posted
	e.act(t, 200, id(posted), "match", "")
	e.act(t, 200, id(posted), "post", "")
	po2 := e.order(t, e.Supplier, "PO-2")
	e.receive(t, po2, 1, "10")
	// 10 EA at 13.00 against an order price of 12.00: a price variance beyond 2% / 5.00, held.
	held, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "154.70", "24.70", poItem(po2, 1, "10", "EA", "130.00")), "")
	if m := e.act(t, 200, id(held), "match", ""); m["is_clean"] != false {
		t.Fatalf("match: %v", m)
	}
	// 12 EA at 13.00 against 10 received: a price variance and a quantity issue.
	twice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-3", "185.64", "29.64", poItem(po2, 1, "12", "EA", "156.00")), "")
	e.act(t, 200, id(twice), "match", "")
	// An invoice without an order is not replayed.
	e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "V-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")

	base := fixture.Document(t)
	today := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	run := func(from, to *config.Config, window time.Duration) configuration.Simulation {
		t.Helper()
		var sim configuration.Simulation
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			sim, err = payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Window: window, Now: func() time.Time { return today }}.Simulate(ctx, from, to)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return sim
	}
	decisions := func(sim configuration.Simulation) map[string]any { return sim.Detail["decisions"].(map[string]any) }

	same := run(base, base, 0)
	if !same.Passed || same.Detail["invoices"] != 3 || decisions(same)["changed"] != 0 || len(same.Detail["postings"].([]posting)) != 0 ||
		decisions(same)["base"].(map[string]int)[payables.DecisionAutoPost] != 1 || decisions(same)["base"].(map[string]int)[payables.DecisionException] != 2 ||
		!strings.Contains(same.Summary, "3 invoices of the last 90 days replayed: 0 decided differently, 0 posting rules move") {
		t.Fatalf("unchanged: %+v", same)
	}

	// A wider tolerance posts the held invoice and leaves the other with its quantity issue only.
	tolerant := fixture.Document(t)
	tolerant.Companies[1].InvoiceTolerance.PricePercent = 10
	sim := run(base, tolerant, 0)
	changes := sim.Detail["changes"].([]change)
	if !sim.Passed || len(changes) != 2 || changes[0].DocumentNumber != twice["document_number"] || changes[0].Base != payables.DecisionException ||
		changes[0].Proposed != payables.DecisionException || strings.Join(changes[0].Issues, ",") != payables.IssueQtyReceived ||
		changes[1].DocumentNumber != held["document_number"] || changes[1].Proposed != payables.DecisionAutoPost ||
		decisions(sim)["proposed"].(map[string]int)[payables.DecisionAutoPost] != 2 {
		t.Fatalf("tolerance: %+v", sim)
	}
	// A lower auto-post limit sends the clean invoice to a person.
	limited := fixture.Document(t)
	limited.Companies[1].AutoPostLimit = "100.00"
	if sim = run(base, limited, 0); len(sim.Detail["changes"].([]change)) != 1 || sim.Detail["changes"].([]change)[0].Proposed != payables.DecisionReview {
		t.Fatalf("limit: %+v", sim)
	}
	// Account determination and tax rates move postings; the amounts are the window's.
	moved := fixture.Document(t)
	moved.Companies[1].AccountDetermination.InputTax = "100000"
	moved.Companies[1].TaxCodes[0].RatePercent = "20"
	sim = run(base, moved, 0)
	postings := sim.Detail["postings"].([]posting)
	if len(postings) != 2 || postings[0].Key != "account_determination.input_tax" || postings[0].Invoices != 3 || postings[0].Amount != 2280+2470+2964 ||
		postings[1].Key != "tax_codes.V1.rate_percent" || postings[1].Base != "19" || postings[1].Proposed != "20" || postings[1].Amount != 12000+13000+15600 {
		t.Fatalf("postings: %+v", postings)
	}
	// Dropping a tax code or a company the invoices use fails the simulation.
	gone := fixture.Document(t)
	gone.Companies[1].TaxCodes = gone.Companies[1].TaxCodes[1:]
	if sim = run(base, gone, 0); sim.Passed || len(sim.Detail["breaks"].([]broken)) != 3 || !strings.Contains(sim.Summary, "3 cannot be evaluated") {
		t.Fatalf("tax code gone: %+v", sim)
	}
	gone.Companies = gone.Companies[:1]
	if sim = run(base, gone, 0); sim.Passed || !strings.Contains(sim.Detail["breaks"].([]broken)[0].Reason, "company 2000") {
		t.Fatalf("company gone: %+v", sim)
	}
	// A base without the company compares against nothing: decisions only.
	if sim = run(gone, base, 0); !sim.Passed || len(sim.Detail["postings"].([]posting)) != 0 || len(sim.Detail["changes"].([]change)) != 1 {
		t.Fatalf("new company: %+v", sim)
	}
	// Nothing in a short window.
	if sim = run(base, tolerant, 24*time.Hour); sim.Detail["invoices"] != 0 || !sim.Passed {
		t.Fatalf("short window: %+v", sim)
	}

	simulate := func(ctx context.Context) error {
		_, err := payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Now: func() time.Time { return today }}.Simulate(ctx, base, tolerant)
		return err
	}
	if simulate(context.Background()) == nil {
		t.Fatal("simulated outside a unit of work")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "simulate", simulate)
}

// The detail's slices are the package's own types; tests read them as such.
type (
	change  = payables.Decision
	broken  = payables.Broken
	posting = payables.Posting
)
