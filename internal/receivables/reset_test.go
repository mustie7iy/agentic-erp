package receivables_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

const because = `{"reason": "Applied to the wrong invoice."}`

// openRefs are the customer's open items as "entry line amount", sorted.
func (e env) openRefs(t *testing.T, customer uuid.UUID) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+customer.String(), "", "")
	var out []string
	for _, it := range items(t, page) {
		out = append(out, fmt.Sprintf("%v %v %s", it["journal_entry_id"], it["line_no"], amount(t, it, "amount")))
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// lineOf is the open item's reference as openRefs shows it.
func lineOf(t *testing.T, ref, amount string) string {
	t.Helper()
	return fmt.Sprintf("%s 1 %s EUR", entryOf(t, ref), amount)
}

// TestResetPayment (T75i acceptance): a receipt applied to the wrong
// invoice is undone: the invoice is open again, the receipt waits to be
// applied again, the clearing entry is reversed on its own date and the
// clearing shows its reset; the payment's status, version and reset say
// so; the audit records and events are written. The receipt is then
// applied to the right invoice, and a second reset is refused.
func TestResetPayment(t *testing.T) {
	e := setup(t)
	receipt := e.receipt(t, 10000, "2026-03-20")
	wrong, right := e.invoiced(t, e.customer, "100.00", "2026-03-10"), e.invoiced(t, e.customer, "100.00", "2026-03-11")
	got, tag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", wrong), "")
	id, entry := got["id"].(string), got["journal_entry_id"].(string)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/incoming-payments/"+id+":reset", because, `"7"`)
	e.Problem(422, "request.validation_failed", "POST", "/v1/incoming-payments/"+id+":reset", `{"reason": "  "}`, tag)
	reset, resetTag := e.Must(200, "POST", "/v1/incoming-payments/"+id+":reset", because, tag)
	if reset["status"] != "reset" || reset["version"] != float64(2) || resetTag != `"2"` {
		t.Errorf("status and version: got %v, %v (%s), want reset, 2", reset["status"], reset["version"], resetTag)
	}
	r, _ := reset["reset"].(map[string]any)
	if r == nil || r["reason"] != "Applied to the wrong invoice." || r["journal_entry_number"] == "" || reset["updated_at"] != r["created_at"] {
		t.Fatalf("reset: got %v, want the reason and the reversal, the payment updated then", reset["reset"])
	}
	var reversed, date string
	pgtest.Scan(t, `SELECT r.reversal_entry_id::text, e.posting_date::text FROM ledger.reversal r JOIN ledger.journal_entry e ON e.id = r.reversal_entry_id
		WHERE r.tenant_id = $1 AND r.original_entry_id = $2`, []any{e.Tenant.ID, entry}, &reversed, &date)
	if reversed != r["journal_entry_id"] || date != "2026-03-21" {
		t.Errorf("the reversal: got %s on %s, want %v on the clearing entry's date 2026-03-21", reversed, date, r["journal_entry_id"])
	}
	both := []string{lineOf(t, wrong, "100.00"), lineOf(t, right, "100.00")}
	slices.Sort(both)
	if open, want := e.openRefs(t, e.customer), strings.Join(both, ", "); open != want {
		t.Errorf("the customer's open items: got %s, want both invoices", open)
	}
	if left, _ := e.receipts(t, ""); len(left) != 1 || left[0]["journal_entry_id"] != receipt.ID.String() {
		t.Errorf("receipts not applied: got %v, want the receipt again", left)
	}
	var clearing string
	pgtest.Scan(t, `SELECT clearing_id::text FROM ledger.clearing_reset r JOIN ledger.clearing c ON c.id = r.clearing_id WHERE r.tenant_id = $1 AND c.clearing_entry_id = $2`,
		[]any{e.Tenant.ID, entry}, &clearing)
	shown, _ := e.Must(200, "GET", "/v1/clearings/"+clearing, "", "")
	if its := items(t, shown); shown["reset_at"] == nil || len(its) != 2 {
		t.Errorf("the clearing: got reset at %v with %d lines, want its reset and the receipt and invoice it had cleared", shown["reset_at"], len(its))
	}
	e.resetRecorded(t, id, entry, reversed)
	read, readTag := e.Must(200, "GET", "/v1/incoming-payments/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, reset); a != b || readTag != resetTag {
		t.Errorf("read back: got %s (%s), want %s (%s)", a, readTag, b, resetTag)
	}
	again, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", right), "")
	if open, want := e.openRefs(t, e.customer), lineOf(t, wrong, "100.00"); open != want || again["status"] != "applied" {
		t.Errorf("applied again: got open %s, status %v; want the wrong invoice open, applied", open, again["status"])
	}
	e.Problem(409, "receivables.already_reset", "POST", "/v1/incoming-payments/"+id+":reset", because, resetTag)
	e.Problem(409, "receivables.receipt_already_applied", "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", wrong), "")
}

