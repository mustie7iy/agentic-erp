package closing_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// statusLine is a proposal's derived status as one line: its status,
// whether modified, the amount posted, its journal entry's and reversal's
// numbers, and who rejected it and why.
func statusLine(m map[string]any) string {
	posted := "<nil>"
	if p, ok := m["posted_amount"].(map[string]any); ok {
		posted = fmt.Sprintf("%v %v", p["amount"], p["currency"])
	}
	return fmt.Sprintf("%v modified=%v posted=%s entry=%v reversal=%v decided=%v %v", m["status"], m["modified"], posted, m["journal_entry_number"],
		m["reversal_entry_number"], m["decided_by_name"], m["decision_reason"])
}

// read is a proposal as a person reads it, with its ETag.
func (a *accruing) read(t *testing.T, id string) (map[string]any, string) {
	t.Helper()
	return a.Must(200, "GET", "/v1/accrual-proposals/"+id, "", "")
}

// post posts a proposal's parked entry as the person, and returns the
// journal entry's id and document number.
func (a *accruing) post(t *testing.T, p map[string]any) (string, string) {
	t.Helper()
	entry := "/v1/parked-journal-entries/" + apitest.String(t, p, "parked_entry_id")
	_, tag := a.Must(200, "GET", entry, "", "")
	posted, _ := a.Must(200, "POST", entry+":post", "", tag)
	je, _ := a.Must(200, "GET", "/v1/journal-entries/"+apitest.String(t, posted, "journal_entry_id"), "", "")
	return apitest.String(t, je, "id"), apitest.String(t, je, "document_number")
}

// reverse reverses a journal entry as the person and returns the
// reversal's document number.
func (a *accruing) reverse(t *testing.T, entry string) string {
	t.Helper()
	_, tag := a.Must(200, "GET", "/v1/journal-entries/"+entry, "", "")
	r, _ := a.Must(200, "POST", "/v1/journal-entries/"+entry+":reverse", "", tag)
	return apitest.String(t, r, "document_number")
}

// cancel cancels a proposal's parked entry in the ledger as the person.
func (a *accruing) cancel(t *testing.T, p map[string]any) {
	t.Helper()
	entry := "/v1/parked-journal-entries/" + apitest.String(t, p, "parked_entry_id")
	_, tag := a.Must(200, "GET", entry, "", "")
	a.Must(200, "POST", entry+":cancel", `{"reason": "Not ours"}`, tag)
}

// propose is the agent's proposal of the amount on the account of company
// 2000 with the number, citing the invoice, with the extra JSON members.
func (a *accruing) propose(t *testing.T, number, amount, extra string) map[string]any {
	t.Helper()
	p, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.body(a.Account(t, "2000", number), amount, a.session,
		[][2]string{{"supplier_invoice", a.invoice.String()}}, extra), "")
	return p
}

