package receivables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A relieved invoice cleared apart (ADR-0049 As built batch 94): only an
// incoming payment settles what was relieved, since only it charges the
// relieved tax again.

// clearing is the JSON of a ledger clearing of the items on the day.
func (e discountEnv) clearing(day string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": %q, "items": [%s]}`, e.le, day, strings.Join(items, ", "))
}

// charged is the tax charged again of the invoice, in minor units.
func (e discountEnv) charged(t *testing.T, invoice map[string]any) string {
	t.Helper()
	var tax string
	pgtest.Scan(t, `SELECT coalesce(sum(tax_amount), 0)::text FROM receivables.bad_debt_recharge WHERE tenant_id = $1 AND customer_invoice_id = $2`,
		[]any{e.Tenant.ID, invoice["id"]}, &tax)
	return tax
}

// TestRelievedInvoiceClearing (acceptance): of an invoice of AED 105.00
// wholly written off and its 5.00 of VAT relieved, a ledger clearing of
// its item against a credit of 105.00 posted straight to the customer
// would settle the debt and charge none of the 5.00 again: it is refused
// (422 receivables.relieved_invoice_clearing naming the credit), and so
// is one of its write-off's credit against a debit posted to the customer
// and one of the residual item a payment left against a credit; so is
// one of its item against its own and another relieved invoice's
// write-off credits, which would settle the one with the other's. Its item
// cleared with its write-off's credit, which settles nothing paid, is
// cleared and charges nothing; so is a clearing of an invoice without a
// relief.
func TestRelievedInvoiceClearing(t *testing.T) {
	e := reliefEnv(t)
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	writeOff := e.writtenOff(t, invoice, "105.00", "2026-03-15")
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, writeOff, "AS", "2026-08-01", "2026-08-10"), "")
	item := itemRef(invoice["journal_entry_id"], 1)
	t.Run("its item against a credit to the customer", func(t *testing.T) {
		p := e.In(t).Problem(422, "receivables.relieved_invoice_clearing", "POST", "/v1/clearings", e.clearing("2026-09-15", item, e.credited(t, 10500, "2026-09-15")), "")
		if field(p) != "items[1]" {
			t.Errorf("the field: got %v, want items[1]", field(p))
		}
		if !strings.Contains(fmt.Sprint(p["detail"]), invoice["document_number"].(string)) {
			t.Errorf("the detail: got %q, want it to name invoice %v", fmt.Sprint(p["detail"]), invoice["document_number"])
		}
	})
	t.Run("its write-off's credit against a debit to the customer", func(t *testing.T) {
		debit := e.credited(t, -10500, "2026-09-15")
		e.In(t).Problem(422, "receivables.relieved_invoice_clearing", "POST", "/v1/clearings", e.clearing("2026-09-15", debit, itemRef(writeOff, 2)), "")
	})
	t.Run("a residual item a payment left against a credit", func(t *testing.T) {
		paid := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
		e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(paid, e.writtenOff(t, paid, "105.00", "2026-03-15"), "AS", "2026-08-01", "2026-08-10"), "")
		payment, _ := e.Must(201, "POST", "/v1/incoming-payments", e.paying(e.receipt(t, 3500, "2026-09-15").ID, "2026-09-15",
			e.leaving(itemRef(paid["journal_entry_id"], 1), "70.00")), "")
		residual := itemRef(payment["journal_entry_id"], int(items(t, payment)[0]["residual"].(map[string]any)["line_no"].(float64)))
		p := e.In(t).Problem(422, "receivables.relieved_invoice_clearing", "POST", "/v1/clearings", e.clearing("2026-09-16", e.credited(t, 7000, "2026-09-16"), residual), "")
		if field(p) != "items[0]" {
			t.Errorf("the field: got %v, want items[0]", field(p))
		}
		if got := e.charged(t, paid); got != "167" {
			t.Errorf("the tax charged again: got %s, want 167, the payment's alone", got)
		}
	})
	t.Run("its item against another relieved invoice's write-off credit", func(t *testing.T) {
		a := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
		offA := e.writtenOff(t, a, "50.00", "2026-03-15")
		e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(a, offA, "AS", "2026-08-01", "2026-08-10"), "")
		b := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
		offB := e.writtenOff(t, b, "55.00", "2026-03-15")
		e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(b, offB, "AS", "2026-08-01", "2026-08-10"), "")
		p := e.In(t).Problem(422, "receivables.relieved_invoice_clearing", "POST", "/v1/clearings",
			e.clearing("2026-09-15", itemRef(a["journal_entry_id"], 1), itemRef(offA, 2), itemRef(offB, 2)), "")
		if field(p) != "items[2]" {
			t.Errorf("the field: got %v, want items[2], the other invoice's credit", field(p))
		}
	})
	t.Run("its item with its write-off's credit", func(t *testing.T) {
		e.Must(201, "POST", "/v1/clearings", e.clearing("2026-09-15", item, itemRef(writeOff, 2)), "")
		if got := e.charged(t, invoice); got != "0" {
			t.Errorf("the tax charged again: got %s, want none", got)
		}
	})
	t.Run("items of no relieved invoice", func(t *testing.T) {
		other := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
		e.Must(201, "POST", "/v1/clearings", e.clearing("2026-09-15", itemRef(other["journal_entry_id"], 1), e.credited(t, 10500, "2026-09-15")), "")
	})
}

