package closing_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// accrualPermissions are Carla's, who reads proposals, posts their
// entries and rejects them, the agent's (the machine role holds every one of them that is
// no approval), who proposes them in its sessions, and what the evidence
// is entered and posted with.
var accrualPermissions = append(append([]string{}, closePermissions...), "closing.accrual.propose", "closing.accrual.decide", "ledger.entry.park", "ledger.entry.post",
	"ledger.account.read", "ledger.cost_center.read", "ledger.cost_center.change", "agents.session.record", "partner.supplier.read",
	"payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "procurement.order.read")

// withAccruals is withTemplate with company 2000's accruals credited to
// its liability 219500 (ADR-0058 §3).
func withAccruals(doc *config.Config) {
	withTemplate(doc)
	co := doc.Company("2000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "219500", Name: "Accrued liabilities", Type: config.Liability})
	co.Close.AccrualAccount = "219500"
}

// accruing is a close of company 2000's March 2026 under test, with an
// agent session and evidence: a supplier invoice posted, a purchase order
// released and a journal entry posted, each of company 2000.
type accruing struct {
	fixture.Env
	close, session         string
	agent                  apitest.Client
	invoice, order, entry  uuid.UUID
	invoiceNo, orderNo     string
	centers                fixture.JournalCenters
	expense, accrualAcount uuid.UUID
}

func newAccruing(t *testing.T) *accruing {
	t.Helper()
	return newAccruingWith(t, withAccruals)
}

// newAccruingWith is newAccruing with the configuration extend makes.
func newAccruingWith(t *testing.T, extend func(*config.Config)) *accruing {
	t.Helper()
	e := fixture.NewWith(t, nil, extend, accrualPermissions...)
	march, _ := open(t, e.Client, e.DE, 3, "")
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	a := &accruing{Env: e, close: apitest.String(t, march, "id"), agent: e.As(e.Tenant.Agent), centers: e.Centers(t),
		expense: e.Account(t, "2000", "520000"), accrualAcount: e.Account(t, "2000", "219500")}
	s, _ := a.agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Propose the accruals of March", "model": "erp-default"}`, "")
	a.session = apitest.String(t, s, "id")
	a.invoice = uuid.Must(uuid.NewV7())
	id := e.SupplierInvoice(t, e.Supplier, "COURIER-02", "119.00", "19.00", e.AccountItem(t, "Courier of February", "100.00"))
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+id.String(), "", "")
	inv, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id.String()+":post", `{"posting_date": "2026-03-12"}`, tag)
	a.invoice, a.invoiceNo = id, apitest.String(t, inv, "document_number")
	a.order, a.orderNo = e.Order(t, e.Supplier, "4500000001"), "4500000001"
	a.entry = e.PostedBy(t, "", "2026-02-15", 10000)
	return a
}

// body is a proposal of the amount on the account with the evidence
// (kind and id), in the session, for the supplier and with the centers
// when given; extra is more JSON members.
func (a *accruing) body(account uuid.UUID, amount, session string, evidence [][2]string, extra string) string {
	cited := make([]string, len(evidence))
	for i, ev := range evidence {
		cited[i] = fmt.Sprintf(`{"kind": %q, "document_id": %q}`, ev[0], ev[1])
	}
	return fmt.Sprintf(`{"session_id": %q, "close_id": %q, "account_id": %q, "amount": {"amount": %q, "currency": "EUR"}, "description": "Courier of March",
		"reason": "The courier invoices monthly; March's invoice has not arrived.", "evidence": [%s]%s}`, session, a.close, account, amount,
		strings.Join(cited, ", "), extra)
}

// proposal is the agent's proposal of 100.00 on office supplies for the
// supplier, citing the invoice, with the cost center.
func (a *accruing) proposal(amount string) string {
	return a.body(a.expense, amount, a.session, [][2]string{{"supplier_invoice", a.invoice.String()}},
		fmt.Sprintf(`, "supplier_id": %q, "cost_center_id": %q`, a.Supplier, a.centers.Cost))
}

