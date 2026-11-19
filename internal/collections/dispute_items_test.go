package collections_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// disputeItems is the JSON of a dispute case over the items, each "entry
// amount currency" naming line 1 of the entry, with the fields extra adds.
func disputeItems(e env, items []string, extra string) string {
	list := make([]string, len(items))
	for i, it := range items {
		f := strings.Fields(it)
		list[i] = fmt.Sprintf(`{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1, "disputed_amount": {"amount": %q, "currency": %q}}`, f[0], f[1], f[2])
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "items": [%s], "reason": "Delivered late"%s}`, e.DE, strings.Join(list, ", "), extra)
}

// itemsOf are a case's items as "item_no reference amount local", by
// number.
func itemsOf(t *testing.T, c map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range objects(t, c, "items") {
		out = append(out, fmt.Sprintf("%v %v %s %s", it["item_no"], it["reference"], amount(t, it, "disputed_amount"), amount(t, it, "local_disputed_amount")))
	}
	return strings.Join(out, "; ")
}

// TestDisputeCaseOverSeveralItems: one case disputes two of Northwind's
// invoices, each by its own amount; it reads back and lists with its items,
// its own line its first item's and its amounts their totals; a dunning run
// leaves both out, naming the case for each; the worklist counts one case
// and both amounts; once the case is closed both are dunned again (T86c
// acceptance).
func TestDisputeCaseOverSeveralItems(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	got, tag := e.Must(201, "POST", "/v1/dispute-cases", disputeItems(e, []string{entries[0].ID.String() + " 10.00 EUR",
		entries[1].ID.String() + " 50.00 EUR"}, ""), "")
	id := apitest.String(t, got, "id")
	if items, want := itemsOf(t, got), "1 CI-A 10.00 EUR 10.00 EUR; 2 CI-B 50.00 EUR 50.00 EUR"; items != want {
		t.Errorf("items: got %q, want %q", items, want)
	}
	if c, want := fmt.Sprintf("%v %v %s %s", got["journal_entry_id"], got["reference"], amount(t, got, "disputed_amount"),
		amount(t, got, "local_disputed_amount")), entries[0].ID.String()+" CI-A 60.00 EUR 60.00 EUR"; c != want {
		t.Errorf("the case: got %s, want its first item's line and the items' totals, %s", c, want)
	}
	read, _ := e.Must(200, "GET", "/v1/dispute-cases/"+id, "", "")
	page, _ := e.Must(200, "GET", "/v1/dispute-cases?customer_id="+e.north.String(), "", "")
	if a, b, c := mustJSON(t, read), mustJSON(t, objects(t, page, "items")[0]), mustJSON(t, got); a != c || b != c {
		t.Errorf("read back: got %s, listed %s, want %s", a, b, c)
	}
	if events := e.caseEvents(t); len(events) != 1 || len(apitest.List(t, events[0], "items")) != 2 {
		t.Errorf("DisputeCaseChanged: got %v, want one event naming both items", events)
	}
	run1, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	want := "CI-A " + id + " BP70000002 2026-03-31 91 100.00 EUR 100.00 EUR; CI-B " + id + " BP70000002 2026-06-25 5 50.00 EUR 50.00 EUR"
	if d := disputedOf(t, run1); d != want {
		t.Errorf("disputed: got %q, want %q", d, want)
	}
	if p := e.proposed(t, apitest.String(t, run1, "id")); p != "CI-USD 0->1" {
		t.Errorf("proposed: got %q, want CI-A and CI-B left out", p)
	}
	w := e.worklist(t, keyDate, "")
	north := objects(t, w, "items")[0]
	if c := fmt.Sprintf("%v %s %v", north["customer_id"], amount(t, north, "disputed_amount"), north["open_dispute_cases"]); c !=
		e.north.String()+" 60.00 EUR 1" {
		t.Errorf("Northwind on the worklist: got %s, want 60.00 EUR disputed in one case", c)
	}
	if c := fmt.Sprintf("%s %v", amount(t, w, "disputed_amount"), w["open_dispute_cases"]); c != "60.00 EUR 1" {
		t.Errorf("the worklist's totals: got %s, want 60.00 EUR in one case", c)
	}
	e.Must(200, "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "rejected", "reason": "Delivered as agreed"}`, tag)
	again, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	if p := e.proposed(t, apitest.String(t, again, "id")); p != "CI-USD 0->1, CI-A 0->1, CI-B 0->1" {
		t.Errorf("once the case is closed: got %q, want both items proposed again", p)
	}
	t.Run("an item of a closed case in a new case", func(t *testing.T) {
		e.In(t).Must(201, "POST", "/v1/dispute-cases", disputeItems(e, []string{entries[1].ID.String() + " 1.00 EUR"}, ""), "")
	})
}

