package receivables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// severally is the JSON that applies the receipt to the items of the
// customer, the payer, and of the other customers named.
func (e env) severally(customer uuid.UUID, others []uuid.UUID, receipt any, items ...string) string {
	ids := make([]string, len(others))
	for i, o := range others {
		ids[i] = fmt.Sprintf("%q", o)
	}
	body := e.payment(customer, receipt, "2026-03-21", items...)
	return body[:len(body)-1] + `, "other_customer_ids": [` + strings.Join(ids, ", ") + `]}`
}

// parts is the payment's customers with their parts, as "number amount".
func parts(t *testing.T, got map[string]any) string {
	t.Helper()
	raw, _ := got["customers"].([]any)
	out := make([]string, len(raw))
	for i, r := range raw {
		c, _ := r.(map[string]any)
		out[i] = fmt.Sprintf("%v %v %s", c["customer_number"], c["customer_name"], amount(t, c, "amount"))
	}
	return strings.Join(out, "; ")
}

// TestReceiptForSeveralCustomers (T75h acceptance): one receipt pays the
// payer's invoice and two invoices of a subsidiary, one of them in part:
// one DZ entry clears all three, the residual stays the subsidiary's open
// item, the payment shows each item's customer and each customer's part,
// which add up to the receipt; the event carries the parts; the list by
// either customer finds the payment, by a third none.
func TestReceiptForSeveralCustomers(t *testing.T) {
	e := setup(t)
	sub := e.partner(t, "BP70000003", "Northwind Logistics", "active", "customer")
	e.companyData(t, sub, "NT30")
	third := e.partner(t, "BP70000004", "Fabrikam", "active", "customer")
	receipt := e.receipt(t, 25000, "2026-03-20")
	own, subFirst, subSecond := e.invoiced(t, e.customer, "100.00", "2026-03-10"), e.invoiced(t, sub, "120.00", "2026-03-11"),
		e.invoiced(t, sub, "80.00", "2026-03-12")
	partPaid := strings.TrimSuffix(subSecond, "}") + `, "residual": {"amount": {"amount": "50.00", "currency": "EUR"}, "reason": "disputed"}}`
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.severally(e.customer, []uuid.UUID{sub}, receipt.ID, own, subFirst, partPaid), "")
	if p, want := parts(t, got), "BP70000001 Northwind Traders 100.00 EUR; BP70000003 Northwind Logistics 150.00 EUR"; p != want {
		t.Errorf("customers: got %s, want %s", p, want)
	}
	its := items(t, got)
	for i, want := range []string{e.customer.String() + " BP70000001 Northwind Traders", sub.String() + " BP70000003 Northwind Logistics",
		sub.String() + " BP70000003 Northwind Logistics"} {
		if g := fmt.Sprintf("%v %v %v", its[i]["customer_id"], its[i]["customer_number"], its[i]["customer_name"]); g != want {
			t.Errorf("item %d's customer: got %s, want %s", i+1, g, want)
		}
	}
	entry := got["journal_entry_id"].(string)
	if lines, want := show(e.entryLines(t, entry)), "103000 25000 - false; 120000 -10000 - true; 120000 -12000 - true; 120000 -8000 - true; 120000 5000 - true; "; lines != want {
		t.Errorf("clearing entry's lines: got %s, want %s", lines, want)
	}
	var residualPartner uuid.UUID
	pgtest.Scan(t, `SELECT partner_id FROM ledger.journal_line WHERE tenant_id = $1 AND journal_entry_id = $2 AND line_no = 5`, []any{e.Tenant.ID, entry}, &residualPartner)
	if residualPartner != sub {
		t.Errorf("the residual item's customer: got %s, want the subsidiary %s", residualPartner, sub)
	}
	for _, c := range []struct {
		name     string
		customer uuid.UUID
		want     int
	}{{"the payer", e.customer, 0}, {"the subsidiary", sub, 1}} {
		t.Run(c.name+"'s open items", func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/open-items?partner_id="+c.customer.String(), "", "")
			if n := len(items(t, page)); n != c.want {
				t.Errorf("got %d, want %d", n, c.want)
			}
		})
	}
	payload := e.outboxPayload(t, got["id"].(string), "incoming_payment_applied")
	want := fmt.Sprintf(`[{"amount":{"amount":"100.00","currency":"EUR"},"customer_id":%q,"customer_number":"BP70000001"},`+
		`{"amount":{"amount":"150.00","currency":"EUR"},"customer_id":%q,"customer_number":"BP70000003"}]`, e.customer, sub)
	if g := mustJSON(t, payload["customers"]); g != want {
		t.Errorf("event's customers: got %s, want %s", g, want)
	}
	for _, c := range []struct {
		name     string
		customer uuid.UUID
		want     int
	}{{"the payer", e.customer, 1}, {"the subsidiary", sub, 1}, {"a third customer", third, 0}} {
		t.Run("listed by "+c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/incoming-payments?customer_id="+c.customer.String(), "", "")
			if n := len(items(t, page)); n != c.want {
				t.Errorf("got %d payments, want %d", n, c.want)
			}
		})
	}
	read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
}

