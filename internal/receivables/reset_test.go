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
	"github.com/mustie7iy/agentic-erp/internal/ledger"
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

// is checks one value: got is want, or one message naming what.
func is(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
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
	is(t, "status", reset["status"], "reset")
	is(t, "version", reset["version"], 2)
	is(t, "ETag", resetTag, `"2"`)
	r, _ := reset["reset"].(map[string]any)
	if r == nil {
		t.Fatalf("reset: got none, want the undoing")
	}
	is(t, "reset's reason", r["reason"], "Applied to the wrong invoice.")
	is(t, "the payment's updated_at", reset["updated_at"], r["created_at"])
	var reversed, date string
	pgtest.Scan(t, `SELECT r.reversal_entry_id::text, e.posting_date::text FROM ledger.reversal r JOIN ledger.journal_entry e ON e.id = r.reversal_entry_id
		WHERE r.tenant_id = $1 AND r.original_entry_id = $2`, []any{e.Tenant.ID, entry}, &reversed, &date)
	is(t, "the reversal", reversed, r["journal_entry_id"])
	is(t, "the reversal's date, the clearing entry's", date, "2026-03-21")
	var number string
	pgtest.Scan(t, `SELECT document_number FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, reversed}, &number)
	is(t, "the reversal's number", r["journal_entry_number"], number)
	both := []string{lineOf(t, wrong, "100.00"), lineOf(t, right, "100.00")}
	slices.Sort(both)
	if open, want := e.openRefs(t, e.customer), strings.Join(both, ", "); open != want {
		t.Errorf("the customer's open items: got %s, want both invoices", open)
	}
	left, _ := e.receipts(t, "")
	is(t, "receipts not applied", len(left), 1)
	if len(left) == 1 {
		is(t, "the receipt not applied", left[0]["journal_entry_id"], receipt.ID)
	}
	var clearing string
	pgtest.Scan(t, `SELECT clearing_id::text FROM ledger.clearing_reset r JOIN ledger.clearing c ON c.id = r.clearing_id WHERE r.tenant_id = $1 AND c.clearing_entry_id = $2`,
		[]any{e.Tenant.ID, entry}, &clearing)
	shown, _ := e.Must(200, "GET", "/v1/clearings/"+clearing, "", "")
	if shown["reset_at"] == nil {
		t.Error("the clearing's reset_at: got none, want when it was reset")
	}
	is(t, "the clearing's lines, the receipt and the invoice it had cleared", len(items(t, shown)), 2)
	e.resetRecorded(t, id, entry, reversed)
	read, readTag := e.Must(200, "GET", "/v1/incoming-payments/"+id, "", "")
	is(t, "read back", mustJSON(t, read), mustJSON(t, reset))
	is(t, "read back's ETag", readTag, resetTag)
	again, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", right), "")
	is(t, "applied again: the open items", e.openRefs(t, e.customer), lineOf(t, wrong, "100.00"))
	is(t, "applied again: its status", again["status"], "applied")
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
	if cs, _ := payload["customers"].([]any); len(cs) != 1 || cs[0].(map[string]any)["customer_number"] != "BP70000001" {
		t.Errorf("IncomingPaymentReset's customers: got %v, want the payer's part", payload["customers"])
	}
	if _, ok := payload["reason"]; ok {
		t.Errorf("IncomingPaymentReset: got the reason %v, want none (free text)", payload["reason"])
	}
	var n int
	var ledgerPayload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM ledger.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.ledger.clearing_reset.v1'`,
		[]any{e.Tenant.ID}, &n, &ledgerPayload)
	is(t, "ClearingReset events", n, 1)
	is(t, "ClearingReset's clearing entry", ledgerPayload["clearing_entry_id"], entry)
	is(t, "ClearingReset's reversal", ledgerPayload["reversal_entry_id"], reversal)
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
	is(t, "application 2 undone: open on account", amount(t, on, "open_amount"), "40.00 EUR")
	is(t, "application 2 undone: version", got["version"], 4)
	if apps[1].(map[string]any)["reset"] == nil {
		t.Error("application 2's reset: got none, want the undoing")
	}
	want := []string{fmt.Sprintf("%v 3 -40.00 EUR", apps[0].(map[string]any)["journal_entry_id"]), lineOf(t, second, "40.00")}
	slices.Sort(want)
	if open := e.openRefs(t, e.customer); open != strings.Join(want, ", ") {
		t.Errorf("open items: got %s, want %v", open, want)
	}
	e.Problem(409, "receivables.already_reset", "POST", base+"/applications/2:reset", because, tag)
	got, tag = e.Must(200, "POST", base+"/applications/1:reset", because, tag)
	on, _ = got["on_account"].(map[string]any)
	is(t, "application 1 undone: open on account", amount(t, on, "open_amount"), "100.00 EUR")
	is(t, "application 1 undone: the credit, the payment's own", on["journal_entry_id"], got["journal_entry_id"])
	got, _ = e.Must(201, "POST", base+"/applications", fmt.Sprintf(`{"posting_date": "2026-03-24", "items": [%s]}`, second), "")
	apps, _ = got["applications"].([]any)
	is(t, "applied again: applications", len(apps), 3)
	is(t, "applied again: version", got["version"], 6)
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
	is(t, "journal entries more after the refusal", e.entries(t)-before, 0)
	is(t, "resets after the refusal", resets, 0)
	is(t, "open items after the refusal", e.openRefs(t, e.customer), "")
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

// TestResetRacesAnApplication (T75i): undoing a payment races an
// application of its credit on account; the check that no application is
// in force runs before the locks, so the ledger's check under them decides:
// either the application wins and the undoing is refused, or the undoing
// wins and the application is refused, and nothing is left half done.
func TestResetRacesAnApplication(t *testing.T) {
	e := setup(t)
	got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.onAccount(e.customer, e.receipt(t, 10000, "2026-03-20").ID), "")
	id := uuid.MustParse(got["id"].(string))
	invoice := entryOf(t, e.invoiced(t, e.customer, "30.00", "2026-03-22"))
	h := endpoints(t)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.Reset(ctx, receivables.ResetInput{PaymentID: id, IfMatch: etag.IfMatch(`"1"`), Reason: "Racing."})
			return err
		})
	}()
	go func() {
		defer wg.Done()
		errs[1] = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ApplyOnAccount(ctx, receivables.CreditInput{PaymentID: id, Items: []ledger.ItemRef{{FiscalYear: 2026, EntryID: invoice, LineNo: 1}}})
			return err
		})
	}()
	wg.Wait()
	var resets, apps int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM receivables.incoming_payment_reset WHERE tenant_id = $1),
		(SELECT count(*) FROM receivables.incoming_payment_application WHERE tenant_id = $1)`, []any{e.Tenant.ID}, &resets, &apps)
	switch {
	case errs[0] == nil && errs[1] != nil:
		if c := problem.From(errs[1]).Code; c != "ledger.entry_reversed" && c != "receivables.no_credit_on_account" || resets != 1 || apps != 0 {
			t.Errorf("the undoing won: application refused with %v, %d resets, %d applications; want entry_reversed or no_credit_on_account, 1, 0", errs[1], resets, apps)
		}
	case errs[1] == nil && errs[0] != nil:
		if c := problem.From(errs[0]).Code; c != "ledger.clearing_lines_cleared" && c != "receivables.applications_in_force" || resets != 0 || apps != 1 {
			t.Errorf("the application won: undoing refused with %v, %d resets, %d applications; want clearing_lines_cleared or applications_in_force, 0, 1", errs[0], resets, apps)
		}
	default:
		t.Errorf("got %v and %v, want one of them refused", errs[0], errs[1])
	}
}
