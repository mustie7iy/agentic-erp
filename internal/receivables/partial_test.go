package receivables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// setupTolerance is setup with company 2000's payment tolerance (T75f):
// underpayments up to 0.05 EUR and 1% of the items applied are written off
// to 680000, overpayments up to 0.02 EUR and 0.5% kept on 480000.
func setupTolerance(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		outputCodes("")(doc)
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "680000", Name: "Payment differences", Type: config.Expense},
			config.Account{Number: "480000", Name: "Payment differences kept", Type: config.Revenue})
		de.AccountDetermination.PaymentDifferenceExpense, de.AccountDetermination.PaymentDifferenceRevenue = "680000", "480000"
		de.Receivables.PaymentTolerance = config.PaymentTolerance{UnderpaymentAmount: "0.05", UnderpaymentPercent: "1", OverpaymentAmount: "0.02",
			OverpaymentPercent: "0.5"}
	}, perms...)
	e := env{Env: f, revenue: f.Account(t, "2000", "400000"), other: f.Account(t, "2000", "530000"), receivable: f.Account(t, "2000", "120000"),
		tax: f.Account(t, "2000", "230000"), bank: f.Account(t, "2000", "102000"), incoming: f.Account(t, "2000", "103000")}
	e.customer = e.partner(t, "BP70000001", "Northwind Traders", "active", "customer")
	e.companyData(t, e.customer, "NT30")
	return e
}

// withResidual is an item reference with a residual of the amount in EUR
// for the reason, and the free text when it is not empty.
func withResidual(ref, amount, reason, text string) string {
	residual := fmt.Sprintf(`"residual": {"amount": {"amount": %q, "currency": "EUR"}, "reason": %q`, amount, reason)
	if text != "" {
		residual += fmt.Sprintf(`, "reason_text": %q`, text)
	}
	return strings.TrimSuffix(ref, "}") + ", " + residual + "}}"
}

// TestPartialPayment (T75f acceptance): a receipt that pays an invoice in
// part leaves a residual item for the rest, with its reason; the invoice
// is cleared, the residual is the customer's open item due when the
// invoice was, and the payment names it as a line of its clearing entry.
func TestPartialPayment(t *testing.T) {
	e := setupTolerance(t)
	invoice := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	receipt := e.receipt(t, 6000, "2026-03-20")
	got, _ := e.Must(201, "POST", "/v1/incoming-payments",
		e.payment(e.customer, receipt.ID, "2026-03-21", withResidual(invoice, "40.00", "disputed", "The second visit is disputed.")), "")
	if a, d := amount(t, got, "amount"), amount(t, got, "difference"); a != "60.00 EUR" || d != "0.00 EUR" {
		t.Errorf("amount and difference: got %s and %s, want 60.00 EUR and 0.00 EUR", a, d)
	}
	its := items(t, got)
	residual, ok := its[0]["residual"].(map[string]any)
	if !ok {
		t.Fatalf("item 1: got %v, want its residual", its[0])
	}
	for f, want := range map[string]any{"reason": "disputed", "reason_text": "The second visit is disputed.", "fiscal_year": float64(2026),
		"journal_entry_id": got["journal_entry_id"], "journal_entry_number": got["journal_entry_number"], "line_no": float64(3)} {
		if residual[f] != want {
			t.Errorf("residual %s: got %v, want %v", f, residual[f], want)
		}
	}
	if a := amount(t, residual, "amount"); a != "40.00 EUR" {
		t.Errorf("residual amount: got %s, want 40.00 EUR", a)
	}
	if got, want := show(e.entryLines(t, got["journal_entry_id"].(string))), "103000 6000 - false; 120000 -10000 - true; 120000 4000 - true; "; got != want {
		t.Errorf("clearing entry's lines: got %s, want %s", got, want)
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String(), "", "")
	open := items(t, page)
	if len(open) != 1 || open[0]["journal_entry_id"] != got["journal_entry_id"] || open[0]["line_no"] != float64(3) {
		t.Fatalf("the customer's open items: got %v, want the residual alone", open)
	}
	if terms, _ := open[0]["payment_terms"].(map[string]any); terms == nil || terms["net_due_date"] != "2026-04-09" {
		t.Errorf("the residual's terms: got %v, want the invoice's, due 2026-04-09", open[0]["payment_terms"])
	}
	read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	var residuals float64
	pgtest.Scan(t, `SELECT (payload->>'residual_count')::numeric FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2`,
		[]any{e.Tenant.ID, got["id"]}, &residuals)
	if residuals != 1 {
		t.Errorf("the event's residual count: got %v, want 1", residuals)
	}
}

