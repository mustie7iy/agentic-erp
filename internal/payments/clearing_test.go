package payments_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// paidRequest requests a down payment from the supplier, pays it in a run
// released on the date and returns the request, paid.
func (e env) paidRequest(t *testing.T, supplier uuid.UUID, amount, runDate, extra string) map[string]any {
	t.Helper()
	r := e.request(t, supplier, amount, "2026-03-01", extra)
	e.release(t, e.run(t, runDate, runDate), "")
	paid, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(r), "", "")
	if paid["status"] != "paid" {
		t.Fatalf("request after its run: got %v, want paid", paid["status"])
	}
	return paid
}

// clearing is a :clearDownPayments body with the requests' down payments:
// line 1 of each request's payment entry.
func clearing(requests ...map[string]any) string {
	items := ""
	for i, r := range requests {
		if i > 0 {
			items += ", "
		}
		items += fmt.Sprintf(`{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}`, r["journal_entry_id"])
	}
	return `{"posting_date": "2026-04-02", "items": [` + items + `]}`
}

// TestClearDownPayments: a paid down payment is cleared against the
// supplier's final invoice (SAP F-54, T46d): the request is cleared, the
// transfer credits the down payment and its input tax and debits payables
// under the invoice's terms, and the next run pays the invoice less the
// down payment. The invoice is not reversed while the clearing stands;
// clearing more than the invoice, a request of another order, a request
// not paid, or against a document that is not a posted invoice is refused.
func TestClearDownPayments(t *testing.T) {
	e := downPaymentSetup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	paid := e.paidRequest(t, e.Supplier, "1190.00", "2026-03-10", `, "tax_code": "V1"`)
	big := e.paidRequest(t, e.Supplier, "3000.00", "2026-03-11", "")
	po := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-1', $3, $4, 'EUR', '2026-03-01', 'NT30', 'released', $5, $5)`,
		e.Tenant.ID, po, e.DE, e.Supplier, e.Tenant.Human.ID)
	ordered := e.paidRequest(t, e.Supplier, "10.00", "2026-03-12", `, "purchase_order_id": "`+po.String()+`"`)
	inv := e.invoice(t, e.Supplier, "FIN-1", "2380.00", "2026-04-01")
	path := "/v1/supplier-invoices/" + id(inv) + ":clearDownPayments"
	for name, c := range map[string]struct {
		body, code string
		status     int
	}{
		"more than the invoice": {clearing(paid, big), "payables.down_payments_exceed_invoice", 422},
		"another order":         {clearing(ordered), "payables.down_payment_other_order", 422},
		"not paid":              {`{"posting_date": "2026-04-02", "items": [{"fiscal_year": 2026, "journal_entry_id": "` + uuid.NewString() + `", "line_no": 1}]}`, "payables.not_a_down_payment_request", 422},
	} {
		t.Run(name, func(t *testing.T) { e.in(t).Problem(c.status, c.code, "POST", path, c.body, `"2"`) })
	}
	e.Problem(422, "ledger.duplicate_item", "POST", path, clearing(paid, paid), `"2"`)
	e.Must(412, "POST", path, clearing(paid), `"9"`)
	held := e.run(t, "2026-05-15", "2026-05-15")
	e.Problem(409, "payables.invoice_in_payment_run", "POST", path, clearing(paid), `"2"`)
	e.decide(t, 200, held, "reject", `{"reason": "Clear the down payment first"}`)
	e.Must(200, "POST", path, clearing(paid), `"2"`)
	cleared, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(paid), "", "")
	if cleared["status"] != "cleared" {
		t.Fatalf("request: got %v, want cleared", cleared["status"])
	}
	if cleared["cleared_invoice_id"] != id(inv) {
		t.Errorf("cleared against: got %v, want %s", cleared["cleared_invoice_id"], id(inv))
	}
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+cleared["clearing_entry_id"].(string), "", "")
	for i, want := range []string{"-1000.00", "-190.00", "1190.00"} {
		if got := je["lines"].([]any)[i].(map[string]any)["amount"].(map[string]any)["amount"]; got != want {
			t.Errorf("transfer line %d: got %v, want %s", i+1, got, want)
		}
	}
	if ev := apitest.Published(t, e.Tenant, outbox.Event{Context: "payables", Name: "down_payments_cleared"}); len(ev) != 1 {
		t.Errorf("DownPaymentsCleared: got %d events, want 1", len(ev))
	}
	e.Problem(409, "payables.down_payment_already_cleared", "POST", path, clearing(paid), `"2"`)
	e.Problem(409, "payables.down_payments_cleared", "POST", "/v1/supplier-invoices/"+id(inv)+":reverse", `{"reason": "r"}`, `"2"`)

	run := e.run(t, "2026-05-16", "2026-05-16")
	pays := byKind(e.payments(t, run))
	if got := pays[e.Supplier.String()+" items"]["amount"].(map[string]any)["amount"]; got != "1190.00" {
		t.Errorf("next run pays the invoice less the down payment: got %v, want 1190.00", got)
	}

	memo := e.document(t, "credit_memo", e.Supplier, "CM-9", "1.00", "2026-04-01")
	e.Problem(422, "payables.not_an_invoice", "POST", "/v1/supplier-invoices/"+id(memo)+":clearDownPayments", clearing(big), `"2"`)
}

// TestDownPaymentDiscountOnRemainder: after a taxed down payment is cleared
// against an invoice with a cash discount, the run takes the discount only
// on what remains to pay, and its tax share corrects only the tax of that
// remainder (T46d with T36d): 3% of 2380.00 on the invoice less 3% of
// 1190.00 on the transfer's debit, 35.70, of which 5.70 tax.
func TestDownPaymentDiscountOnRemainder(t *testing.T) {
	e := downPaymentSetupWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return date("2026-03-10").Add(12 * time.Hour) } })
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	paid := e.paidRequest(t, e.Supplier, "1190.00", "2026-03-02", `, "tax_code": "V1"`)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "FIN-1",
		"invoice_date": "2026-03-01", "gross_amount": {"amount": "2380.00", "currency": "EUR"}, "tax_amount": {"amount": "380.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Machine", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "2000.00", "currency": "EUR"},
		"tax_code": "V1"}]}`, e.DE, e.Supplier, e.expense), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":clearDownPayments", strings.Replace(clearing(paid), "2026-04-02", "2026-03-05", 1), `"2"`)
	run := e.release(t, e.run(t, "2026-03-10", "2026-03-20"), "")
	pay := byKind(e.payments(t, run))[e.Supplier.String()+" items"]
	if got := pay["amount"].(map[string]any)["amount"]; got != "1154.30" {
		t.Errorf("payment: got %v, want 1154.30 (1190.00 less 35.70)", got)
	}
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+pay["journal_entry_id"].(string), "", "")
	sums := map[string]money.Minor{}
	for _, l := range je["lines"].([]any) {
		m := l.(map[string]any)
		v, err := money.ParseAmount(m["amount"].(map[string]any)["amount"].(string), 2)
		if err != nil {
			t.Fatal(err)
		}
		sums[m["account_id"].(string)] += v
	}
	for account, want := range map[string]money.Minor{"140000": -570, "400000": -3000} {
		t.Run("account "+account, func(t *testing.T) {
			if got := sums[e.Account(t, "2000", account).String()]; got != want {
				t.Errorf("discount line: got %v, want %v", got, want)
			}
		})
	}
}
