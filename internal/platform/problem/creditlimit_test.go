package problem_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestCreditExceedsInvoice: the refusal carries the context's code, the
// figures with their currency and, on gross_amount, what the invoice's
// gross still leaves, never less than nothing.
func TestCreditExceedsInvoice(t *testing.T) {
	invoice := uuid.New()
	cases := []struct {
		name   string
		f      problem.CreditFigures
		leaves string
	}{
		{"part of the gross is left", problem.CreditFigures{Gross: 11900, Credited: 10000, Attempted: 1901, Currency: "EUR", Exponent: 2}, "19.00"},
		{"credited past the gross already", problem.CreditFigures{Gross: 11900, Credited: 12000, Attempted: 1, Currency: "EUR", Exponent: 2}, "0.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := problem.CreditExceedsInvoice("receivables.credit_exceeds_invoice", invoice, "CI1", "CI2", c.f)
			if p.Status != 422 || p.Code != "receivables.credit_exceeds_invoice" {
				t.Errorf("got %d %s, want 422 receivables.credit_exceeds_invoice", p.Status, p.Code)
			}
			if len(p.Errors) != 1 {
				t.Fatalf("field errors: got %d, want 1", len(p.Errors))
			}
			if got, want := p.Errors[0].Field+": "+p.Errors[0].Message, "gross_amount: at most what the invoice's gross amount leaves: "+c.leaves; got != want {
				t.Errorf("field error: got %q, want %q", got, want)
			}
			want := problem.CreditLimit{InvoiceID: invoice, InvoiceNumber: "CI1", InvoiceGross: problem.Money{Amount: c.f.Gross.Plain(2), Currency: "EUR"},
				Credited: problem.Money{Amount: c.f.Credited.Plain(2), Currency: "EUR"}, Attempted: problem.Money{Amount: c.f.Attempted.Plain(2), Currency: "EUR"}}
			if p.CreditLimit == nil || *p.CreditLimit != want {
				t.Errorf("credit_limit: got %+v, want %+v", p.CreditLimit, want)
			}
		})
	}
}
