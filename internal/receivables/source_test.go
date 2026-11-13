package receivables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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

// TestCreditMemoWithGivenTax: a credit memo another context enters in
// process for an invoice may give its items' taxes, the shares of what
// the invoice bore (T85d; ADR-0063, As built T85d); it posts those taxes,
// each code's shared out among its components, not what the codes give
// again; a code that gives no tax refuses a given one, and only such a
// memo gives a tax.
func TestCreditMemoWithGivenTax(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	entered, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", e.item("10.00", "A1"), e.item("2.00", "A0")), "")
	invoice := entered["id"].(string)
	e.Must(200, "POST", "/v1/customer-invoices/"+invoice+":post", `{"posting_date": "2026-03-10"}`, `"1"`)
	ref := uuid.MustParse(invoice)
	tax := func(m money.Minor) *money.Minor { return &m }
	a1, a0 := "A1", "A0"
	memo := func(source string, items ...receivables.ItemInput) receivables.Input {
		return receivables.Input{LegalEntityID: e.DE, CustomerID: e.customer, Kind: receivables.KindCreditMemo, ReferenceInvoiceID: &ref,
			InvoiceDate: date(t, "2026-03-20"), Currency: "EUR", Items: items, Source: &receivables.Source{ID: uuid.Must(uuid.NewV7()), Number: source}}
	}
	item := func(net, code string, given *money.Minor) receivables.ItemInput {
		in := receivables.ItemInput{AccountID: e.revenue, Net: receivables.Amount{Value: net, Currency: "EUR"}, Tax: given}
		if code != "" {
			in.TaxCode = &code
		}
		return in
	}
	// enter enters the memo and posts it when post is set; lines are its entry's output tax lines, read in the request,
	// whose transaction is rolled back.
	enter := func(in receivables.Input, post bool) (v receivables.Invoice, lines string, err error) {
		err = e.inRequest(t, func(ctx context.Context) error {
			var err error
			if v, err = h.CreateInvoice(ctx, in); err != nil || !post {
				return err
			}
			if v, err = h.PostInvoice(ctx, v.ID, func(int32) error { return nil }, nil); err != nil {
				return err
			}
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT coalesce(string_agg(format('%s %s %s', a.number, l.tax_code, l.amount_tc), ', ' ORDER BY l.line_no), '')
				FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
				WHERE l.journal_entry_id = $1 AND a.number = '230000'`, *v.EntryID).Scan(&lines)
		})
		return v, lines, err
	}
	t.Run("the given taxes post", func(t *testing.T) {
		// A1 gives 0.63 on 3.33 and 0.19 on 1.00: 0.64 and 0.18 are given instead, 0.82 in all where the code gives 0.82
		// once; the zero-rated A0 bears none and posts no tax line.
		v, lines, err := enter(memo("CR1", item("3.33", a1, tax(64)), item("1.00", a1, tax(18)), item("0.50", "", nil), item("0.40", a0, tax(0))), true)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%d %d %d", v.Items[0].Tax, v.Items[1].Tax, v.Gross); got != "64 18 605" {
			t.Errorf("the items' tax and the memo's gross: got %s, want 64 18 605", got)
		}
		if lines != "230000 A1 82" {
			t.Errorf("the memo's output tax lines: got %s, want 230000 A1 82 (minor units)", lines)
		}
	})
	t.Run("a code that gives no tax", func(t *testing.T) {
		_, _, err := enter(memo("CR2", item("1.00", a0, tax(1))), true)
		if p := problem.From(err); p.Code != "receivables.tax_changed" {
			t.Errorf("got %v, want receivables.tax_changed", err)
		}
	})
	for _, c := range []struct {
		name string
		in   receivables.Input
	}{
		{"a memo without a source", func() receivables.Input { in := memo("CR3", item("1.00", a1, tax(19))); in.Source = nil; return in }()},
		{"an invoice", func() receivables.Input {
			in := memo("CR4", item("1.00", a1, tax(19)))
			in.Kind, in.ReferenceInvoiceID = "", nil
			return in
		}()},
		{"a negative tax", memo("CR5", item("1.00", a1, tax(-1)))},
		{"a tax on an item without a code", memo("CR6", item("1.00", "", tax(1)))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := enter(c.in, false); err == nil || !strings.Contains(err.Error(), "tax") {
				t.Errorf("got %v, want the given tax refused", err)
			}
		})
	}
}
