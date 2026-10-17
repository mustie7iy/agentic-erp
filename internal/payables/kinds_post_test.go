package payables_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// posted matches and posts a document and returns it posted.
func (e env) posted(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	if m := e.act(t, 200, id(v), "match", ""); m["is_clean"] != true {
		t.Fatalf("match of %s: got issues %v, want clean", v["kind"], m["issues"])
	}
	return e.act(t, 200, id(v), "post", `{"posting_date": "2026-03-21"}`)
}

// TestCreditMemosAndSubsequentDebitsPost matches and posts each kind on a
// directly valued order: a credit memo against what was invoiced, as KG,
// Dr supplier / Cr the item's account and tax; a subsequent debit on an
// invoiced order item without quantity, as KR, Dr the item's account / Cr
// supplier; with their match issues when they credit more than was
// invoiced or revalue what was never invoiced, and their events signed by
// kind.
func TestCreditMemosAndSubsequentDebitsPost(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.posted(t, invoice)
	ref := fmt.Sprintf(`, "reference_invoice_id": %q`, id(invoice))
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "28.56", "4.56", ref, poItem(po, 1, "2", "EA", "24.00")), "")
	posted := e.posted(t, memo)
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", posted["journal_entry_id"]), "", "")
	if je["document_type"] != "KG" {
		t.Errorf("credit memo document type: got %v, want KG", je["document_type"])
	}
	got := e.entryLines(t, posted["journal_entry_id"])
	want := []line{{e.expense.String(), "-24.00", nil}, {e.tax.String(), "-4.56", nil}, {e.ap.String(), "28.56", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("credit memo lines: got %v, want %v", got, want)
	}
	postedEvents := events(t, e.Tenant, "supplier_invoice_posted")
	last := postedEvents[len(postedEvents)-1]
	if last["kind"] != "credit_memo" {
		t.Errorf("SupplierInvoicePosted kind: got %v, want credit_memo", last["kind"])
	}
	if got := last["gross_amount"].(map[string]any)["amount"]; got != "-28.56" {
		t.Errorf("SupplierInvoicePosted gross: got %v, want -28.56", got)
	}
	if got := last["tax_amount"].(map[string]any)["amount"]; got != "-4.56" {
		t.Errorf("SupplierInvoicePosted tax: got %v, want -4.56", got)
	}
	debit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_debit", "SD-1", "5.00", "0.00",
		fmt.Sprintf(`, "purchase_order_id": %q`, po), poItem(po, 1, "0", "EA", "5.00")), "")
	posted = e.posted(t, debit)
	got = e.entryLines(t, posted["journal_entry_id"])
	want = []line{{e.expense.String(), "5.00", nil}, {e.ap.String(), "-5.00", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("subsequent debit lines: got %v, want %v", got, want)
	}
	credit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_credit", "SC-0", "2.00", "0.00",
		fmt.Sprintf(`, "purchase_order_id": %q`, po), poItem(po, 1, "0", "EA", "2.00")), "")
	posted = e.posted(t, credit)
	je, _ = e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", posted["journal_entry_id"]), "", "")
	if je["document_type"] != "KG" {
		t.Errorf("subsequent credit document type: got %v, want KG", je["document_type"])
	}
	got = e.entryLines(t, posted["journal_entry_id"])
	want = []line{{e.expense.String(), "-2.00", nil}, {e.ap.String(), "2.00", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("subsequent credit lines: got %v, want %v", got, want)
	}
	for name, c := range map[string]struct{ body, issue string }{
		"credit beyond the invoiced": {e.document("credit_memo", "CM-2", "108.00", "0.00", "", poItem(po, 1, "9", "EA", "108.00")), "quantity_exceeds_invoiced"},
		"revaluing what was not invoiced": {e.document("subsequent_credit", "SC-1", "3.00", "0.00", fmt.Sprintf(`, "purchase_order_id": %q`, po),
			poItem(po, 2, "0", "KG", "3.00")), "nothing_invoiced"},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := e.Client
			c2.T = t
			v, _ := c2.Must(201, "POST", "/v1/supplier-invoices", c.body, "")
			m, _ := c2.Must(200, "POST", "/v1/supplier-invoices/"+id(v)+":match", "", e.tag(t, id(v)))
			issues := m["issues"].([]any)
			if len(issues) != 1 || issues[0].(map[string]any)["code"] != c.issue {
				t.Errorf("issues: got %v, want one %s", issues, c.issue)
			}
		})
	}
	t.Run("simulated by kind", func(t *testing.T) {
		sim, err := e.simulate(t, fixture.Document(t), fixture.Document(t))
		if err != nil {
			t.Fatal(err)
		}
		if got := sim.Detail["invoices"]; got != 6 {
			t.Errorf("simulated documents: got %v, want every kind's 6", got)
		}
	})
}