// resetRecorded checks the audit records and the events of a reset.
func (e env) resetRecorded(t *testing.T, id, entry, reversal string) {
	t.Helper()
	for _, action := range []string{"receivables.incoming_payment.reset", "ledger.clearing.reset"} {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = $2`, []any{e.Tenant.ID, action}, &n)
		if n != 1 {
			t.Errorf("audit records %s: got %d, want 1", action, n)
		}
	}
	payload := e.outboxPayload(t, id, "incoming_payment_reset")
	fields(t, "IncomingPaymentReset", payload, map[string]any{"incoming_payment_id": id, "customer_number": "BP70000001", "application_no": 0,
		"amount": eur("100.00"), "item_count": 1, "clearing_entry_id": entry, "reversal_entry_id": reversal, "reversal_posting_date": "2026-03-21"})
	if _, ok := payload["reason"]; ok {
		t.Errorf("IncomingPaymentReset: got the reason %v, want none (free text)", payload["reason"])
	}
	var n int
	var ledgerPayload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM ledger.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.ledger.clearing_reset.v1'`,
		[]any{e.Tenant.ID}, &n, &ledgerPayload)
	if n != 1 || ledgerPayload["clearing_entry_id"] != entry || ledgerPayload["reversal_entry_id"] != reversal {
		t.Errorf("ClearingReset: got %d, %v; want one for the clearing entry and its reversal", n, ledgerPayload)
	}
	if its, _ := ledgerPayload["items"].([]any); len(its) != 2 {
		t.Errorf("ClearingReset's items: got %v, want the receipt and the invoice", ledgerPayload["items"])
	}
}

// TestResetResidualClearedSince (T75i acceptance): a payment whose
// residual item another payment cleared is not undone until that one is;
// then the invoice is open in full again and the residual is no open item.
func TestResetResidualClearedSince(t *testing.T) {
	e := setup(t)
	invoice := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	partPaid := strings.TrimSuffix(invoice, "}") + `, "residual": {"amount": {"amount": "40.00", "currency": "EUR"}, "reason": "disputed"}}`
	first, tag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 6000, "2026-03-20").ID, "2026-03-21", partPaid), "")
	residual := itemRef(first["journal_entry_id"], 3)
	second, secondTag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 4000, "2026-03-22").ID, "2026-03-22", residual), "")
	id := first["id"].(string)
	before := e.entries(t)
	e.Problem(409, "ledger.clearing_lines_cleared", "POST", "/v1/incoming-payments/"+id+":reset", because, tag)
	if n := e.entries(t) - before; n != 0 {
		t.Errorf("journal entries after the refusal: got %d more, want none", n)
	}
	e.Must(200, "POST", "/v1/incoming-payments/"+second["id"].(string)+":reset", because, secondTag)
	if open, want := e.openRefs(t, e.customer), fmt.Sprintf("%v 3 40.00 EUR", first["journal_entry_id"]); open != want {
		t.Errorf("after the second is undone: got %s, want the residual open again", open)
	}
	e.Must(200, "POST", "/v1/incoming-payments/"+id+":reset", because, tag)
	if open, want := e.openRefs(t, e.customer), lineOf(t, invoice, "100.00"); open != want {
		t.Errorf("after both are undone: got %s, want the invoice open in full", open)
	}
}