// TestDisputeItemRefusals: what a case's items must be (T86c acceptance).
func TestDisputeItemRefusals(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	usd := e.post(t, item{customer: e.north, amount: 1500, currency: "USD", day: "2026-04-01", ref: "CI-NUSD"})
	a, b, c := entries[0].ID.String(), entries[1].ID.String(), entries[2].ID.String()
	e.open(t, disputeItems(e, []string{b + " 1.00 EUR"}, ""))
	for _, r := range []struct {
		name, body, code, field string
		status                  int
	}{
		{"another customer's item", disputeItems(e, []string{a + " 1.00 EUR", entries[7].ID.String() + " 1.00 USD"}, ""),
			"collections.item_not_open", "items[1].journal_entry_id", 422},
		{"an item named twice", disputeItems(e, []string{a + " 1.00 EUR", a + " 2.00 EUR"}, ""), "request.validation_failed", "items[1].journal_entry_id", 422},
		{"an item in another currency", disputeItems(e, []string{a + " 1.00 EUR", usd[0].ID.String() + " 1.00 USD"}, ""),
			"collections.currency_mismatch", "items[1].journal_entry_id", 422},
		{"an amount in another currency", disputeItems(e, []string{a + " 1.00 EUR", c + " 1.00 USD"}, ""),
			"collections.currency_mismatch", "items[1].disputed_amount.currency", 422},
		{"more than an item owes", disputeItems(e, []string{a + " 1.00 EUR", c + " 70.01 EUR"}, ""),
			"collections.invalid_disputed_amount", "items[1].disputed_amount.amount", 422},
		{"an item of an open case, not the first", disputeItems(e, []string{c + " 1.00 EUR", b + " 1.00 EUR"}, ""), "collections.dispute_exists", "", 409},
		{"both forms", strings.Replace(disputeItems(e, []string{a + " 1.00 EUR"}, ""), `"items"`, `"line_no": 1, "items"`, 1),
			"collections.items_or_item", "items", 422},
		{"one form in part", fmt.Sprintf(`{"legal_entity_id": %q, "journal_entry_id": %q, "reason": "x"}`, e.DE, a), "collections.items_or_item", "items", 422},
		{"neither form", fmt.Sprintf(`{"legal_entity_id": %q, "reason": "x"}`, e.DE), "collections.items_or_item", "items", 422},
	} {
		t.Run(r.name, func(t *testing.T) {
			got := e.In(t).Problem(r.status, r.code, "POST", "/v1/dispute-cases", r.body, "")
			if r.field != "" {
				if f := fieldOf(t, got); f != r.field {
					t.Errorf("field: got %q, want %q", f, r.field)
				}
			}
		})
	}
}

// fieldOf is the field a problem's first error names.
func fieldOf(t *testing.T, p map[string]any) string {
	t.Helper()
	errs := objects(t, p, "errors")
	if len(errs) == 0 {
		t.Fatalf("got %v, want a problem naming a field", p)
	}
	return fmt.Sprint(errs[0]["field"])
}

// TestDisputeItemsThroughThePort: what only a port could answer: items
// whose amounts in the company's currency add up to more than an amount
// holds are refused, and a Go caller naming no item, or more than a case
// takes, is refused as the contract would.
func TestDisputeItemsThroughThePort(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	open := func(h *collections.Endpoints, items []collections.CaseItemInput) error {
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.OpenCase(ctx, collections.CaseInput{LegalEntityID: e.DE, Items: items, Reason: "x"})
			return err
		})
	}
	h := endpoints(t)
	h.OpenItems = hugeItems{}
	whole := []collections.CaseItemInput{{Item: ledger.ItemRef{FiscalYear: 2026, EntryID: entries[0].ID, LineNo: 1}, Amount: "100.00", Currency: "EUR"},
		{Item: ledger.ItemRef{FiscalYear: 2026, EntryID: entries[1].ID, LineNo: 1}, Amount: "50.00", Currency: "EUR", Field: "items[1]."}}
	if p := problem.From(open(h, whole)); p.Code != "collections.amount_too_large" {
		t.Errorf("got %v, want collections.amount_too_large", p)
	}
	many := make([]collections.CaseItemInput, collections.MaxCaseItems+1)
	for name, items := range map[string][]collections.CaseItemInput{"no item": nil, "too many items": many} {
		t.Run(name, func(t *testing.T) {
			if p := problem.From(open(endpoints(t), items)); p.Code != "request.validation_failed" {
				t.Errorf("got %v, want request.validation_failed", p)
			}
		})
	}
}
