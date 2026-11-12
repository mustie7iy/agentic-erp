package sales

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestAsBilling: a refusal by receivables or the ledger names the billing
// document and its own fields; the problem it was made from is not
// changed; another error passes as it is.
func TestAsBilling(t *testing.T) {
	doc := BillingDocument{DocumentNumber: "BD7", Items: make([]BillingItem, 2)}
	const lead = "Billing document BD7 cannot be posted as a customer invoice: "
	for _, c := range []struct {
		name, field, detail string
		wantField, want     string
	}{
		{"the customer's line", "lines[0].account_id", "Line 1: blocked.", "customer_id", "The customer's line: blocked."},
		{"an item's line", "lines[2].account_id", "Line 3: blocked.", "items[1].account_id", "Item 2: blocked."},
		{"an item's line without a field of it", "lines[1]", "Line 2: refused.", "items[0]", "Item 1: refused."},
		{"an item's line refused for a center", "lines[1].cost_center_id", "Line 2 needs a cost center.", "items[0].account_id", "Item 1 needs a cost center."},
		{"a tax line", "lines[3].account_id", "Line 4: blocked.", "items", "The output tax on the items: blocked."},
		{"a line the detail does not lead with", "lines[1].tax_code", "The entry is refused.", "items[0].tax_code", "The entry is refused."},
		{"an item's cost center", "items[1].cost_center_id", "Item 2 needs a cost center.", "items[1].account_id", "Item 2 needs a cost center."},
		{"an item's profit center", "items[0].profit_center_id", "Item 1 needs a profit center.", "items[0].account_id", "Item 1 needs a profit center."},
		{"a field the document has too", "items[0].tax_code", "No rate.", "items[0].tax_code", "No rate."},
		{"a center that is no item's", "cost_center_id", "No center.", "cost_center_id", "No center."},
	} {
		t.Run(c.name, func(t *testing.T) {
			made := problem.Invalid("ledger.refused", c.field, c.detail)
			var got *problem.Problem
			if !errors.As(asBilling(fmt.Errorf("receivables: post: %w", made), doc), &got) {
				t.Fatal("got no problem, want the refusal")
			}
			if got.Code != "ledger.refused" {
				t.Errorf("code: got %s, want ledger.refused, as it was made", got.Code)
			}
			if got.Errors[0].Field != c.wantField {
				t.Errorf("field: got %s, want %s", got.Errors[0].Field, c.wantField)
			}
			if got.Detail != lead+c.want {
				t.Errorf("detail: got %q, want %q", got.Detail, lead+c.want)
			}
			if made.Errors[0].Field != c.field {
				t.Errorf("the problem it was made from: got its field changed to %s, want %s left", made.Errors[0].Field, c.field)
			}
			if made.Detail != c.detail {
				t.Errorf("the problem it was made from: got its detail changed to %q, want %q left", made.Detail, c.detail)
			}
		})
	}
	t.Run("a refusal without a field", func(t *testing.T) {
		var got *problem.Problem
		if !errors.As(asBilling(problem.Conflict("ledger.period_not_open", "Period 2 is closed."), doc), &got) {
			t.Fatal("got no problem, want the refusal")
		}
		if got.Detail != lead+"Period 2 is closed." {
			t.Errorf("detail: got %q, want %q", got.Detail, lead+"Period 2 is closed.")
		}
	})
	t.Run("another error", func(t *testing.T) {
		failed := errors.New("connection lost")
		got := asBilling(failed, doc)
		if !errors.Is(got, failed) {
			t.Errorf("got %v, want the error as it is", got)
		}
		if p := new(problem.Problem); errors.As(got, &p) {
			t.Errorf("got the problem %v, want no problem made of another error", p)
		}
	})
}
