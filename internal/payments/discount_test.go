package payments_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// discountTerms gives the suppliers term ZB01 (3% within 14 days, 2%
// within 30, net 45) and company 2000 a cash discount account (400000).
func (e env) discountTerms(t *testing.T, suppliers ...uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, discount2_days, discount2_pct,
		created_by, updated_by) VALUES ($1, 'ZB01', '3% 14, 2% 30, net 45', 45, 14, 3, 30, 2, $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	for _, s := range suppliers {
		pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_term_code = 'ZB01', version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`, e.Tenant.ID, s)
	}
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'cash_discount', $3, $4)`,
		e.Tenant.ID, e.DE, e.Account(t, "2000", "400000"), e.Tenant.Human.ID)
}

// memo posts a credit memo of the supplier naming the invoice ("" for
// none), on the expense account.
func (e env) memo(t *testing.T, supplier uuid.UUID, ref, gross, date, invoice string) map[string]any {
	t.Helper()
	reference := ""
	if invoice != "" {
		reference = fmt.Sprintf(`"reference_invoice_id": %q,`, invoice)
	}
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "kind": "credit_memo", %s "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": "0.00", "currency": "EUR"},
		"items": [{"account_id": %q, "description": "Fees", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}]}`,
		e.DE, supplier, ref, date, reference, gross, e.expense, gross), "")
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", fmt.Sprintf(`{"posting_date": %q}`, date), `"1"`)
	return posted
}

// paid is a run's payment of the supplier as "amount discount", and its
// items' discounts, or "none".
func (e env) paid(t *testing.T, run map[string]any, supplier uuid.UUID) string {
	t.Helper()
	for _, p := range e.payments(t, run) {
		pay := p.(map[string]any)
		if pay["supplier_id"] != supplier.String() {
			continue
		}
		var items []string
		for _, it := range pay["items"].([]any) {
			item := it.(map[string]any)
			items = append(items, fmt.Sprint(item["amount"].(map[string]any)["amount"], "-", item["discount_amount"].(map[string]any)["amount"]))
		}
		return fmt.Sprint(pay["amount"].(map[string]any)["amount"], " ", pay["discount_amount"].(map[string]any)["amount"], " ", items)
	}
	return "none"
}

// TestCashDiscountInProposals: a run takes the cash discount of the tier
// its run date is in, by the terms each invoice was posted with (T36c),
// and pays an item that is not yet due when waiting until its due-by date
// would lose discount (SAP F110); after the last tier it pays the invoice
// in full when due. A credit memo naming the invoice takes the invoice's
// discount off its credit; one naming none takes none.
func TestCashDiscountInProposals(t *testing.T) {
	e := setup(t)
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	invoice := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01") // net due 15 April; 3% to 15 March, 2% to 31 March
	for _, c := range []struct {
		name, runDate, dueBy, want string
	}{
		{"within the first tier, pulled forward", "2026-03-10", "2026-03-20", "97.00 3.00 [100.00-3.00]"},
		{"within the first tier, the discount kept until the due-by date", "2026-03-10", "2026-03-12", "none"},
		{"within the second tier, pulled forward", "2026-03-20", "2026-04-05", "98.00 2.00 [100.00-2.00]"},
		{"after the tiers, when due", "2026-04-15", "2026-04-15", "100.00 0.00 [100.00-0.00]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			run := e.run(t, c.runDate, c.dueBy)
			if got := e.paid(t, run, e.Supplier); got != c.want {
				t.Errorf("payment: got %s, want %s", got, c.want)
			}
			if c.want != "none" {
				e.decide(t, 200, run, "reject", `{"reason": "test"}`)
			}
		})
	}
	e.memo(t, e.Supplier, "CM-1", "30.00", "2026-03-05", id(invoice))
	run := e.run(t, "2026-03-10", "2026-03-20")
	if got, want := e.paid(t, run, e.Supplier), "67.90 2.10 [100.00-3.00 -30.00--0.90]"; got != want {
		t.Errorf("payment with a credit memo naming the invoice: got %s, want %s", got, want)
	}
	proposed := events(t, e.Tenant, "payment_run_proposed")
	if got := proposed[len(proposed)-1]["discount_amount"].(map[string]any)["amount"]; got != "2.10" {
		t.Errorf("PaymentRunProposed discount: got %v, want 2.10", got)
	}
	e.decide(t, 200, run, "reject", `{"reason": "test"}`)
	e.memo(t, e.Supplier, "CM-2", "10.00", "2026-03-05", "")
	run = e.run(t, "2026-03-10", "2026-03-20")
	if got, want := e.paid(t, run, e.Supplier), "57.90 2.10 [100.00-3.00 -30.00--0.90 -10.00-0.00]"; got != want {
		t.Errorf("payment with a credit memo naming no invoice too: got %s, want %s", got, want)
	}
}

// TestDiscountedPaymentPosts: the release posts a discounted payment with
// the bank credited with what is paid and the discount on the cash
// discount account, and the bank file pays exactly that.
func TestDiscountedPaymentPosts(t *testing.T) {
	e := setup(t)
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.release(t, e.run(t, "2026-03-10", "2026-03-20"), "")
	pay := e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", pay["journal_entry_id"]), "", "")
	got := map[string]string{}
	for _, l := range je["lines"].([]any) {
		line := l.(map[string]any)
		got[line["account_id"].(string)] = line["amount"].(map[string]any)["amount"].(string)
	}
	want := map[string]string{e.Account(t, "2000", "211000").String(): "100.00", e.Account(t, "2000", "100000").String(): "-97.00",
		e.Account(t, "2000", "400000").String(): "-3.00"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("payment entry: got %v, want %v", got, want)
	}
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	e.Must(202, "POST", "/v1/bank-files/"+id(file)+":transmit", "", `"1"`)
	if xml := string(e.connector.files[0].Content); !strings.Contains(xml, `<InstdAmt Ccy="EUR">97.00</InstdAmt>`) {
		t.Errorf("bank file: got\n%s\nwant it to pay 97.00", xml)
	}
}

// TestDebitBalanceAfterDiscount: a supplier whose items net to nothing only
// after their discount is excluded with its debit balance, the net amount.
func TestDebitBalanceAfterDiscount(t *testing.T) {
	e := setup(t)
	e.discountTerms(t, e.Supplier)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.memo(t, e.Supplier, "CM-1", "97.00", "2026-03-05", "")
	run := e.run(t, "2026-03-10", "2026-03-20")
	if got := e.paid(t, run, e.Supplier); got != "none" {
		t.Errorf("payment: got %s, want none", got)
	}
	if got := e.exclusions(t, run)[e.Supplier.String()]; got != "debit_balance 0.00" {
		t.Errorf("exclusion: got %q, want debit_balance 0.00 (100.00 less its 3.00 discount less the memo's 97.00)", got)
	}
}