// TestSeveralCustomersOnAccount (T75h): a receipt paying only another
// customer's invoice leaves the rest on the payer's account; the payer is
// listed first with a part of zero.
func TestSeveralCustomersOnAccount(t *testing.T) {
	e := setup(t)
	sub := e.partner(t, "BP70000003", "Northwind Logistics", "active", "customer")
	e.companyData(t, sub, "NT30")
	invoice := e.invoiced(t, sub, "60.00", "2026-03-10")
	body := e.severally(e.customer, []uuid.UUID{sub}, e.receipt(t, 10000, "2026-03-20").ID, invoice)
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", body[:len(body)-1]+`, "on_account": true}`, "")
	if p, want := parts(t, got), "BP70000001 Northwind Traders 0.00 EUR; BP70000003 Northwind Logistics 60.00 EUR"; p != want {
		t.Errorf("customers: got %s, want %s", p, want)
	}
	var partner uuid.UUID
	pgtest.Scan(t, `SELECT partner_id FROM ledger.journal_line WHERE tenant_id = $1 AND journal_entry_id = $2 AND line_no = 3`,
		[]any{e.Tenant.ID, got["journal_entry_id"]}, &partner)
	on, _ := got["on_account"].(map[string]any)
	if on == nil {
		t.Fatal("on account: got nothing, want 40.00 EUR")
	}
	is(t, "on account", amount(t, on, "amount"), "40.00 EUR")
	is(t, "the credit's customer", partner, e.customer)
}

// TestSeveralCustomersRefusals (T75h acceptance): an item of a customer
// not named, an other customer unknown, the payer named again and one
// with no item are refused, naming the field, a customer named twice by
// the contract, and nothing posts.
func TestSeveralCustomersRefusals(t *testing.T) {
	e := setup(t)
	sub := e.partner(t, "BP70000003", "Northwind Logistics", "active", "customer")
	e.companyData(t, sub, "NT30")
	third := e.partner(t, "BP70000004", "Fabrikam", "active", "customer")
	e.companyData(t, third, "NT30")
	receipt := e.receipt(t, 10000, "2026-03-20").ID
	own, subs, thirds := e.invoiced(t, e.customer, "40.00", "2026-03-10"), e.invoiced(t, sub, "60.00", "2026-03-10"), e.invoiced(t, third, "60.00", "2026-03-10")
	for _, c := range []struct {
		name, body  string
		status      int
		code, field string
	}{
		{"an item of a customer not named", e.severally(e.customer, []uuid.UUID{sub}, receipt, own, thirds), 422, "ledger.not_the_customers_item", "items[1]"},
		{"an unknown customer", e.severally(e.customer, []uuid.UUID{sub, uuid.New()}, receipt, own, subs), 422, "request.reference_not_found", "other_customer_ids[1]"},
		{"the payer again", e.severally(e.customer, []uuid.UUID{e.customer}, receipt, own, subs), 422, "receivables.duplicate_customer", "other_customer_ids[0]"},
		{"a customer twice", e.severally(e.customer, []uuid.UUID{sub, sub}, receipt, own, subs), 400, "request.invalid_body", ""}, // uniqueItems
		{"a customer without items", e.severally(e.customer, []uuid.UUID{third, sub}, receipt, own, subs), 422, "receivables.customer_without_items", "other_customer_ids[0]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.entries(t)
			if f := field(e.In(t).Problem(c.status, c.code, "POST", "/v1/incoming-payments", c.body, "")); c.field != "" && f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
			if n, m := e.entries(t)-before, e.payments(t); n != 0 || m != 0 {
				t.Errorf("got %d journal entries more and %d payments, want none", n, m)
			}
		})
	}
}

// TestSeveralCustomersPartOutOfRange (T75h): items that are together the
// receipt but whose part of one customer is out of range are refused, and
// nothing posts.
func TestSeveralCustomersPartOutOfRange(t *testing.T) {
	e := setup(t)
	sub := e.partner(t, "BP70000003", "Northwind Logistics", "active", "customer")
	const half = 4_700_000_000_000_000_000
	first, second := e.credited(t, e.customer, -half, "2026-03-10"), e.credited(t, e.customer, -half, "2026-03-10") // two debits of the payer
	credit := e.credited(t, sub, 9_000_000_000_000_000_000, "2026-03-10")
	receipt := e.receipt(t, 2*half-9_000_000_000_000_000_000, "2026-03-20").ID
	before := e.entries(t)
	e.Problem(422, "receivables.amount_too_large", "POST", "/v1/incoming-payments", e.severally(e.customer, []uuid.UUID{sub}, receipt, first, second, credit), "")
	if n, m := e.entries(t)-before, e.payments(t); n != 0 || m != 0 {
		t.Errorf("got %d journal entries more and %d payments, want none", n, m)
	}
}
