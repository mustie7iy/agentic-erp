package problem

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// CreditFigures are the amounts of a credit that would exceed the invoice
// it names, in that invoice's currency, which is the credit's: the
// invoice's gross amount, what the posted credits naming it already
// credit, and the gross amount of the credit refused. None is negative.
type CreditFigures struct {
	Gross, Credited, Attempted money.Minor
	Currency                   string
	Exponent                   int // the currency's minor-unit digits
}

// CreditExceedsInvoice refuses a credit that, with the posted credits
// naming the same invoice, would credit more than that invoice's gross
// amount (product owner 2026-11-10 for suppliers; ADR-0063 §7 for
// customers): a 422 with the context's code (payables.credit_exceeds_invoice,
// receivables.credit_exceeds_invoice) naming gross_amount, with what the
// invoice's gross still leaves, and the figures as the problem's
// credit_limit. document is the number of the credit refused. Both
// contexts answer the one refusal, so it is made here rather than twice.
func CreditExceedsInvoice(code string, invoice uuid.UUID, invoiceNumber, document string, f CreditFigures) *Problem {
	plain := func(m money.Minor) string { return m.Plain(f.Exponent) }
	in := func(m money.Minor) Money { return Money{Amount: plain(m), Currency: f.Currency} }
	p := Unprocessable(code, fmt.Sprintf("Invoice %s, which %s credits (reference_invoice_id), has a gross amount of %s %s, "+
		"of which posted credits naming it already credit %s; this one's gross_amount of %s would exceed it.",
		invoiceNumber, document, plain(f.Gross), f.Currency, plain(f.Credited), plain(f.Attempted)),
		FieldError{Field: "gross_amount", Code: "credit_exceeds_invoice",
			Message: "at most what the invoice's gross amount leaves: " + plain(max(f.Gross-f.Credited, 0))})
	p.CreditLimit = &CreditLimit{InvoiceID: invoice, InvoiceNumber: invoiceNumber, InvoiceGross: in(f.Gross), Credited: in(f.Credited),
		Attempted: in(f.Attempted)}
	return p
}