// TestAccrualStatuses: a proposal's status is derived from its parked
// entry at every read: proposed while parked, pending approval once
// submitted, posted with its journal entry's number, modified with the
// amount posted when a person changed it first, reversed with the
// reversal's number, rejected once the ledger cancelled it. The list
// filters by the derived status and sums the filtered set per status and
// currency, on every page alike.
func TestAccrualStatuses(t *testing.T) {
	a := newAccruing(t)
	asProposed := a.propose(t, "520000", "100.00", fmt.Sprintf(`, "supplier_id": %q, "cost_center_id": %q`, a.Supplier, a.centers.Cost))
	changed := a.propose(t, "520000", "40.00", "")
	reversed := a.propose(t, "530000", "30.00", fmt.Sprintf(`, "profit_center_id": %q`, a.centers.Profit))
	cancelled := a.propose(t, "530000", "20.00", fmt.Sprintf(`, "supplier_id": %q, "profit_center_id": %q`, a.Supplier, a.centers.Profit))
	open := a.propose(t, "530000", "5.00", fmt.Sprintf(`, "supplier_id": %q, "cost_center_id": %q`, a.Supplier, a.centers.Cost))
	if got, want := statusLine(open), "proposed modified=false posted=<nil> entry=<nil> reversal=<nil> decided=<nil> <nil>"; got != want {
		t.Errorf("a proposal just made: got %s, want %s", got, want)
	}
	pgtest.Unguarded(t, `UPDATE ledger.parked_entry SET status = 'pending_approval', approval_request_id = gen_random_uuid(), owner_id = $2 WHERE id = $1`,
		uuid.MustParse(apitest.String(t, open, "parked_entry_id")), a.Tenant.Human.ID)
	if got, _ := a.read(t, apitest.String(t, open, "id")); got["status"] != "pending_approval" {
		t.Errorf("submitted for approval: got %v, want pending_approval", got["status"])
	}

	_, number := a.post(t, asProposed)
	got, tag := a.read(t, apitest.String(t, asProposed, "id"))
	if line, want := statusLine(got), "posted modified=false posted=100.00 EUR entry="+number+" reversal=<nil> decided=<nil> <nil>"; line != want {
		t.Errorf("posted as proposed: got %s, want %s", line, want)
	}
	if tag != `"1"` {
		t.Errorf("a derived status moves no version: got ETag %s, want \"1\"", tag)
	}
	entry := "/v1/parked-journal-entries/" + apitest.String(t, changed, "parked_entry_id")
	_, etag := a.Must(200, "GET", entry, "", "")
	a.Must(200, "PATCH", entry, fmt.Sprintf(`{"lines": [{"account_id": %q, "amount": {"amount": "45.50", "currency": "EUR"}, "line_text": "Courier"},
		{"account_id": %q, "amount": {"amount": "-45.50", "currency": "EUR"}, "line_text": "Courier"}]}`, a.expense, a.accrualAcount), etag)
	_, number = a.post(t, changed)
	if got, _ := a.read(t, apitest.String(t, changed, "id")); statusLine(got) != "posted modified=true posted=45.50 EUR entry="+number+" reversal=<nil> decided=<nil> <nil>" {
		t.Errorf("posted changed: got %s, want posted, modified, 45.50 EUR, entry %s", statusLine(got), number)
	}
	je, number := a.post(t, reversed)
	reversal := a.reverse(t, je)
	if got, _ := a.read(t, apitest.String(t, reversed, "id")); statusLine(got) != "reversed modified=false posted=30.00 EUR entry="+number+" reversal="+reversal+" decided=<nil> <nil>" {
		t.Errorf("reversed: got %s, want reversed with entry %s and reversal %s", statusLine(got), number, reversal)
	}
	a.cancel(t, cancelled)
	if got, _ := a.read(t, apitest.String(t, cancelled, "id")); statusLine(got) != "rejected modified=false posted=<nil> entry=<nil> reversal=<nil> decided=<nil> <nil>" {
		t.Errorf("cancelled in the ledger: got %s, want rejected, decided by no one here", statusLine(got))
	}

	list := func(query string) ([]string, []string) {
		page, _ := a.Must(200, "GET", "/v1/accrual-proposals?"+query, "", "")
		var ids, totals []string
		for _, x := range apitest.List(t, page, "items") {
			ids = append(ids, x.(map[string]any)["id"].(string))
		}
		for _, x := range apitest.List(t, page, "totals") {
			r := x.(map[string]any)
			posted := "-"
			if p, ok := r["posted_amount"].(map[string]any); ok {
				posted = fmt.Sprint(p["amount"])
			}
			totals = append(totals, fmt.Sprintf("%v %v %v: %v posted %v, %v modified", r["status"], r["currency"], r["count"], r["amount"].(map[string]any)["amount"],
				posted, r["modified_count"]))
		}
		return ids, totals
	}
	every := []string{"pending_approval EUR 1: 5.00 posted -, 0 modified", "posted EUR 2: 140.00 posted 145.50, 1 modified",
		"reversed EUR 1: 30.00 posted 30.00, 0 modified", "rejected EUR 1: 20.00 posted -, 0 modified"}
	cases := []struct {
		name, query string
		ids, totals []string
	}{
		{"every proposal", "close_id=" + a.close, idsOf(t, asProposed, changed, reversed, cancelled, open), every},
		{"the first page", "page_size=2", idsOf(t, asProposed, changed), every},
		{"posted", "status=posted", idsOf(t, asProposed, changed), []string{"posted EUR 2: 140.00 posted 145.50, 1 modified"}},
		{"rejected", "status=rejected", idsOf(t, cancelled), []string{"rejected EUR 1: 20.00 posted -, 0 modified"}},
		{"proposed", "status=proposed", nil, nil},
		{"another company's", "legal_entity_id=" + a.US.String(), nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, totals := list(tc.query)
			wantLines(t, "proposals", ids, tc.ids)
			wantLines(t, "totals", totals, tc.totals)
		})
	}
}

// idsOf are the proposals' ids.
func idsOf(t *testing.T, ps ...map[string]any) []string {
	t.Helper()
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = apitest.String(t, p, "id")
	}
	return out
}

