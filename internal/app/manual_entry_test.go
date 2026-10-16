package app_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// manualEntries is a configured tenant whose people park and post manual
// entries of legal entity 2000 (EUR, manual_entry_approval_limit 10000.00).
type manualEntries struct {
	fixture.Env
	bank, revenue string
}

func newManualEntries(t *testing.T) manualEntries {
	t.Helper()
	f := fixture.New(t, "ledger.entry.read", "ledger.entry.post", "ledger.entry.park", "ledger.period.read", "ledger.period.manage",
		"ledger.rate.read", "ledger.rate.maintain")
	return manualEntries{Env: f, bank: f.Account(t, "2000", "100000").String(), revenue: f.Account(t, "2000", "400000").String()}
}

// park parks a two-line entry of amount in currency, dated date, as c,
// and returns its path.
func (m manualEntries) park(t *testing.T, c apitest.Client, date, currency, amount string) string {
	t.Helper()
	body := fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q, "lines": [
		{"account_id": %q, "amount": {"amount": %q, "currency": %q}}, {"account_id": %q, "amount": {"amount": "-%s", "currency": %q}}]}`,
		m.DE, date, date, currency, m.bank, amount, currency, m.revenue, amount, currency)
	pe, _ := c.Must(201, "POST", "/v1/parked-journal-entries", body, "")
	return "/v1/parked-journal-entries/" + pe["id"].(string)
}

// post posts the parked entry at path as its owner and returns it.
func (m manualEntries) post(t *testing.T, path string) map[string]any {
	t.Helper()
	_, tag := m.Must(200, "GET", path, "", "")
	pe, _ := m.Must(200, "POST", path+":post", "", tag)
	return pe
}

func expect(t *testing.T, what string, got map[string]any, fields map[string]any) {
	t.Helper()
	for field, w := range fields {
		if got[field] != w {
			t.Errorf("%s %s: got %v, want %v", what, field, got[field], w)
		}
	}
}

// TestManualEntryWithinTheLimit: an entry whose debits are within its
// company's limit is posted at once, with no approval request.
func TestManualEntryWithinTheLimit(t *testing.T) {
	m := newManualEntries(t)
	path := m.park(t, m.Client, "2026-03-15", "EUR", "10000.00")
	pe := m.post(t, path)
	expect(t, "posted entry", pe, map[string]any{"status": "posted", "journal_fiscal_year": float64(2026), "approval_request_id": nil})
	je, _ := m.Must(200, "GET", "/v1/journal-entries/"+pe["journal_entry_id"].(string), "", "")
	expect(t, "journal entry", je, map[string]any{"source_context": "ledger", "source_document_id": pe["id"], "posted_by": m.Tenant.Human.ID.String()})
	if page, _ := m.As(m.Approver).Must(200, "GET", "/v1/approval-requests?object_id="+pe["id"].(string), "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("approval requests of an entry within the limit: got %v, want none", page["items"])
	}
	changed := apitest.Published(t, m.Tenant, outbox.Event{Context: "ledger", Name: "parked_journal_entry_changed"})
	if len(changed) != 2 {
		t.Fatalf("parked events: got %d, want 2", len(changed))
	}
	expect(t, "post event", changed[1], map[string]any{"status": "posted", "previous_status": "parked", "journal_entry_id": pe["journal_entry_id"]})
}

// TestManualEntryAboveTheLimit: above the limit the entry waits for the
// approval of someone else, who posts it by approving.
func TestManualEntryAboveTheLimit(t *testing.T) {
	m := newManualEntries(t)
	path := m.park(t, m.Client, "2026-03-15", "EUR", "10000.01")
	pe := m.post(t, path)
	expect(t, "submitted entry", pe, map[string]any{"status": "pending_approval", "journal_entry_id": nil})
	r := m.Pending(t, pe["id"].(string))
	expect(t, "request", r, map[string]any{"object_type": "manual_entry", "requester_id": m.Tenant.Human.ID.String(), "id": pe["approval_request_id"]})
	expect(t, "request amount", r["amount"].(map[string]any), map[string]any{"amount": "10000.01", "currency": "EUR"})
	version := fmt.Sprintf(`"%v"`, r["version"])
	m.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "Mine"}`, version)
	agent := m.As(m.Tenant.Agent)
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, version)
	// Frozen while it is decided.
	_, tag := m.Must(200, "GET", path, "", "")
	m.Problem(409, "ledger.invalid_parked_status", "PATCH", path, `{"header_text": "x"}`, tag)
	m.Problem(409, "ledger.invalid_parked_status", "POST", path+":post", "", tag)

	m.As(m.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, version)
	posted, _ := m.Must(200, "GET", path, "", "")
	expect(t, "approved entry", posted, map[string]any{"status": "posted", "approval_request_id": r["id"]})
	je, _ := m.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	expect(t, "journal entry", je, map[string]any{"source_document_id": pe["id"], "posted_by": m.Approver.ID.String()})
	changed := apitest.Published(t, m.Tenant, outbox.Event{Context: "ledger", Name: "parked_journal_entry_changed"})
	if len(changed) != 3 {
		t.Fatalf("parked events: got %d, want 3", len(changed))
	}
	expect(t, "submit event", changed[1], map[string]any{"status": "pending_approval", "previous_status": "parked", "approval_request_id": r["id"]})
	expect(t, "approval event", changed[2], map[string]any{"status": "posted", "previous_status": "pending_approval"})
}