// TestCorrectionsAndTheirInvoice: a correction posts only while the
// invoice it names stands, checked under a lock at :post; an invoice is
// reversed only once the corrections posted against it are, and the
// reversal's event is signed by kind.
func TestCorrectionsAndTheirInvoice(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.posted(t, invoice)
	ref := fmt.Sprintf(`, "reference_invoice_id": %q`, id(invoice))
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "11.90", "1.90", ref, poItem(po, 1, "1", "EA", "10.00")), "")
	e.posted(t, memo)
	e.Problem(409, "payables.invoice_has_corrections", "POST", "/v1/supplier-invoices/"+id(invoice)+":reverse", `{"reason": "wrong"}`, e.tag(t, id(invoice)))
	e.act(t, 200, id(memo), "reverse", `{"reason": "wrong"}`)
	reversed := events(t, e.Tenant, "supplier_invoice_reversed")
	if got := reversed[len(reversed)-1]["gross_amount"].(map[string]any)["amount"]; got != "-11.90" {
		t.Errorf("credit memo's SupplierInvoiceReversed gross: got %v, want -11.90", got)
	}
	parked, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-2", "11.90", "1.90", ref, poItem(po, 1, "1", "EA", "10.00")), "")
	e.act(t, 200, id(parked), "match", "")
	e.act(t, 200, id(invoice), "reverse", `{"reason": "wrong"}`)
	e.Problem(409, "payables.reference_not_posted", "POST", "/v1/supplier-invoices/"+id(parked)+":post", "", e.tag(t, id(parked)))
}

// TestCreditMemoOnGRIR: on an order valued through GR/IR a credit memo
// credits GR/IR at the order price of the quantity it credits, the rest on
// price differences, and leaves the item received but no longer all
// invoiced; a subsequent debit moves no quantity and goes to price
// differences whole.
func TestCreditMemoOnGRIR(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	e.GRIR(t)
	grir, diff := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String()
	po := e.grirOrder(t, "PO-1")
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}}]}`, e.DE, po), "")
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.posted(t, invoice)
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "29.75", "4.75", "", poItem(po, 1, "2", "EA", "25.00")), "")
	got := e.entryLines(t, e.posted(t, memo)["journal_entry_id"])
	want := []line{{grir, "-24.00", po.String()}, {diff, "-1.00", nil}, {e.tax.String(), "-4.75", nil}, {e.ap.String(), "29.75", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("credit memo lines: got %v, want %v", got, want)
	}
	debit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_debit", "SD-1", "6.00", "0.00",
		fmt.Sprintf(`, "purchase_order_id": %q`, po), poItem(po, 1, "0", "EA", "6.00")), "")
	got = e.entryLines(t, e.posted(t, debit)["journal_entry_id"])
	want = []line{{grir, "0.00", po.String()}, {diff, "6.00", nil}, {e.ap.String(), "-6.00", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("subsequent debit lines: got %v, want %v", got, want)
	}
	credit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_credit", "SC-1", "4.00", "0.00",
		fmt.Sprintf(`, "purchase_order_id": %q`, po), poItem(po, 1, "0", "EA", "4.00")), "")
	got = e.entryLines(t, e.posted(t, credit)["journal_entry_id"])
	want = []line{{grir, "0.00", po.String()}, {diff, "-4.00", nil}, {e.ap.String(), "4.00", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("subsequent credit lines: got %v, want %v", got, want)
	}
	position := func(t *testing.T, want string) {
		t.Helper()
		page, _ := e.Must(200, "GET", "/v1/gr-ir-balances?open_only=false&legal_entity_id="+e.DE.String()+"&purchase_order_id="+po.String(), "", "")
		b := page["items"].([]any)[0].(map[string]any)
		got := fmt.Sprint(b["received_quantity"].(map[string]any)["value"], " ", b["invoiced_quantity"].(map[string]any)["value"], " ",
			b["balance"].(map[string]any)["amount"])
		if got != want {
			t.Errorf("position (received, invoiced, balance): got %s, want %s", got, want)
		}
	}
	position(t, "10 8 -24.00")
	e.act(t, 200, id(memo), "reverse", `{"reason": "wrong"}`)
	position(t, "10 10 0.00")
}

// TestCorrectionPostingFailures: every statement of posting a correction
// that names its invoice, and of reversing an invoice whose corrections
// are checked, reports its failure.
func TestCorrectionPostingFailures(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.posted(t, invoice)
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "11.90", "1.90",
		fmt.Sprintf(`, "reference_invoice_id": %q`, id(invoice)), poItem(po, 1, "1", "EA", "10.00")), "")
	e.act(t, 200, id(memo), "match", "")
	h := endpoints(nil)
	u := func(m map[string]any) uuid.UUID { return uuid.MustParse(id(m)) }
	for name, call := range map[string]func(ctx context.Context) error{
		"post a correction": func(ctx context.Context) error {
			_, err := h.PostSupplierInvoice(ctx, gen.OptSupplierInvoicePost{}, gen.PostSupplierInvoiceParams{ID: u(memo), IfMatch: "*"})
			return err
		},
		"reverse an invoice": func(ctx context.Context) error {
			_, err := h.ReverseSupplierInvoice(ctx, &gen.SupplierInvoiceReverse{Reason: "r"}, gen.ReverseSupplierInvoiceParams{ID: u(invoice), IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call) })
	}
}
