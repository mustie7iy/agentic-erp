package receivables_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// onAccount is the JSON that puts the receipt on the customer's account,
// applied to the items first.
func (e env) onAccount(customer uuid.UUID, receipt any, items ...string) string {
	body := e.payment(customer, receipt, "2026-03-21", items...)
	return body[:len(body)-1] + `, "on_account": true}`
}

// TestPaymentOnAccount (T75g acceptance): a receipt with no item goes on
// the customer's account as a credit; the payment shows it open; an
// application applies part of it to an invoice, leaving the rest on
// account, and the payment's version moves; a second application takes
// the rest and an invoice's residual; then nothing is open.
func TestPaymentOnAccount(t *testing.T) {
	e := setupTolerance(t)
	got, tag := e.Must(201, "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 10000, "2026-03-20").ID), "")
	id, entry := got["id"].(string), got["journal_entry_id"].(string)
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if got, want := show(e.entryLines(t, entry)), "103000 10000 - false; 120000 -10000 - true; "; got != want {
		t.Errorf("clearing entry's lines: got %s, want %s", got, want)
	}
	on, _ := got["on_account"].(map[string]any)
	if on == nil || amount(t, on, "amount") != "100.00 EUR" || amount(t, on, "open_amount") != "100.00 EUR" || on["journal_entry_id"] != entry ||
		on["line_no"] != float64(2) {
		t.Fatalf("on account: got %v, want 100.00 EUR open on line 2 of the clearing entry", got["on_account"])
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String(), "", "")
	if open := items(t, page); len(open) != 1 || open[0]["journal_entry_id"] != entry || amount(t, open[0], "amount") != "-100.00 EUR" {
		t.Errorf("the customer's open items: got %v, want the credit of 100.00", open)
	}
	invoice := e.invoiced(t, e.customer, "60.00", "2026-03-22")
	got, tag = e.Must(201, "POST", "/v1/incoming-payments/"+id+"/applications", fmt.Sprintf(`{"posting_date": "2026-03-23", "items": [%s]}`, invoice), "")
	if tag != `"2"` || got["version"] != float64(2) {
		t.Errorf("version: got %v (%s), want 2", got["version"], tag)
	}
	apps, _ := got["applications"].([]any)
	if len(apps) != 1 {
		t.Fatalf("applications: got %v, want one", got["applications"])
	}
	a := apps[0].(map[string]any)
	if amount(t, a, "amount") != "100.00 EUR" || amount(t, a, "on_account_amount") != "40.00 EUR" || a["application_no"] != float64(1) ||
		a["posting_date"] != "2026-03-23" || got["updated_at"] != a["created_at"] {
		t.Errorf("application: got %v, want 100.00 applied, 40.00 left, number 1 of 2026-03-23, the payment updated then", a)
	}
	if got, want := show(e.entryLines(t, a["journal_entry_id"])), "120000 10000 - true; 120000 -6000 - true; 120000 -4000 - true; "; got != want {
		t.Errorf("the application's lines: got %s, want %s", got, want)
	}
	its := items(t, got)
	if len(its) != 1 || its[0]["application_no"] != float64(1) || its[0]["item_no"] != float64(1) {
		t.Errorf("items: got %v, want the invoice as item 1 of application 1", its)
	}
	on = got["on_account"].(map[string]any)
	if amount(t, on, "open_amount") != "40.00 EUR" || on["journal_entry_id"] != a["journal_entry_id"] || on["line_no"] != float64(3) {
		t.Errorf("on account: got %v, want 40.00 open on line 3 of the application's entry", on)
	}
	e.creditEvent(t, id, 1, "40.00")
	second := withResidual(e.invoiced(t, e.customer, "50.00", "2026-03-22"), "10.00", "short_payment", "")
	got, _ = e.Must(201, "POST", "/v1/incoming-payments/"+id+"/applications", fmt.Sprintf(`{"posting_date": "2026-03-24", "items": [%s]}`, second), "")
	on = got["on_account"].(map[string]any)
	if amount(t, on, "open_amount") != "0.00 EUR" || on["line_no"] != nil {
		t.Errorf("on account: got %v, want nothing open", on)
	}
	its = items(t, got)
	if r, _ := its[1]["residual"].(map[string]any); r == nil || r["journal_entry_id"] != got["applications"].([]any)[1].(map[string]any)["journal_entry_id"] {
		t.Errorf("the second item's residual: got %v, want a line of the second application's entry", its[1]["residual"])
	}
	read, readTag := e.Must(200, "GET", "/v1/incoming-payments/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b || readTag != `"3"` {
		t.Errorf("read back: got %s (%s), want %s (\"3\")", a, readTag, b)
	}
	e.Problem(409, "receivables.no_credit_on_account", "POST", "/v1/incoming-payments/"+id+"/applications",
		fmt.Sprintf(`{"items": [%s]}`, e.invoiced(t, e.customer, "1.00", "2026-03-22")), "")
}

// creditEvent checks IncomingPaymentOnAccountApplied of the application.
func (e env) creditEvent(t *testing.T, payment string, no int, left string) {
	t.Helper()
	var n int
	var payload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.receivables.incoming_payment_on_account_applied.v1' AND (payload->>'application_no')::int = $3`,
		[]any{e.Tenant.ID, payment, no}, &n, &payload)
	if n != 1 {
		t.Fatalf("IncomingPaymentOnAccountApplied events: got %d, want 1", n)
	}
	if amount(t, payload, "on_account_amount") != left+" EUR" || payload["customer_number"] != "BP70000001" || payload["item_count"] != float64(1) {
		t.Errorf("event payload: got %v, want %s EUR left of customer BP70000001's 1 item", payload, left)
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'receivables.incoming_payment.apply_on_account' AND object_id = $2`,
		[]any{e.Tenant.ID, payment}, &audits)
	if audits < 1 {
		t.Error("the application has no audit record")
	}
}

// TestOnAccountRefusals (T75g): on account with nothing left, for a
// customer not set up in the company, and an application of a payment
// without a credit, of another tenant's payment or of a credit cleared
// otherwise are refused.
func TestOnAccountRefusals(t *testing.T) {
	e := setupTolerance(t)
	invoice := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	e.Problem(422, "receivables.nothing_left_on_account", "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 10000, "2026-03-20").ID, invoice), "")
	stranger := e.partner(t, "BP70000009", "Not set up", "active", "customer")
	e.Problem(422, "receivables.customer_not_usable", "POST", "/v1/incoming-payments", e.onAccount(stranger, e.receipt(t, 500, "2026-03-20").ID), "")
	e.Problem(422, "ledger.no_items", "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 500, "2026-03-20").ID, "2026-03-21"), "")
	exact, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 10000, "2026-03-20").ID, "2026-03-21", invoice), "")
	e.Problem(409, "receivables.no_credit_on_account", "POST", "/v1/incoming-payments/"+exact["id"].(string)+"/applications",
		fmt.Sprintf(`{"items": [%s]}`, e.invoiced(t, e.customer, "1.00", "2026-03-22")), "")
	e.Problem(404, "request.not_found", "POST", "/v1/incoming-payments/"+uuid.NewString()+"/applications",
		fmt.Sprintf(`{"items": [%s]}`, e.invoiced(t, e.customer, "1.00", "2026-03-22")), "")
	t.Run("a credit netted by another payment", func(t *testing.T) {
		got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 2000, "2026-03-20").ID), "")
		credit := itemRef(got["journal_entry_id"], 2)
		e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 1000, "2026-03-20").ID, "2026-03-21",
			e.invoiced(t, e.customer, "30.00", "2026-03-10"), credit), "")
		read, _ := e.Must(200, "GET", "/v1/incoming-payments/"+got["id"].(string), "", "")
		if on := read["on_account"].(map[string]any); amount(t, on, "open_amount") != "0.00 EUR" {
			t.Errorf("on account: got %v, want nothing open", on)
		}
		e.Problem(409, "receivables.no_credit_on_account", "POST", "/v1/incoming-payments/"+got["id"].(string)+"/applications",
			fmt.Sprintf(`{"items": [%s]}`, e.invoiced(t, e.customer, "1.00", "2026-03-22")), "")
	})
	t.Run("an application's residual refused", func(t *testing.T) {
		got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 2000, "2026-03-20").ID), "")
		e.Problem(422, "receivables.unknown_residual_reason", "POST", "/v1/incoming-payments/"+got["id"].(string)+"/applications",
			fmt.Sprintf(`{"items": [%s]}`, withResidual(e.invoiced(t, e.customer, "30.00", "2026-03-10"), "5.00", "bored", "")), "")
		e.Problem(422, "ledger.clearing_not_balanced", "POST", "/v1/incoming-payments/"+got["id"].(string)+"/applications",
			fmt.Sprintf(`{"items": [%s]}`, e.invoiced(t, e.customer, "30.00", "2026-03-10")), "")
	})
}

// TestApplyOnAccountConcurrently (T75g acceptance): two applications of
// one credit race; one applies it, the other is refused and writes
// nothing.
func TestApplyOnAccountConcurrently(t *testing.T) {
	e := setupTolerance(t)
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 10000, "2026-03-20").ID), "")
	id := uuid.MustParse(got["id"].(string))
	h := endpoints(t)
	refs := []string{e.invoiced(t, e.customer, "10.00", "2026-03-22"), e.invoiced(t, e.customer, "20.00", "2026-03-22")}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.ApplyOnAccount(ctx, receivables.CreditInput{PaymentID: id, Items: []ledger.ItemRef{{FiscalYear: 2026, EntryID: entryOf(t, refs[i]), LineNo: 1}}})
				return err
			})
		}()
	}
	wg.Wait()
	refused := 0
	for _, err := range errs {
		if err != nil {
			if c := problem.From(err).Code; c != "ledger.item_already_cleared" && c != "receivables.no_credit_on_account" {
				t.Errorf("the loser: got %v, want 409 item_already_cleared or no_credit_on_account", err)
			}
			refused++
		}
	}
	var apps int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.incoming_payment_application WHERE tenant_id = $1 AND incoming_payment_id = $2`, []any{e.Tenant.ID, id}, &apps)
	if refused != 1 || apps != 1 {
		t.Errorf("refused %d, applications %d: want one of each", refused, apps)
	}
}