// TestResetApplication (T75i acceptance): applications of a credit on
// account are undone last first, each opening the credit it consumed
// again as it stood; the payment is undone only once none is in force.
func TestResetApplication(t *testing.T) {
	e := setup(t)
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 10000, "2026-03-20").ID), "")
	id := got["id"].(string)
	first, second := e.invoiced(t, e.customer, "60.00", "2026-03-22"), e.invoiced(t, e.customer, "40.00", "2026-03-22")
	apply := func(ref string) string {
		_, tag := e.Must(201, "POST", "/v1/incoming-payments/"+id+"/applications", fmt.Sprintf(`{"posting_date": "2026-03-23", "items": [%s]}`, ref), "")
		return tag
	}
	apply(first)
	tag := apply(second)
	base := "/v1/incoming-payments/" + id
	for _, c := range []struct {
		name, path, code string
		status           int
	}{
		{"the payment while applications are in force", base + ":reset", "receivables.applications_in_force", 409},
		{"an application not the latest", base + "/applications/1:reset", "receivables.application_not_latest", 409},
		{"an application the payment has not", base + "/applications/3:reset", "request.not_found", 404},
	} {
		t.Run(c.name, func(t *testing.T) { e.In(t).Problem(c.status, c.code, "POST", c.path, because, tag) })
	}
	got, tag = e.Must(200, "POST", base+"/applications/2:reset", because, tag)
	on, _ := got["on_account"].(map[string]any)
	apps, _ := got["applications"].([]any)
	if amount(t, on, "open_amount") != "40.00 EUR" || got["version"] != float64(4) || apps[1].(map[string]any)["reset"] == nil {
		t.Errorf("application 2 undone: got open %v, version %v, applications %v; want 40.00 open again, version 4, application 2 reset", on, got["version"], apps)
	}
	want := []string{fmt.Sprintf("%v 3 -40.00 EUR", apps[0].(map[string]any)["journal_entry_id"]), lineOf(t, second, "40.00")}
	slices.Sort(want)
	if open := e.openRefs(t, e.customer); open != strings.Join(want, ", ") {
		t.Errorf("open items: got %s, want %v", open, want)
	}
	e.Problem(409, "receivables.already_reset", "POST", base+"/applications/2:reset", because, tag)
	got, tag = e.Must(200, "POST", base+"/applications/1:reset", because, tag)
	if on, _ := got["on_account"].(map[string]any); amount(t, on, "open_amount") != "100.00 EUR" || on["journal_entry_id"] != got["journal_entry_id"] {
		t.Errorf("application 1 undone: got %v, want the payment's own credit of 100.00 open", on)
	}
	got, _ = e.Must(201, "POST", base+"/applications", fmt.Sprintf(`{"posting_date": "2026-03-24", "items": [%s]}`, second), "")
	if apps, _ := got["applications"].([]any); len(apps) != 3 || got["version"] != float64(6) {
		t.Errorf("applied again: got %d applications, version %v; want 3, version 6", len(apps), got["version"])
	}
}

// TestResetClosedPeriod (T75i acceptance): a payment whose clearing
// entry's period is closed since is not undone, and nothing changes.
func TestResetClosedPeriod(t *testing.T) {
	e := setup(t)
	invoice := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	got, tag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 10000, "2026-03-20").ID, "2026-03-21", invoice), "")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 3`,
		e.Tenant.ID)
	before := e.entries(t)
	e.Problem(409, "ledger.period_not_open", "POST", "/v1/incoming-payments/"+got["id"].(string)+":reset", because, tag)
	var resets int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM ledger.clearing_reset WHERE tenant_id = $1) + (SELECT count(*) FROM receivables.incoming_payment_reset WHERE tenant_id = $1)`,
		[]any{e.Tenant.ID}, &resets)
	if n := e.entries(t) - before; n != 0 || resets != 0 || e.openRefs(t, e.customer) != "" {
		t.Errorf("after the refusal: got %d entries more, %d resets, open %q; want nothing changed", n, resets, e.openRefs(t, e.customer))
	}
}

// TestResetConcurrently (T75i acceptance): two resets of one payment race;
// one undoes it, the other is refused and writes nothing.
func TestResetConcurrently(t *testing.T) {
	e := setup(t)
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, e.receipt(t, 10000, "2026-03-20").ID, "2026-03-21",
		e.invoiced(t, e.customer, "100.00", "2026-03-10")), "")
	id := uuid.MustParse(got["id"].(string))
	h := endpoints(t)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.Reset(ctx, receivables.ResetInput{PaymentID: id, IfMatch: etag.IfMatch(`"1"`), Reason: "Twice at once."})
				return err
			})
		}()
	}
	wg.Wait()
	refused := 0
	for _, err := range errs {
		if err != nil {
			if c := problem.From(err).Code; c != "ledger.clearing_already_reset" && c != "receivables.already_reset" {
				t.Errorf("the loser: got %v, want 409 clearing_already_reset or already_reset", err)
			}
			refused++
		}
	}
	var resets, reversals int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM receivables.incoming_payment_reset WHERE tenant_id = $1),
		(SELECT count(*) FROM ledger.reversal WHERE tenant_id = $1 AND original_entry_id = $2)`, []any{e.Tenant.ID, got["journal_entry_id"]}, &resets, &reversals)
	if refused != 1 || resets != 1 || reversals != 1 {
		t.Errorf("refused %d, resets %d, reversals %d: want one of each", refused, resets, reversals)
	}
}

// TestResetOnceMapping (T75i): a second reset that reaches the database is
// answered as already reset.
func TestResetOnceMapping(t *testing.T) {
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "incoming_payment_reset_once"}); p.Status != 409 || p.Code != "receivables.already_reset" {
		t.Errorf("incoming_payment_reset_once: got %v, want 409 receivables.already_reset", p)
	}
}