// accrualLine is a proposal as one line: its company and period, agent,
// accounts, centers, supplier, amount, date, status and evidence.
func accrualLine(t *testing.T, m map[string]any) string {
	t.Helper()
	var cited []string
	for _, x := range apitest.List(t, m, "evidence") {
		ev := x.(map[string]any)
		cited = append(cited, fmt.Sprintf("%v %v %v/%v", ev["kind"], ev["document_number"], ev["supplier_number"], ev["supplier_name"]))
	}
	amount := m["amount"].(map[string]any)
	return fmt.Sprintf("%v %v/%v by %v: Dr %v %v (%v %v, %v %v) Cr %v %v for %v %v, %v %v on %v, %v; %q; cites %v", m["company_code"], m["fiscal_year"],
		m["fiscal_period"], m["agent_name"], m["account_number"], m["account_name"], m["cost_center_code"], m["cost_center_name"], m["profit_center_code"],
		m["profit_center_name"], m["accrual_account_number"], m["accrual_account_name"], m["supplier_number"], m["supplier_name"], amount["amount"],
		amount["currency"], m["posting_date"], m["status"], m["description"], cited)
}

// parkedLine is the parked entry of a proposal as one line: its status,
// who parked it, its dates, header text and lines.
func parkedLine(t *testing.T, m map[string]any) string {
	t.Helper()
	out := fmt.Sprintf("%v by %v, %v/%v %v %q:", m["status"], m["created_by_kind"], m["document_date"], m["posting_date"], m["currency"], m["header_text"])
	for _, x := range apitest.List(t, m, "lines") {
		l := x.(map[string]any)
		out += fmt.Sprintf(" %v %v cc=%v pc=%v %q;", l["account_id"], l["amount"].(map[string]any)["amount"], l["cost_center_id"], l["profit_center_id"],
			l["line_text"])
	}
	return out
}

