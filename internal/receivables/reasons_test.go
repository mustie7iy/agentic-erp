package receivables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// setupReasons is setup with company 2000's payment differences (T75o):
// underpayments up to 5.00 EUR and 5% within the tolerance, written off
// to 680000, and three reasons: short_payment and disputed leave a
// residual item, bank_charges writes the difference off to 690000.
func setupReasons(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		outputCodes("")(doc)
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "680000", Name: "Payment differences", Type: config.Expense},
			config.Account{Number: "690000", Name: "Bank charges borne", Type: config.Expense})
		de.AccountDetermination.PaymentDifferenceExpense = "680000"
		de.Receivables.PaymentTolerance = config.PaymentTolerance{UnderpaymentAmount: "5.00", UnderpaymentPercent: "5"}
		de.Receivables.PaymentDifferenceReasons = []config.DifferenceReason{
			{Code: "short_payment", Description: "Paid less", Treatment: config.TreatmentResidual},
			{Code: "disputed", Description: "Disputed", Treatment: config.TreatmentResidual},
			{Code: "bank_charges", Description: "The customer's bank charged", Treatment: config.TreatmentWriteOff, Account: "690000"}}
	}, perms...)
	e := env{Env: f, revenue: f.Account(t, "2000", "400000"), other: f.Account(t, "2000", "530000"), receivable: f.Account(t, "2000", "120000"),
		tax: f.Account(t, "2000", "230000"), bank: f.Account(t, "2000", "102000"), incoming: f.Account(t, "2000", "103000")}
	e.customer = e.partner(t, "BP70000001", "Northwind Traders", "active", "customer")
	e.companyData(t, e.customer, "NT30")
	return e
}

// TestReasonWritesOff (T75o acceptance): a residual whose reason writes
// off is a line on the reason's account, the invoice cleared and nothing
// left open; the payment shows it as the item's write-off with its
// account, and the customer's part net of it; a residual reason still
// leaves a residual item; it reads back the same and the audit record
// counts the write-off.
func TestReasonWritesOff(t *testing.T) {
	e := setupReasons(t)
	invoice, second := e.invoiced(t, e.customer, "100.00", "2026-03-10"), e.invoiced(t, e.customer, "50.00", "2026-03-10")
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 12600, "2026-03-20").ID, "2026-03-21",
		withResidual(invoice, "4.00", "bank_charges", "Charged by Northwind's bank"), withResidual(second, "20.00", "disputed", "")), "")
	its := items(t, got)
	off, _ := its[0]["write_off"].(map[string]any)
	if off == nil || its[0]["residual"] != nil {
		t.Fatalf("item 1: got %v, want its write-off and no residual", its[0])
	}
	for f, want := range map[string]any{"reason": "bank_charges", "reason_text": "Charged by Northwind's bank", "account_id": e.Account(t, "2000", "690000").String(),
		"account_number": "690000", "account_name": "Bank charges borne", "journal_entry_id": got["journal_entry_id"],
		"journal_entry_number": got["journal_entry_number"], "fiscal_year": float64(2026), "line_no": float64(4)} {
		if off[f] != want {
			t.Errorf("write-off %s: got %v, want %v", f, off[f], want)
		}
	}
	if a := amount(t, off, "amount"); a != "4.00 EUR" {
		t.Errorf("write-off amount: got %s, want 4.00 EUR", a)
	}
	if residual, _ := its[1]["residual"].(map[string]any); residual == nil || its[1]["write_off"] != nil || residual["reason"] != "disputed" {
		t.Errorf("item 2: got %v, want its residual item and no write-off", its[1])
	}
	if got, want := show(e.entryLines(t, got["journal_entry_id"].(string))),
		"103000 12600 - false; 120000 -10000 - true; 120000 -5000 - true; 690000 400 - false; 120000 2000 - true; "; got != want {
		t.Errorf("clearing entry's lines: got %s, want %s", got, want)
	}
	customers, _ := got["customers"].([]any)
	if len(customers) != 1 || amount(t, customers[0].(map[string]any), "amount") != "126.00 EUR" || amount(t, got, "difference") != "0.00 EUR" {
		t.Errorf("parts and difference: got %v and %s, want the customer's 126.00 EUR and no difference", customers, amount(t, got, "difference"))
	}
	read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String(), "", "")
	if open := items(t, page); len(open) != 1 || amount(t, open[0], "amount") != "20.00 EUR" {
		t.Errorf("the customer's open items: got %v, want the disputed residual alone", open)
	}
	var writeOffs float64
	pgtest.Scan(t, `SELECT (detail->>'write_offs')::numeric FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2
		AND action = 'receivables.incoming_payment.apply'`, []any{e.Tenant.ID, got["id"]}, &writeOffs)
	if writeOffs != 1 {
		t.Errorf("the audit record's write-offs: got %v, want 1", writeOffs)
	}
}

// TestReasonRefusals (T75o acceptance): a reason the company does not
// have, one of the four it had before configuring its own among them, is
// refused listing the company's; a write-off whose account the company's
// accounts no longer have is refused; a write-off beyond the tolerance,
// alone or with the payment difference, is refused; each names its field,
// and nothing posts.
func TestReasonRefusals(t *testing.T) {
	e := setupReasons(t)
	receipt := e.receipt(t, 9000, "2026-03-20")
	invoice := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	for _, c := range []struct {
		name, item, code, field, detail string
	}{
		{"a reason the company does not have", withResidual(invoice, "10.00", "deduction", ""), "receivables.unknown_residual_reason",
			"items[0].residual.reason", "one of the company's payment-difference reasons: short_payment, disputed, bank_charges"},
		{"a write-off beyond the tolerance", withResidual(invoice, "10.00", "bank_charges", ""), "ledger.clearing_not_balanced", "items",
			"by 10.00: an underpayment beyond"},
		{"a write-off and a difference beyond it together", withResidual(invoice, "4.00", "bank_charges", ""), "ledger.clearing_not_balanced", "items",
			"by 10.00: an underpayment beyond"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(422, c.code, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", c.item), "")
			if field(p) != c.field || !strings.Contains(fmt.Sprint(p["detail"]), c.detail) {
				t.Errorf("got %v naming %v, want %q naming %s", p["detail"], field(p), c.detail, c.field)
			}
		})
	}
	t.Run("a write-off account the company no longer has", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM ledger.company_account WHERE tenant_id = $1 AND account_id = $2`, e.Tenant.ID, e.Account(t, "2000", "690000"))
		p := e.In(t).Problem(422, "receivables.no_write_off_account", "POST", "/v1/incoming-payments",
			e.payment(e.customer, receipt.ID, "2026-03-21", withResidual(invoice, "4.00", "bank_charges", "")), "")
		if field(p) != "items[0].residual.reason" {
			t.Errorf("field: got %v, want items[0].residual.reason", field(p))
		}
	})
	if e.payments(t) != 0 {
		t.Error("a refused payment was written")
	}
}
