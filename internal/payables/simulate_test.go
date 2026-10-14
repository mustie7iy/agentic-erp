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

// simulation replays the scenario's invoices on a fixed day.
type simulation struct {
	e     env
	today time.Time
}

func (s simulation) run(tb testing.TB, from, to *config.Config, window time.Duration) configuration.Simulation {
	tb.Helper()
	var sim configuration.Simulation
	err := apitest.InRequest(tb, s.e.Tenant, s.e.Tenant.Human, func(ctx context.Context) (err error) {
		sim, err = payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Window: window, Now: func() time.Time { return s.today }}.Simulate(ctx, from, to)
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	return sim
}

func decisions(sim configuration.Simulation) map[string]any {
	return sim.Detail["decisions"].(map[string]any)
}

// TestSimulationReplaysInvoices replays the window's invoices under
// proposed configurations: tolerances and the auto-post limit change
// decisions, account determination and tax codes move postings, and a
// proposal that drops something an invoice needs cannot be evaluated.
func TestSimulationReplaysInvoices(t *testing.T) {
	e := setup(t)
	s := simulation{e: e, today: time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)}
	base := fixture.Document(t)
	tolerant := fixture.Document(t)
	tolerant.Companies[1].InvoiceTolerance.PricePercent = 10
	var held, twice map[string]any
	if !t.Run("invoices", func(t *testing.T) { held, twice = simulatedInvoices(t, e.in(t)) }) {
		t.FailNow()
	}
	if !t.Run("unchanged", func(t *testing.T) {
		same := s.run(t, base, base, 0)
		if !same.Passed {
			t.Errorf("unchanged passed: got false, want true: %+v", same)
		}
		if same.Detail["invoices"] != 3 {
			t.Errorf("unchanged invoices: got %v, want 3", same.Detail["invoices"])
		}
		if decisions(same)["changed"] != 0 {
			t.Errorf("unchanged decisions changed: got %v, want 0", decisions(same)["changed"])
		}
		if got := same.Detail["postings"].([]posting); len(got) != 0 {
			t.Errorf("unchanged postings: got %+v, want none", got)
		}
		if got := decisions(same)["base"].(map[string]int)[payables.DecisionAutoPost]; got != 1 {
			t.Errorf("unchanged auto-post decisions: got %d, want 1", got)
		}
		if got := decisions(same)["base"].(map[string]int)[payables.DecisionException]; got != 2 {
			t.Errorf("unchanged exception decisions: got %d, want 2", got)
		}
		if want := "3 invoices of the last 90 days replayed: 0 decided differently, 0 posting rules move"; !strings.Contains(same.Summary, want) {
			t.Errorf("unchanged summary: got %q, want it to contain %q", same.Summary, want)
		}
	}) {
		t.FailNow()
	}
	if !t.Run("decisions change", func(t *testing.T) { checkDecisionChanges(t, s, base, tolerant, held, twice) }) {
		t.FailNow()
	}
	if !t.Run("postings move", func(t *testing.T) { checkPostingsMove(t, s, base) }) {
		t.FailNow()
	}
	if !t.Run("something dropped", func(t *testing.T) { checkDropped(t, s, base, tolerant) }) {
		t.FailNow()
	}
	t.Run("every statement failing", func(t *testing.T) {
		simulate := func(ctx context.Context) error {
			_, err := payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Now: func() time.Time { return s.today }}.Simulate(ctx, base, tolerant)
			return err
		}
		if simulate(context.Background()) == nil {
			t.Fatal("simulated outside a unit of work")
		}
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "simulate", simulate)
	})
}

// simulatedInvoices enters a clean posted invoice, a held one, one with two
// issues and one without an order (not replayed); it returns the held one
// and the one with two issues.
func simulatedInvoices(t *testing.T, e env) (held, twice map[string]any) {
	t.Helper()
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	posted := e.clean(t, po, "INV-1") // 157.80 gross, 22.80 tax: clean, posted
	e.act(t, 200, id(posted), "match", "")
	e.act(t, 200, id(posted), "post", "")
	po2 := e.order(t, e.Supplier, "PO-2")
	e.receive(t, po2, 1, "10")
	// 10 EA at 13.00 against an order price of 12.00: a price variance beyond 2% / 5.00, held.
	held, _ = e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "154.70", "24.70", poItem(po2, 1, "10", "EA", "130.00")), "")
	if m := e.act(t, 200, id(held), "match", ""); m["is_clean"] != false {
		t.Fatalf("match: %v", m)
	}
	// 12 EA at 13.00 against 10 received: a price variance and a quantity issue.
	twice, _ = e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-3", "185.64", "29.64", poItem(po2, 1, "12", "EA", "156.00")), "")
	e.act(t, 200, id(twice), "match", "")
	// An invoice without an order is not replayed.
	e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "V-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	return held, twice
}