// TestRejectAccrualProposal: a person rejects a proposal for a reason:
// its parked entry is cancelled, owned by them now, the proposal rejected
// with who, when and why, audited and published; it is not rejected
// again, nor once posted. A rejected proposal, or one whose entry the
// ledger cancelled, no longer blocks a new proposal of the same expense.
func TestRejectAccrualProposal(t *testing.T) {
	a := newAccruing(t)
	p, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("100.00"), "")
	id := apitest.String(t, p, "id")
	path := "/v1/accrual-proposals/" + id + ":rejectProposal"
	a.Problem(404, "request.not_found", "POST", "/v1/accrual-proposals/"+uuid.NewString()+":rejectProposal", `{"reason": "No"}`, `"1"`)
	a.Problem(422, "request.validation_failed", "POST", path, `{"reason": " "}`, `"1"`)
	a.Problem(412, "request.version_mismatch", "POST", path, `{"reason": "No"}`, `"7"`)
	a.agent.Problem(403, "identity.agent_not_allowed", "POST", path, `{"reason": "No"}`, `"1"`)
	got, tag := a.Must(200, "POST", path, `{"reason": "The courier invoiced March already"}`, `"1"`)
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	if line := statusLine(got); line != "rejected modified=false posted=<nil> entry=<nil> reversal=<nil> decided=human The courier invoiced March already" {
		t.Errorf("rejected: got %s", line)
	}
	if _, ok := got["decided_at"]; !ok {
		t.Errorf("decided_at: got none, want when it was rejected")
	}
	parked, _ := a.Must(200, "GET", "/v1/parked-journal-entries/"+apitest.String(t, p, "parked_entry_id"), "", "")
	wantFields(t, "its parked entry", parked, map[string]any{"status": "cancelled", "owner_id": a.Tenant.Human.ID.String()})
	events := apitest.Published(t, a.Tenant, outbox.Event{Context: "closing", Name: "accrual_proposal_rejected"})
	if len(events) != 1 || events[0]["accrual_proposal_id"] != id || events[0]["reason"] != "The courier invoiced March already" {
		t.Errorf("AccrualProposalRejected: got %v, want one of %s with the reason", events, id)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'closing.accrual_proposal.reject' AND object_id = $2`,
		[]any{a.Tenant.ID, id}, &audited)
	if audited != 1 {
		t.Errorf("audit records of the rejection: got %d, want 1", audited)
	}
	got = a.Problem(409, "closing.accrual_not_open", "POST", path, `{"reason": "Again"}`, `"2"`)
	if !strings.Contains(fmt.Sprint(got["detail"]), "is rejected") {
		t.Errorf("detail: got %v, want it to name the status", got["detail"])
	}

	again, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("90.00"), "")
	a.cancel(t, again)
	third, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("80.00"), "")
	a.post(t, third)
	a.Problem(409, "closing.accrual_not_open", "POST", "/v1/accrual-proposals/"+apitest.String(t, third, "id")+":rejectProposal", `{"reason": "No"}`, `"1"`)
	a.agent.Problem(409, "closing.accrual_duplicate", "POST", "/v1/accrual-proposals", a.proposal("70.00"), "")
}

// TestRejectAccrualProposalOwner: an entry another person took on is
// theirs to cancel: the rejection is refused with the ledger's problem and
// changes nothing.
func TestRejectAccrualProposalOwner(t *testing.T) {
	a := newAccruing(t)
	p, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("100.00"), "")
	other := a.As(a.Person(t, "Other controller"))
	entry := "/v1/parked-journal-entries/" + apitest.String(t, p, "parked_entry_id")
	other.Must(200, "PATCH", entry, `{"header_text": "Taken on"}`, `"1"`)
	a.Problem(403, "ledger.not_entry_owner", "POST", "/v1/accrual-proposals/"+apitest.String(t, p, "id")+":rejectProposal", `{"reason": "No"}`, `"1"`)
	got, tag := a.read(t, apitest.String(t, p, "id"))
	if got["status"] != "proposed" || tag != `"1"` {
		t.Errorf("after the refusal: got %v %s, want proposed at \"1\"", got["status"], tag)
	}
	other.Must(200, "POST", "/v1/accrual-proposals/"+apitest.String(t, p, "id")+":rejectProposal", `{"reason": "Mine to reject"}`, `"1"`)
}

// withAccrualChecks is withAccruals with the two accrual checks.
func withAccrualChecks(doc *config.Config) {
	withAccruals(doc)
	doc.Close.Tasks = append(doc.Close.Tasks,
		config.CloseTask{Code: "ACCRUALS", Name: "Accrual proposals decided", Kind: config.CloseCheck, Check: "accruals_decided", DueDay: 2},
		config.CloseTask{Code: "REVERSED", Name: "Last period's accruals reversed", Kind: config.CloseCheck, Check: "accruals_reversed", DueDay: 1})
}

// check runs a check task of a close and returns its result as one line:
// whether it passed, its summary, the previous close looked at and the
// proposals named, each by account, amount, status and journal entry.
func check(t *testing.T, a *accruing, closeID, code string) string {
	t.Helper()
	_, tag := a.Must(200, "GET", "/v1/period-closes/"+closeID, "", "")
	task, _ := a.Must(200, "POST", "/v1/period-closes/"+closeID+"/tasks/"+code+":check", "", tag)
	r := task["check_result"].(map[string]any)
	out := fmt.Sprintf("%v %v: %v | previous=%v %v/%v count=%v:", r["check"], r["passed"], r["summary"], r["previous_close_id"] != nil,
		r["previous_fiscal_year"], r["previous_fiscal_period"], r["accruals_count"])
	for _, x := range apitest.List(t, r, "accruals") {
		f := x.(map[string]any)
		out += fmt.Sprintf(" %v %v %v %v entry=%v", f["account_number"], f["amount"].(map[string]any)["amount"], f["amount"].(map[string]any)["currency"],
			f["status"], f["journal_entry_number"])
	}
	return out
}

// TestAccrualChecks: accruals_decided fails while a proposal of the close
// is proposed or pending approval, naming it, and passes once each is
// posted or rejected; accruals_reversed passes without a close of the
// previous period, fails while an accrual posted for it is not reversed,
// naming its journal entry, and passes once it is.
func TestAccrualChecks(t *testing.T) {
	a := newAccruingWith(t, withAccrualChecks)
	march := a.close
	jan, _ := open(t, a.Client, a.DE, 1, "")
	if got, want := check(t, a, apitest.String(t, jan, "id"), "REVERSED"), "accruals_reversed true: The company has no close of 2025/12, so no accrual of it to reverse. | previous=false 2025/12 count=0:"; got != want {
		t.Errorf("January, without December's close:\ngot  %s\nwant %s", got, want)
	}
	feb, _ := open(t, a.Client, a.DE, 2, "")
	a.close = apitest.String(t, feb, "id")
	posted := a.propose(t, "520000", "60.00", "")
	rejected := a.propose(t, "530000", "10.00", "")
	if got, want := check(t, a, a.close, "REVERSED"), "accruals_reversed true: Every accrual posted for the close of 2026/1 is reversed. | previous=true 2026/1 count=0:"; got != want {
		t.Errorf("January's close without accruals:\ngot  %s\nwant %s", got, want)
	}
	want := "accruals_decided false: 2 accrual proposals of the close are proposed or pending approval; post or reject them. | previous=false <nil>/<nil> count=2:" +
		" 520000 60.00 EUR proposed entry=<nil> 530000 10.00 EUR proposed entry=<nil>"
	if got := check(t, a, a.close, "ACCRUALS"); got != want {
		t.Errorf("two open:\ngot  %s\nwant %s", got, want)
	}
	second := a.propose(t, "530000", "15.00", fmt.Sprintf(`, "supplier_id": %q`, a.Supplier))
	je2, number2 := a.post(t, second)
	je, number := a.post(t, posted)
	a.Must(200, "POST", "/v1/accrual-proposals/"+apitest.String(t, rejected, "id")+":rejectProposal", `{"reason": "Not an expense of February"}`, `"1"`)
	if got, want := check(t, a, a.close, "ACCRUALS"), "accruals_decided true: Every accrual proposal of the close is decided: posted or rejected. | previous=false <nil>/<nil> count=0:"; got != want {
		t.Errorf("all decided:\ngot  %s\nwant %s", got, want)
	}
	want = "accruals_reversed false: 2 accruals posted for the close of 2026/2 are not reversed; reverse their journal entries in this period. | previous=true 2026/2 count=2:" +
		" 520000 60.00 EUR posted entry=" + number + " 530000 15.00 EUR posted entry=" + number2
	if got := check(t, a, march, "REVERSED"); got != want {
		t.Errorf("February's accruals unreversed:\ngot  %s\nwant %s", got, want)
	}
	a.reverse(t, je2)
	want = "accruals_reversed false: 1 accrual posted for the close of 2026/2 is not reversed; reverse its journal entry in this period. | previous=true 2026/2 count=1:" +
		" 520000 60.00 EUR posted entry=" + number
	if got := check(t, a, march, "REVERSED"); got != want {
		t.Errorf("February's accrual unreversed:\ngot  %s\nwant %s", got, want)
	}
	a.reverse(t, je)
	if got, want := check(t, a, march, "REVERSED"), "accruals_reversed true: Every accrual posted for the close of 2026/2 is reversed. | previous=true 2026/2 count=0:"; got != want {
		t.Errorf("February's accrual reversed:\ngot  %s\nwant %s", got, want)
	}
}