// TestReliefOfAnInvoiceClearedApart: an invoice written off in full whose
// item a ledger clearing settled against a credit posted straight to the
// customer before any relief is no bad debt: its relief is refused (422
// receivables.relieved_invoice_clearing naming customer_invoice_id and
// the clearing). One whose item was cleared with its write-off's credit
// alone is relieved.
func TestReliefOfAnInvoiceClearedApart(t *testing.T) {
	e := reliefEnv(t)
	settled := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	off := e.writtenOff(t, settled, "105.00", "2026-03-15")
	c, _ := e.Must(201, "POST", "/v1/clearings", e.clearing("2026-03-20", itemRef(settled["journal_entry_id"], 1), e.credited(t, 10500, "2026-03-20")), "")
	p := e.In(t).Problem(422, "receivables.relieved_invoice_clearing", "POST", "/v1/bad-debt-reliefs", relief(settled, off, "AS", "2026-08-01", "2026-08-10"), "")
	if field(p) != "customer_invoice_id" {
		t.Errorf("the field: got %v, want customer_invoice_id", field(p))
	}
	if !strings.Contains(fmt.Sprint(p["detail"]), c["id"].(string)) {
		t.Errorf("the detail: got %q, want it to name clearing %v", fmt.Sprint(p["detail"]), c["id"])
	}
	alone := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	aloneOff := e.writtenOff(t, alone, "105.00", "2026-03-15")
	e.Must(201, "POST", "/v1/clearings", e.clearing("2026-03-20", itemRef(alone["journal_entry_id"], 1), itemRef(aloneOff, 2)), "")
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(alone, aloneOff, "AS", "2026-08-01", "2026-08-10"), "")
}

// TestReliefRacesAClearing: a relief and a ledger clearing of one
// customer run one after the other, in either order, without a deadlock:
// a clearing of the invoice's item against a credit holding the customer
// makes a relief claimed meanwhile wait and then refuse it; a relief
// holding the customer makes the clearing wait and then refuses it.
func TestReliefRacesAClearing(t *testing.T) {
	e := reliefEnv(t)
	h := endpoints(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	lg := app.Ledger(app.Config{Pages: pages})
	clear := func(invoice map[string]any, credit string) func(ctx context.Context) error {
		ref := func(entry any, line int32) gen.ClearingItemRef {
			return gen.ClearingItemRef{FiscalYear: 2026, JournalEntryID: uuid.MustParse(fmt.Sprint(entry)), LineNo: line}
		}
		return func(ctx context.Context) error {
			_, err := lg.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.le, ClearingDate: date(t, "2026-09-15"),
				Items: []gen.ClearingItemRef{ref(invoice["journal_entry_id"], 1), ref(entryOf(t, credit), 1)}}, gen.CreateClearingParams{})
			return err
		}
	}
	claim := func(invoice map[string]any, writeOff string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CreateBadDebtRelief(ctx, &gen.BadDebtReliefCreate{CustomerInvoiceID: uuid.MustParse(invoice["id"].(string)),
				WriteOffJournalEntryID: uuid.MustParse(writeOff), TaxCode: "AS", CustomerNotifiedOn: gen.NewOptDate(date(t, "2026-08-01")),
				PostingDate: gen.NewOptDate(date(t, "2026-09-15"))}, gen.CreateBadDebtReliefParams{})
			return err
		}
	}
	for _, clearingFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("clearing first ", clearingFirst), func(t *testing.T) {
			invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
			off := e.writtenOff(t, invoice, "105.00", "2026-03-15")
			clearing, relieving := clear(invoice, e.credited(t, 10500, "2026-09-15")), claim(invoice, off)
			if clearingFirst {
				first, second := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, clearing, relieving)
				if p := problem.From(second); first != nil || p.Code != "receivables.relieved_invoice_clearing" {
					t.Errorf("got %v and %v, want the clearing done and the relief refused", first, second)
				}
				return
			}
			first, second := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, relieving, clearing)
			if p := problem.From(second); first != nil || p.Code != "receivables.relieved_invoice_clearing" {
				t.Errorf("got %v and %v, want the relief claimed and the clearing refused", first, second)
			}
		})
	}
}

// TestRelievedClearingDatabaseFailures runs a relief and then a clearing
// of the relieved invoice's item with its write-off's credit, which the
// guard reads and lets through, with each of their database statements
// failing in turn.
func TestRelievedClearingDatabaseFailures(t *testing.T) {
	e := reliefEnv(t)
	h := endpoints(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	lg := app.Ledger(app.Config{Pages: pages})
	invoice := e.invoiced(t, "2026-01-10", e.item("100.00", "S5", "AE-DU"))
	off := e.writtenOff(t, invoice, "105.00", "2026-03-15")
	items := []gen.ClearingItemRef{{FiscalYear: 2026, JournalEntryID: uuid.MustParse(invoice["journal_entry_id"].(string)), LineNo: 1},
		{FiscalYear: 2026, JournalEntryID: uuid.MustParse(off), LineNo: 2}}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "relieve a written-off invoice", func(ctx context.Context) error {
		_, err := h.CreateBadDebtRelief(ctx, &gen.BadDebtReliefCreate{CustomerInvoiceID: uuid.MustParse(invoice["id"].(string)),
			WriteOffJournalEntryID: uuid.MustParse(off), TaxCode: "AS", CustomerNotifiedOn: gen.NewOptDate(date(t, "2026-08-01")),
			PostingDate: gen.NewOptDate(date(t, "2026-08-10"))}, gen.CreateBadDebtReliefParams{})
		return err
	})
	e.Must(201, "POST", "/v1/bad-debt-reliefs", relief(invoice, off, "AS", "2026-08-01", "2026-08-10"), "")
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "clear a relieved invoice's item with its write-off's credit", func(ctx context.Context) error {
		_, err := lg.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.le, ClearingDate: date(t, "2026-09-15"), Items: items}, gen.CreateClearingParams{})
		return err
	})
}