// checkDecisionChanges: a wider tolerance posts the held invoice and leaves
// the other with its quantity issue only; a lower auto-post limit sends the
// clean invoice to a person.
func checkDecisionChanges(t *testing.T, s simulation, base, tolerant *config.Config, held, twice map[string]any) {
	t.Helper()
	sim := s.run(t, base, tolerant, 0)
	changes := sim.Detail["changes"].([]change)
	if !sim.Passed {
		t.Errorf("tolerance passed: got false, want true: %+v", sim)
	}
	if len(changes) != 2 {
		t.Fatalf("tolerance changes: got %d, want 2: %+v", len(changes), changes)
	}
	if changes[0].DocumentNumber != twice["document_number"] {
		t.Errorf("first change: got %s, want %v", changes[0].DocumentNumber, twice["document_number"])
	}
	if changes[0].Base != payables.DecisionException {
		t.Errorf("first change base: got %s, want %s", changes[0].Base, payables.DecisionException)
	}
	if changes[0].Proposed != payables.DecisionException {
		t.Errorf("first change proposed: got %s, want %s", changes[0].Proposed, payables.DecisionException)
	}
	if got := strings.Join(changes[0].Issues, ","); got != payables.IssueQtyReceived {
		t.Errorf("first change issues: got %s, want %s", got, payables.IssueQtyReceived)
	}
	if changes[1].DocumentNumber != held["document_number"] {
		t.Errorf("second change: got %s, want %v", changes[1].DocumentNumber, held["document_number"])
	}
	if changes[1].Proposed != payables.DecisionAutoPost {
		t.Errorf("second change proposed: got %s, want %s", changes[1].Proposed, payables.DecisionAutoPost)
	}
	if got := decisions(sim)["proposed"].(map[string]int)[payables.DecisionAutoPost]; got != 2 {
		t.Errorf("tolerance auto-post decisions: got %d, want 2", got)
	}
	limited := fixture.Document(t)
	limited.Companies[1].AutoPostLimit = "100.00"
	if sim = s.run(t, base, limited, 0); len(sim.Detail["changes"].([]change)) != 1 || sim.Detail["changes"].([]change)[0].Proposed != payables.DecisionReview {
		t.Fatalf("limit: %+v", sim)
	}
}

// checkPostingsMove: account determination and tax rates move postings;
// the amounts are the window's.
func checkPostingsMove(t *testing.T, s simulation, base *config.Config) {
	t.Helper()
	moved := fixture.Document(t)
	moved.Companies[1].AccountDetermination.InputTax = "100000"
	moved.Companies[1].TaxCodes[0].RatePercent = "20"
	sim := s.run(t, base, moved, 0)
	postings := sim.Detail["postings"].([]posting)
	if len(postings) != 2 {
		t.Fatalf("postings: got %d, want 2: %+v", len(postings), postings)
	}
	if postings[0].Key != "account_determination.input_tax" {
		t.Errorf("first posting key: got %s, want account_determination.input_tax", postings[0].Key)
	}
	if postings[0].Invoices != 3 {
		t.Errorf("first posting invoices: got %d, want 3", postings[0].Invoices)
	}
	if postings[0].Amount != 2280+2470+2964 {
		t.Errorf("first posting amount: got %d, want %d", postings[0].Amount, 2280+2470+2964)
	}
	if postings[1].Key != "tax_codes.V1.rate_percent" {
		t.Errorf("second posting key: got %s, want tax_codes.V1.rate_percent", postings[1].Key)
	}
	if postings[1].Base != "19" {
		t.Errorf("second posting base: got %s, want 19", postings[1].Base)
	}
	if postings[1].Proposed != "20" {
		t.Errorf("second posting proposed: got %s, want 20", postings[1].Proposed)
	}
	if postings[1].Amount != 12000+13000+15600 {
		t.Errorf("second posting amount: got %d, want %d", postings[1].Amount, 12000+13000+15600)
	}
}

// checkDropped: dropping a tax code or a company the invoices use fails the
// simulation; a base without the company compares against nothing
// (decisions only); nothing is in a short window.
func checkDropped(t *testing.T, s simulation, base, tolerant *config.Config) {
	t.Helper()
	gone := fixture.Document(t)
	gone.Companies[1].TaxCodes = gone.Companies[1].TaxCodes[1:]
	sim := s.run(t, base, gone, 0)
	if sim.Passed {
		t.Errorf("tax code gone passed: got true, want false")
	}
	if got := len(sim.Detail["breaks"].([]broken)); got != 3 {
		t.Errorf("tax code gone breaks: got %d, want 3", got)
	}
	if !strings.Contains(sim.Summary, "3 cannot be evaluated") {
		t.Errorf("tax code gone summary: got %q, want it to contain %q", sim.Summary, "3 cannot be evaluated")
	}
	gone.Companies = gone.Companies[:1]
	if sim = s.run(t, base, gone, 0); sim.Passed || !strings.Contains(sim.Detail["breaks"].([]broken)[0].Reason, "company 2000") {
		t.Fatalf("company gone: %+v", sim)
	}
	sim = s.run(t, gone, base, 0)
	if !sim.Passed {
		t.Errorf("new company passed: got false, want true: %+v", sim)
	}
	if got := sim.Detail["postings"].([]posting); len(got) != 0 {
		t.Errorf("new company postings: got %+v, want none", got)
	}
	if got := len(sim.Detail["changes"].([]change)); got != 1 {
		t.Errorf("new company changes: got %d, want 1", got)
	}
	if sim = s.run(t, base, tolerant, 24*time.Hour); sim.Detail["invoices"] != 0 || !sim.Passed {
		t.Fatalf("short window: %+v", sim)
	}
}

// The detail's slices are the package's own types; tests read them as such.
type (
	change  = payables.Decision
	broken  = payables.Broken
	posting = payables.Posting
)
