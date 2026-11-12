package receivables_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// TestInvoiceNamesItsSource: an invoice another context enters in process
// names the document it came from, by id and number, as it is stored and
// as the API gives it, and a document is invoiced once; an invoice a
// person enters names none (T79b; ADR-0063 §2, ADR-0067 §9).
func TestInvoiceNamesItsSource(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	source := receivables.Source{ID: uuid.Must(uuid.NewV7()), Number: "BD100001"}
	in := receivables.Input{LegalEntityID: e.DE, CustomerID: e.customer, InvoiceDate: date(t, "2026-03-10"), Currency: "EUR",
		Items: []receivables.ItemInput{{AccountID: e.revenue, Net: receivables.Amount{Value: "10.00", Currency: "EUR"}}}, Source: &source}
	err := e.inRequest(t, func(ctx context.Context) error {
		v, err := h.CreateInvoice(ctx, in)
		if err != nil {
			return err
		}
		if v.SourceDocumentID == nil || *v.SourceDocumentID != source.ID {
			t.Errorf("the invoice's source: got %v, want %s", v.SourceDocumentID, source.ID)
		}
		if v.SourceDocumentNumber == nil || *v.SourceDocumentNumber != source.Number {
			t.Errorf("the source's number: got %v, want %s", v.SourceDocumentNumber, source.Number)
		}
		res, err := h.GetCustomerInvoice(ctx, gen.GetCustomerInvoiceParams{ID: v.ID})
		if err != nil {
			return err
		}
		read := res.(*gen.GetCustomerInvoiceOKHeaders).Response
		if got := read.SourceDocumentID.Or(uuid.Nil); got != source.ID {
			t.Errorf("source_document_id: got %s, want %s", got, source.ID)
		}
		if got := read.SourceDocumentNumber.Or(""); got != source.Number {
			t.Errorf("source_document_number: got %q, want %s", got, source.Number)
		}
		if _, err := h.CreateInvoice(ctx, in); err == nil || !strings.Contains(err.Error(), "customer_invoice_source_once") {
			t.Errorf("a second invoice of the document: got %v, want it refused by customer_invoice_source_once", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("an invoice a person enters names none", func(t *testing.T) {
		got, _ := e.In(t).Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("10.00", "")), "")
		for _, f := range []string{"source_document_id", "source_document_number"} {
			if v, ok := got[f]; ok {
				t.Errorf("%s: got %v, want it absent", f, v)
			}
		}
	})
}