// TestAccrualProposals: the agent proposes an accrual of the close's
// period in its session, citing a posted invoice; it is parked as the
// agent's entry, Dr the expense with its center, Cr the accrual account,
// on the period's last day, both lines with the cost center's profit
// center, and published; the same proposal again answers it, even once
// the accrual account can no longer be posted to, another of the same
// expense is a duplicate, in this session or another. A person reads it with its evidence and posts the entry.
func TestAccrualProposals(t *testing.T) {
	a := newAccruing(t)
	p, tag := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("100.00"), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	id := apitest.String(t, p, "id")
	want := fmt.Sprintf(`2000 2026/3 by agent: Dr 520000 Office Supplies (CC100 Cost center hundred, PC10 Profit center ten) Cr 219500 Accrued liabilities for `+
		`BP90000001 BP90000001, 100.00 EUR on 2026-03-31, proposed; "Courier of March"; cites [supplier_invoice %s BP90000001/BP90000001]`, a.invoiceNo)
	if got := accrualLine(t, p); got != want {
		t.Errorf("proposal:\ngot  %s\nwant %s", got, want)
	}
	entry := apitest.String(t, p, "parked_entry_id")
	parked, entryTag := a.Must(200, "GET", "/v1/parked-journal-entries/"+entry, "", "")
	want = fmt.Sprintf(`parked by agent, 2026-03-31/2026-03-31 EUR "Accrual proposal %s": %s 100.00 cc=%s pc=%s "Courier of March"; %s -100.00 cc=<nil> pc=%s "Courier of March";`,
		id, a.expense, a.centers.Cost, a.centers.Profit, a.accrualAcount, a.centers.Profit)
	if got := parkedLine(t, parked); got != want {
		t.Errorf("parked entry:\ngot  %s\nwant %s", got, want)
	}
	events := apitest.Published(t, a.Tenant, outbox.Event{Context: "closing", Name: "accrual_proposed"})
	if len(events) != 1 || events[0]["accrual_proposal_id"] != id || events[0]["parked_entry_id"] != entry {
		t.Errorf("AccrualProposed: got %v, want one of proposal %s and entry %s", events, id, entry)
	}
	if len(events) == 1 {
		wantFields(t, "AccrualProposed", events[0], map[string]any{"legal_entity_code": "2000", "supplier_number": "BP90000001"})
	}

	again, _ := a.agent.Must(200, "POST", "/v1/accrual-proposals", a.proposal("100.00"), "")
	if again["id"] != id {
		t.Errorf("the same proposal again: got %v, want %v", again["id"], id)
	}
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE legal_entity_id = $1 AND account_id = $2`,
		a.DE, a.accrualAcount)
	if again, _ := a.agent.Must(200, "POST", "/v1/accrual-proposals", a.proposal("100.00"), ""); again["id"] != id {
		t.Errorf("the same proposal again, the accrual account blocked: got %v, want %v", again["id"], id)
	}
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = false, version = version + 1 WHERE legal_entity_id = $1 AND account_id = $2`,
		a.DE, a.accrualAcount)
	a.agent.Problem(409, "closing.accrual_duplicate", "POST", "/v1/accrual-proposals", a.proposal("120.00"), "")
	s, _ := a.agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Again", "model": "erp-default"}`, "")
	other := a.body(a.expense, "100.00", apitest.String(t, s, "id"), [][2]string{{"supplier_invoice", a.invoice.String()}},
		fmt.Sprintf(`, "supplier_id": %q, "cost_center_id": %q`, a.Supplier, a.centers.Cost))
	a.agent.Problem(409, "closing.accrual_duplicate", "POST", "/v1/accrual-proposals", other, "")
	if n := len(apitest.Published(t, a.Tenant, outbox.Event{Context: "closing", Name: "accrual_proposed"})); n != 1 {
		t.Errorf("AccrualProposed after the retry and duplicates: got %d, want 1", n)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'closing.accrual_proposal.propose' AND object_id = $2`,
		[]any{a.Tenant.ID, id}, &audited)
	if audited != 1 {
		t.Errorf("audit records of the proposal: got %d, want 1", audited)
	}

	got, getTag := a.Must(200, "GET", "/v1/accrual-proposals/"+id, "", "")
	if line := accrualLine(t, got); line != accrualLine(t, p) || getTag != tag {
		t.Errorf("GET: got %s %s, want %s %s", line, getTag, accrualLine(t, p), tag)
	}
	posted, _ := a.Must(200, "POST", "/v1/parked-journal-entries/"+entry+":post", "", entryTag)
	if posted["status"] != "posted" {
		t.Errorf("the entry posted by a person: got %v, want posted", posted["status"])
	}
	a.agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/parked-journal-entries/"+entry+":post", "", `"*"`)
}

// TestAccrualProposalEvidence: a proposal may rest on a posted invoice, a
// released order and a posted journal entry of the company, each kept
// with its number, an invoice or an order with its supplier, an entry
// with its fiscal year; without a supplier or centers, the parked entry
// carries none, and AccrualProposed names no supplier. A profit center is
// on both lines.
func TestAccrualProposalEvidence(t *testing.T) {
	a := newAccruing(t)
	body := a.body(a.Account(t, "2000", "530000"), "50.00", a.session, [][2]string{{"journal_entry", a.entry.String()},
		{"purchase_order", a.order.String()}, {"supplier_invoice", a.invoice.String()}}, fmt.Sprintf(`, "profit_center_id": %q`, a.centers.Profit))
	p, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", body, "")
	var cited []string
	for _, x := range apitest.List(t, p, "evidence") {
		ev := x.(map[string]any)
		cited = append(cited, fmt.Sprintf("%v fy=%v supplier=%v", ev["kind"], ev["fiscal_year"], ev["supplier_id"]))
	}
	want := []string{"journal_entry fy=2026 supplier=<nil>", "purchase_order fy=<nil> supplier=" + a.Supplier.String(),
		"supplier_invoice fy=<nil> supplier=" + a.Supplier.String()}
	wantLines(t, "evidence", cited, want)
	wantFields(t, "proposal without supplier or cost center", p, map[string]any{"supplier_id": nil, "cost_center_id": nil, "profit_center_code": "PC10",
		"profit_center_name": "Profit center ten"})
	events := apitest.Published(t, a.Tenant, outbox.Event{Context: "closing", Name: "accrual_proposed"})
	if len(events) != 1 {
		t.Fatalf("AccrualProposed: got %d, want 1", len(events))
	}
	wantFields(t, "AccrualProposed without supplier", events[0], map[string]any{"supplier_id": nil, "supplier_number": nil, "legal_entity_code": "2000"})
	parked, _ := a.Must(200, "GET", "/v1/parked-journal-entries/"+apitest.String(t, p, "parked_entry_id"), "", "")
	for i, x := range apitest.List(t, parked, "lines") {
		if got := x.(map[string]any)["profit_center_id"]; got != a.centers.Profit.String() {
			t.Errorf("line %d's profit center: got %v, want %s", i+1, got, a.centers.Profit)
		}
	}
}

// TestAccrualProposalRefusals: each refusal of a proposal, with its
// problem and field; nothing is parked.
func TestAccrualProposalRefusals(t *testing.T) {
	a := newAccruing(t)
	other := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, '4500000002', $3, $4, 'EUR', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		a.Tenant.ID, other, a.DE, a.Supplier, a.Tenant.Human.ID)
	parkedInvoice := a.SupplierInvoice(t, a.Supplier, "COURIER-03", "119.00", "19.00", a.AccountItem(t, "Courier", "100.00"))
	usEntry := uuid.Must(uuid.NewV7())
	ended, _ := a.agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Ended", "model": "erp-default"}`, "")
	a.agent.Must(200, "POST", "/v1/agent-sessions/"+apitest.String(t, ended, "id")+":end", `{"status": "completed", "input_tokens": 0, "output_tokens": 0}`, `"1"`)
	invoice := [][2]string{{"supplier_invoice", a.invoice.String()}}
	cases := []struct {
		name   string
		client apitest.Client
		body   string
		status int
		code   string
	}{
		{"a person's proposal", a.Client, a.proposal("100.00"), 403, "agents.not_an_agent"},
		{"an ended session", a.agent, a.body(a.expense, "1.00", apitest.String(t, ended, "id"), invoice, ""), 409, "agents.session_ended"},
		{"another's session", a.agent, a.body(a.expense, "1.00", uuid.NewString(), invoice, ""), 422, "request.reference_not_found"},
		{"a description of white space", a.agent, strings.Replace(a.proposal("1.00"), `"Courier of March"`, `" "`, 1), 422, "request.validation_failed"},
		{"a reason of white space", a.agent, strings.Replace(a.proposal("1.00"), `"The courier invoices monthly; March's invoice has not arrived."`, `" "`, 1),
			422, "request.validation_failed"},
		{"an unknown close", a.agent, strings.Replace(a.proposal("1.00"), a.close, uuid.NewString(), 1), 404, "request.not_found"},
		{"an amount of zero", a.agent, a.proposal("0.00"), 422, "request.invalid_amount"},
		{"a negative amount", a.agent, a.proposal("-1.00"), 422, "request.invalid_amount"},
		{"an amount finer than cents", a.agent, a.proposal("1.001"), 422, "request.invalid_amount"},
		{"an amount in another currency", a.agent, strings.Replace(a.proposal("1.00"), `"currency": "EUR"`, `"currency": "USD"`, 1), 422, "request.invalid_amount"},
		{"a balance sheet account", a.agent, a.body(a.Account(t, "2000", "100000"), "1.00", a.session, invoice, ""), 422, "closing.accrual_account_invalid"},
		{"an unknown account", a.agent, a.body(uuid.New(), "1.00", a.session, invoice, ""), 422, "closing.accrual_account_invalid"},
		{"another company's account", a.agent, a.body(a.Account(t, "1000", "520000"), "1.00", a.session, invoice, ""), 422, "closing.accrual_account_invalid"},
		{"an unknown supplier", a.agent, a.body(a.expense, "1.00", a.session, invoice, fmt.Sprintf(`, "supplier_id": %q`, uuid.New())), 422,
			"request.reference_not_found"},
		{"an invoice not posted", a.agent, a.body(a.expense, "1.00", a.session, [][2]string{{"supplier_invoice", parkedInvoice.String()}}, ""), 422,
			"closing.evidence_unknown"},
		{"an order not released", a.agent, a.body(a.expense, "1.00", a.session, [][2]string{{"purchase_order", other.String()}}, ""), 422,
			"closing.evidence_unknown"},
		{"an unknown order", a.agent, a.body(a.expense, "1.00", a.session, [][2]string{{"purchase_order", uuid.NewString()}}, ""), 422,
			"closing.evidence_unknown"},
		{"an unknown entry", a.agent, a.body(a.expense, "1.00", a.session, [][2]string{{"journal_entry", usEntry.String()}}, ""), 422,
			"closing.evidence_unknown"},
		{"an invoice cited as an entry", a.agent, a.body(a.expense, "1.00", a.session, [][2]string{{"journal_entry", a.invoice.String()}}, ""), 422,
			"closing.evidence_unknown"},
		{"an unknown kind", a.agent, a.body(a.expense, "1.00", a.session, [][2]string{{"goods_receipt", a.invoice.String()}}, ""), 422,
			"closing.evidence_unknown"},
		{"a document cited twice", a.agent, a.body(a.expense, "1.00", a.session, append(invoice, invoice[0]), ""), 400, "request.invalid_body"},
		{"no evidence", a.agent, a.body(a.expense, "1.00", a.session, nil, ""), 400, "request.invalid_body"},
		{"a cost center the ledger refuses", a.agent, a.body(a.expense, "1.00", a.session, invoice, fmt.Sprintf(`, "cost_center_id": %q`, uuid.New())), 422,
			"ledger.cost_center_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.client.Problem(tc.status, tc.code, "POST", "/v1/accrual-proposals", tc.body, "")
			if tc.code == "closing.evidence_unknown" && !strings.Contains(fmt.Sprint(got["errors"]), "evidence[0]") {
				t.Errorf("errors: got %v, want evidence[0]", got["errors"])
			}
			if tc.code == "ledger.cost_center_invalid" && (!strings.Contains(fmt.Sprint(got["errors"]), "field:cost_center_id") ||
				!strings.HasPrefix(fmt.Sprint(got["detail"]), "The expense line: ")) {
				t.Errorf("the ledger's refusal: got %v %v, want it on cost_center_id, naming the expense line", got["errors"], got["detail"])
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.parked_entry WHERE tenant_id = $1 AND created_by_kind = 'agent'`, []any{a.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("entries parked by the refused proposals: got %d, want none", n)
	}
}

// TestAccrualAccountMissing: a company that configures no accrual account,
// or one it cannot post to by hand, refuses every proposal.
func TestAccrualAccountMissing(t *testing.T) {
	a := newAccruing(t)
	body := a.body(a.expense, "1.00", a.session, [][2]string{{"supplier_invoice", a.invoice.String()}}, "")
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE legal_entity_id = $1 AND account_id = $2`,
		a.DE, a.accrualAcount)
	a.agent.Problem(409, "closing.accrual_account_missing", "POST", "/v1/accrual-proposals", body, "")
	activate(t, a.Tenant.ID, func(doc *config.Config) { doc.Company("2000").Close.AccrualAccount = "" })
	got := a.agent.Problem(409, "closing.accrual_account_missing", "POST", "/v1/accrual-proposals", body, "")
	if !strings.Contains(fmt.Sprint(got["detail"]), "configures no accrual account") {
		t.Errorf("detail: got %v, want it to say the company configures none", got["detail"])
	}
}

// TestAccrualProposalList: proposals listed oldest first and filtered by
// close, company, session and parked entry, cursor-paged; a reader of
// another company sees none, and one who may not read business partners
// sees the suppliers' ids without their numbers and names.
func TestAccrualProposalList(t *testing.T) {
	a := newAccruing(t)
	first, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.proposal("100.00"), "")
	second, _ := a.agent.Must(201, "POST", "/v1/accrual-proposals", a.body(a.Account(t, "2000", "530000"), "20.00", a.session,
		[][2]string{{"supplier_invoice", a.invoice.String()}}, ""), "")
	ids := func(c apitest.Client, query string) string {
		page, _ := c.Must(200, "GET", "/v1/accrual-proposals?"+query, "", "")
		var out []string
		for _, x := range apitest.List(t, page, "items") {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return strings.Join(out, " ")
	}
	both := apitest.String(t, first, "id") + " " + apitest.String(t, second, "id")
	cases := []struct{ name, query, want string }{
		{"every proposal", "", both},
		{"of the close", "close_id=" + a.close, both},
		{"of another close", "close_id=" + uuid.NewString(), ""},
		{"of the company", "legal_entity_id=" + a.DE.String(), both},
		{"of another company", "legal_entity_id=" + a.US.String(), ""},
		{"of the session", "session_id=" + a.session, both},
		{"of the parked entry", "parked_entry_id=" + apitest.String(t, second, "parked_entry_id"), apitest.String(t, second, "id")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(a.Client, tc.query); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	page, _ := a.Must(200, "GET", "/v1/accrual-proposals?page_size=1", "", "")
	next, _ := page["next_page_token"].(string)
	if got := ids(a.Client, "page_size=1&page_token="+next); got != apitest.String(t, second, "id") {
		t.Errorf("the second page: got %s, want the second proposal", got)
	}
	if got := ids(a.As(a.Reader(t, "Reader of 1000", a.US)), ""); got != "" {
		t.Errorf("a reader of company 1000: got %s, want none", got)
	}
	a.Tenant.Revoke(t, "partner.supplier.read", a.Tenant.Human)
	got, _ := a.Must(200, "GET", "/v1/accrual-proposals/"+apitest.String(t, first, "id"), "", "")
	ev := apitest.List(t, got, "evidence")[0].(map[string]any)
	wantFields(t, "without partner.supplier.read", got, map[string]any{"supplier_id": a.Supplier.String(), "supplier_number": nil, "supplier_name": nil})
	wantFields(t, "its evidence without partner.supplier.read", ev, map[string]any{"supplier_id": a.Supplier.String(), "supplier_number": nil, "supplier_name": nil})
}