// TestManualEntryInAnotherCurrency: an entry in a foreign currency is
// measured in its company's currency at the rate of its posting date.
func TestManualEntryInAnotherCurrency(t *testing.T) {
	m := newManualEntries(t)
	m.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR", "valid_from": "2026-01-01",
		"rate": "1.25", "source": "ECB"}`, "")
	within := m.post(t, m.park(t, m.Client, "2026-03-15", "USD", "8000.00")) // 10000.00 EUR
	above := m.post(t, m.park(t, m.Client, "2026-03-15", "USD", "8000.01"))  // 10000.0125 EUR
	expect(t, "8000.00 USD", within, map[string]any{"status": "posted"})
	expect(t, "8000.01 USD", above, map[string]any{"status": "pending_approval"})
}

// TestManualEntryRejectedOrWithdrawn: a rejected entry is rejected for
// good; a withdrawn one is parked again, to change and post again.
func TestManualEntryRejectedOrWithdrawn(t *testing.T) {
	m := newManualEntries(t)
	rejected := m.post(t, m.park(t, m.Client, "2026-03-15", "EUR", "20000.00"))
	r := m.Pending(t, rejected["id"].(string))
	m.As(m.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "No"}`, fmt.Sprintf(`"%v"`, r["version"]))
	path := "/v1/parked-journal-entries/" + rejected["id"].(string)
	got, tag := m.Must(200, "GET", path, "", "")
	expect(t, "rejected entry", got, map[string]any{"status": "rejected", "approval_request_id": r["id"], "journal_entry_id": nil})
	m.Problem(409, "ledger.invalid_parked_status", "POST", path+":post", "", tag)

	withdrawn := m.post(t, m.park(t, m.Client, "2026-03-15", "EUR", "20000.00"))
	r = m.Pending(t, withdrawn["id"].(string))
	m.Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":cancel", `{"comment": "Wrong amount"}`, fmt.Sprintf(`"%v"`, r["version"]))
	path = "/v1/parked-journal-entries/" + withdrawn["id"].(string)
	got, tag = m.Must(200, "GET", path, "", "")
	expect(t, "withdrawn entry", got, map[string]any{"status": "parked", "approval_request_id": nil})
	m.Must(200, "PATCH", path, fmt.Sprintf(`{"lines": [{"account_id": %q, "amount": {"amount": "500.00", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-500.00", "currency": "EUR"}}]}`, m.bank, m.revenue), tag)
	expect(t, "corrected entry", m.post(t, path), map[string]any{"status": "posted"})
}

// TestManualEntryPeriodClosedWhilePending: an approval does not post into
// a period closed meanwhile; the request stays pending to be decided again.
func TestManualEntryPeriodClosedWhilePending(t *testing.T) {
	m := newManualEntries(t)
	pe := m.post(t, m.park(t, m.Client, "2026-03-15", "EUR", "20000.00"))
	page, _ := m.Must(200, "GET", fmt.Sprintf("/v1/fiscal-periods?legal_entity_id=%s&fiscal_year=2026&order_by=period&page_size=3", m.DE), "", "")
	period := page["items"].([]any)[2].(map[string]any)
	m.Must(200, "POST", "/v1/fiscal-periods/"+period["id"].(string)+":close", "", fmt.Sprintf(`"%v"`, period["version"]))
	r := m.Pending(t, pe["id"].(string))
	m.As(m.Approver).Problem(409, "ledger.period_not_open", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	expect(t, "request after the refused approval", m.Pending(t, pe["id"].(string)), map[string]any{"status": "pending"})
	got, _ := m.Must(200, "GET", "/v1/parked-journal-entries/"+pe["id"].(string), "", "")
	expect(t, "entry after the refused approval", got, map[string]any{"status": "pending_approval"})
}

// TestAgentsNeverPost: an agent parks its proposal; a person posts it, and
// approves it only if someone else posted it.
func TestAgentsNeverPost(t *testing.T) {
	m := newManualEntries(t)
	m.Tenant.AllCompanies(t, m.Tenant.Agent)
	agent := m.As(m.Tenant.Agent)
	path := m.park(t, agent, "2026-03-15", "EUR", "20000.00")
	_, tag := agent.Must(200, "GET", path, "", "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":post", "", tag)
	pe := m.post(t, path)
	expect(t, "proposal posted by a person", pe, map[string]any{"status": "pending_approval", "owner_id": m.Tenant.Human.ID.String()})
	expect(t, "its request", m.Pending(t, pe["id"].(string)), map[string]any{"requester_id": m.Tenant.Human.ID.String()})
}

// TestApproverWithoutTheCompany: the request authorises the posting, so an
// approver who may not act in the entry's legal entity still posts it.
func TestApproverWithoutTheCompany(t *testing.T) {
	m := newManualEntries(t)
	pe := m.post(t, m.park(t, m.Client, "2026-03-15", "EUR", "20000.00"))
	pgtest.Exec(t, `UPDATE identity.principal SET all_companies = false, version = version + 1 WHERE id = $1`, m.Approver.ID)
	r := m.Pending(t, pe["id"].(string))
	m.As(m.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
	got, _ := m.Must(200, "GET", "/v1/parked-journal-entries/"+pe["id"].(string), "", "")
	expect(t, "entry approved by an approver outside the company", got, map[string]any{"status": "posted"})
}
