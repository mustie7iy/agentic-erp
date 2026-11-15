package payables_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestRefusedPostAudited: a posting the API refuses with a business
// outcome is audited as refused on the invoice, with the problem's code
// and detail and the posting date asked for, although the posting rolls
// back (ADR-0072 As built, T108b); an invoice that is not found leaves no
// record, and the invoice posts once its period is open.
func TestRefusedPostAudited(t *testing.T) {
	e := setup(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "100.00", "0.00", untaxedItem(e.expense, "100.00")), "")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`,
		e.Tenant.ID)
	p := e.Problem(409, "ledger.period_not_open", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-02-10"}`, e.tag(t, id(inv)))
	var action, outcome, code, detail, date string
	pgtest.Scan(t, `SELECT action, outcome, detail->>'code', detail->>'detail', coalesce(detail->>'posting_date', '') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_id = $2 AND outcome = 'refused'`, []any{e.Tenant.ID, id(inv)}, &action, &outcome, &code, &detail, &date)
	if got := action + " " + outcome + " " + code + " " + date; got != "payables.supplier_invoice.post refused ledger.period_not_open 2026-02-10" {
		t.Errorf("the refusal's audit: got %s, want the posting refused for the closed period on 2026-02-10", got)
	}
	if detail != p["detail"] {
		t.Errorf("the refusal's detail: got %q, want the problem's %q", detail, p["detail"])
	}
	// Asked without a posting date, a refusal names none.
	pgtest.Exec(t, `UPDATE payables.supplier_invoice SET posting_date = '2026-02-11', version = version + 1 WHERE id = $1`, id(inv))
	e.Problem(409, "ledger.period_not_open", "POST", "/v1/supplier-invoices/"+id(inv)+":post", "", e.tag(t, id(inv)))
	var dated, undated int
	pgtest.Scan(t, `SELECT count(*) FILTER (WHERE detail ? 'posting_date'), count(*) FILTER (WHERE NOT detail ? 'posting_date') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_id = $2 AND outcome = 'refused'`, []any{e.Tenant.ID, id(inv)}, &dated, &undated)
	if dated != 1 || undated != 1 {
		t.Errorf("refusals with and without a posting date: got %d and %d, want 1 and 1", dated, undated)
	}
	e.Problem(404, "request.not_found", "POST", "/v1/supplier-invoices/01a1aa00-0000-7000-8000-000000000999:post", "", `"1"`)
	var nowhere int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = '01a1aa00-0000-7000-8000-000000000999'`,
		[]any{e.Tenant.ID}, &nowhere)
	if nowhere != 0 {
		t.Errorf("audit of an invoice not found: got %d, want none", nowhere)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`,
		e.Tenant.ID)
	if got := e.act(t, 200, id(inv), "post", "")["status"]; got != "posted" {
		t.Errorf("status once the period is open: got %v, want posted", got)
	}
}