// TestPaymentDifference (T75f acceptance): a few cents short of the
// invoice, within the company's tolerance, are written off and the
// invoice cleared in full; beyond the tolerance the payment is refused
// and nothing posts.
func TestPaymentDifference(t *testing.T) {
	e := setupTolerance(t)
	got, _ := e.Must(201, "POST", "/v1/incoming-payments",
		e.payment(e.customer, e.receipt(t, 9997, "2026-03-20").ID, "2026-03-21", e.invoiced(t, e.customer, "100.00", "2026-03-10")), "")
	if d := amount(t, got, "difference"); d != "0.03 EUR" {
		t.Errorf("difference: got %s, want 0.03 EUR", d)
	}
	if got, want := show(e.entryLines(t, got["journal_entry_id"].(string))), "103000 9997 - false; 120000 -10000 - true; 680000 3 - false; "; got != want {
		t.Errorf("clearing entry's lines: got %s, want %s", got, want)
	}
	before := e.payments(t)
	p := e.Problem(422, "ledger.clearing_not_balanced", "POST", "/v1/incoming-payments",
		e.payment(e.customer, e.receipt(t, 9990, "2026-03-20").ID, "2026-03-21", e.invoiced(t, e.customer, "100.00", "2026-03-10")), "")
	if !strings.Contains(fmt.Sprint(p["detail"]), "by 0.10: an underpayment beyond the company's payment tolerance") {
		t.Errorf("detail: got %v, want the difference beyond the tolerance", p["detail"])
	}
	if e.payments(t) != before {
		t.Error("a refused payment was written")
	}
}

// TestResidualRefusals (T75f): a residual that is not a positive amount in
// the receipt's currency with a known reason, or is not less than its
// invoice, is refused naming its field, and nothing posts.
func TestResidualRefusals(t *testing.T) {
	e := setupTolerance(t)
	receipt := e.receipt(t, 6000, "2026-03-20")
	invoice := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	for _, c := range []struct {
		name, item  string
		status      int
		code, field string
	}{
		{"an unknown reason", withResidual(invoice, "40.00", "bored", ""), 422, "receivables.unknown_residual_reason", "items[0].residual.reason"},
		{"another currency", strings.Replace(withResidual(invoice, "40.00", "disputed", ""), `"EUR"`, `"USD"`, 1), 422, "receivables.currency_mismatch",
			"items[0].residual.amount.currency"},
		{"zero", withResidual(invoice, "0.00", "disputed", ""), 422, "receivables.amount_not_positive", "items[0].residual.amount.amount"},
		{"three decimals", withResidual(invoice, "40.001", "disputed", ""), 400, "request.invalid_amount", "items[0].residual.amount.amount"},
		{"white space", withResidual(invoice, "40.00", "other", "  "), 422, "request.validation_failed", "items[0].residual.reason_text"},
		{"the whole invoice", withResidual(invoice, "100.00", "disputed", ""), 422, "ledger.invalid_residual", "items[0].residual.amount"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.Problem(c.status, c.code, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", c.item), "")
			if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != c.field {
				t.Errorf("field errors: got %v, want one naming %s", p["errors"], c.field)
			}
		})
	}
	if e.payments(t) != 0 {
		t.Error("a refused payment was written")
	}
}
