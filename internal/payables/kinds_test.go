package payables_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestNegativeAmountsRefused: a supplier invoice's gross and tax amounts
// are not negative whatever its kind, which gives the direction (T34): a
// credit memo is entered as one, never as an invoice of negative amounts.
func TestNegativeAmountsRefused(t *testing.T) {
	e := setup(t)
	for name, body := range map[string]string{
		"gross": e.invoice(e.Supplier, "NEG-1", "-100.00", "0.00", accountItem(e.expense, "-100.00")),
		"tax":   e.invoice(e.Supplier, "NEG-2", "100.00", "-19.00", accountItem(e.expense, "119.00")),
	} {
		t.Run(name, func(t *testing.T) {
			c := e.Client
			c.T = t
			c.Problem(422, "payables.negative_amount", "POST", "/v1/supplier-invoices", body, "")
		})
	}
	if got := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "supplier_invoice_amounts_not_negative"}).Code; got != "payables.negative_amount" {
		t.Errorf("constraint: got %s, want payables.negative_amount", got)
	}
}
