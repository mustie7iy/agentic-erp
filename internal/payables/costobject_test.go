package payables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestCostObjectFields: a supplier invoice's cost object custom fields
// name centers of its company valid on its posting date, else its invoice
// date, checked at entry and again on the date it posts (ADR-0046 As built
// T56k).
func TestCostObjectFields(t *testing.T) {
	f := fixture.NewWith(t, nil, fixture.CostObjectFields, perms...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	c := e.CostCenters(t, fixture.GRIRAccounts)
	foreign := e.ForeignProfitCenter(t)
	invoice := func(ref, ext, posting string) string {
		body := strings.Replace(e.invoice(e.Supplier, ref, "1.00", "0.00", centeredItem(e.expense, "1.00", uuid.Nil, uuid.Nil)),
			`"items"`, `"extensions": `+ext+posting+`, "items"`, 1)
		return body
	}
	t.Run("named and posted", func(t *testing.T) {
		e := e.in(t)
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", invoice("INV-C1", fmt.Sprintf(`{"cost_center": %q, "profit_center": %q}`, c.Locked, c.Profit), ""), "")
		if posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-21"}`); posted["status"] != "posted" {
			t.Errorf("status: got %v, want posted", posted["status"])
		}
	})
	t.Run("refused at entry", func(t *testing.T) {
		for name, x := range map[string]struct {
			ext, posting, field, code string
		}{
			"another company's profit center": {fmt.Sprintf(`{"profit_center": %q}`, foreign), "", "extensions.profit_center", "not_in_company"},
			"an unknown cost center":          {fmt.Sprintf(`{"cost_center": %q}`, uuid.New()), "", "extensions.cost_center", "not_found"},
			// The posting date, not the invoice date (2026-03-10), is judged.
			"before the cost center's validity": {fmt.Sprintf(`{"cost_center": %q}`, c.Admin), `, "posting_date": "2025-12-31"`, "extensions.cost_center", "not_valid_on_date"},
		} {
			t.Run(name, func(t *testing.T) {
				p := e.in(t).Problem(422, "request.invalid_extension", "POST", "/v1/supplier-invoices", invoice("INV-R", x.ext, x.posting), "")
				wantOneFieldError(t, p, x.field+" "+x.code)
			})
		}
	})
	t.Run("judged again on the date it posts", func(t *testing.T) {
		e := e.in(t)
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", invoice("INV-C2", fmt.Sprintf(`{"cost_center": %q}`, c.Admin), ""), "")
		p := e.Problem(422, "request.invalid_extension", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2025-12-31"}`, e.tag(t, id(inv)))
		wantOneFieldError(t, p, "extensions.cost_center not_valid_on_date")
	})
}

// wantOneFieldError checks that a problem has the one field error "field
// code".
func wantOneFieldError(t *testing.T, p map[string]any, want string) {
	t.Helper()
	var got []string
	errs, _ := p["errors"].([]any)
	for _, e := range errs {
		f, _ := e.(map[string]any)
		got = append(got, fmt.Sprint(f["field"], " ", f["code"]))
	}
	if strings.Join(got, ", ") != want {
		t.Errorf("field errors: got %v, want %s", got, want)
	}
}
