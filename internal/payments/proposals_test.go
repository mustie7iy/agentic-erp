package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// proposalPerms are what the agent of the proposal tests holds besides
// reading statements: its sessions and proposing.
var proposalPerms = []string{"payments.statement.propose", "agents.session.record", "agents.session.read"}

// proposalScenario is the matching scenario with the agent allowed to
// propose, and one running session of the agent.
func proposalScenario(t *testing.T, more ...string) (matchScenario, string) {
	t.Helper()
	e := matchSetupWith(t, append(append([]string{}, proposalPerms...), more...)...)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	sc := matchStatementOf(t, e)
	return sc, e.session(t)
}

// session opens a running session of the tenant's agent.
func (e env) session(t *testing.T) string {
	t.Helper()
	s, _ := e.agent().Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Reconcile statement lines", "model": "erp-default"}`, "")
	return id(s)
}

// propose has the agent propose for a line in the session, on any
// version of the statement; the answer's status, body and ETag.
func (sc matchScenario) propose(t *testing.T, line int, session, body string) (int, map[string]any, string) {
	t.Helper()
	return sc.proposeIf(t, line, session, body, "*")
}

// proposeIf is propose with the If-Match given.
func (sc matchScenario) proposeIf(t *testing.T, line int, session, body, ifMatch string) (int, map[string]any, string) {
	t.Helper()
	e := sc.e.agent()
	r := e.API.Do(apitest.Req{Method: "POST", Path: sc.line(line, "propose"), Tenant: e.Tenant.ID, As: e.Client.As, Key: apitest.NewKey(),
		IfMatch: ifMatch, Body: `{"session_id": "` + session + `", ` + body + `}`})
	return r.Code, apitest.JSON(t, r), r.Header().Get("ETag")
}

// TestProposeStatementLines: the agent proposes for a line the rules left
// open or unmatched the payments it clears, a posting rule or its
// exclusion; the line is proposed, the rule's open match rejected, and the
// rules leave the proposal; the same proposal again in the session is the
// first, another is refused (ADR-0047, T49g).
func TestProposeStatementLines(t *testing.T) {
	sc, session := proposalScenario(t)
	e, pays := sc.e, sc.pays
	before := sc.tag(t)
	code, m, tag := sc.propose(t, 4, session, `"payment_ids": ["`+pays[2].payment+`"], "reason": "The remittance names INV-3."`)
	if code != 201 {
		t.Fatalf("propose: got %d %v, want 201", code, m)
	}
	ms := e.matchesOf(t, sc.statement, 4)
	ev := events(t, e.Tenant, "bank_statement_line_proposed")
	checks{
		"the proposal's kind":             {m["kind"], "agent"},
		"the proposal's status":           {m["status"], "open"},
		"what it proposes":                {m["proposal"], "match"},
		"its session":                     {m["session_id"], session},
		"its targets":                     {strings.Join(targetsOf(m), ""), "1:" + pays[2].payment},
		"the ETag, the statement's":       {tag, sc.tag(t)},
		"the ETag, moved on":              {tag != before, true},
		"line 4's matches":                {len(ms), 2},
		"the rule's match":                {field(ms[0], "status"), "rejected"},
		"the rule's match has no outcome": {field(ms[0], "outcome"), nil},
		"line 4":                          {e.lineStatuses(t, sc.statement)[3], "proposed"},
		"proposed events":                 {len(ev), 1},
		"the event's proposal":            {ev[0]["proposal"], "match"},
		"the event's session":             {ev[0]["session_id"], session},
	}.check(t)
	t.Run("the same again, on the ETag read before", func(t *testing.T) {
		code, again, againTag := sc.proposeIf(t, 4, session, `"payment_ids": ["`+pays[2].payment+`"], "reason": "Said again."`, before)
		checks{"status": {code, 200}, "the first proposal": {id(again), id(m)}, "the ETag": {againTag, tag}}.check(t)
	})
	t.Run("a statement changed since it was read", func(t *testing.T) {
		if code, body, _ := sc.proposeIf(t, 5, session, `"exclude": true, "reason": "x"`, before); code != 412 {
			t.Errorf("got %d %v, want 412", code, body)
		}
	})
	_, after := e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	xCode, x, _ := sc.propose(t, 6, session, `"exclude": true, "reason": "Interest from another bank's correction."`)
	rCode, r, _ := sc.propose(t, 5, session, `"posting_rule": "NCHG", "reason": "Charges."`)
	againCode, again, _ := sc.propose(t, 5, session, `"posting_rule": "NCHG", "reason": "Charges."`)
	checks{
		"the rules leave the proposal":       {sc.live(t, 4)["kind"], "agent"},
		"the rules change nothing":           {after, tag},
		"an exclusion proposed":              {xCode, 201},
		"the exclusion's proposal":           {x["proposal"], "exclude"},
		"the exclusion's targets":            {len(x["targets"].([]any)), 0},
		"a posting rule proposed":            {rCode, 201},
		"the posting rule's targets":         {strings.Join(targetsOf(r), ""), "1:NCHG"},
		"the posting rule again":             {againCode, 200},
		"the posting rule again, the first":  {id(again), id(r)},
		"matched events, the import's alone": {len(events(t, e.Tenant, "bank_statement_line_matched")), 6},
	}.check(t)
}

// checks are values and what each should be, checked as subtests.
type checks map[string][2]any

func (cs checks) check(t *testing.T) {
	t.Helper()
	for name, c := range cs {
		t.Run(name, func(t *testing.T) {
			if c[0] != c[1] {
				t.Errorf("got %v, want %v", c[0], c[1])
			}
		})
	}
}

// TestProposalRefusals: a proposal names one thing the line may clear (or
// its exclusion), for a line that takes one, in the agent's running
// session; a session proposes once per line.
func TestProposalRefusals(t *testing.T) {
	sc, session := proposalScenario(t)
	e, pays := sc.e, sc.pays
	if code, body, _ := sc.propose(t, 4, session, `"payment_ids": ["`+pays[2].payment+`"], "reason": "x"`); code != 201 {
		t.Fatalf("propose: got %d %v", code, body)
	}
	other, ended := e.session(t), e.session(t)
	e.agent().Must(200, "POST", "/v1/agent-sessions/"+ended+":end", `{"status": "completed", "input_tokens": 0, "output_tokens": 0}`, "*")
	pgtest.Exec(t, `UPDATE payments.bank_statement_line SET status = 'pending_approval', version = version + 1 WHERE statement_id = $1 AND line_no = 3`, sc.statement)
	for name, c := range map[string]struct {
		status  int
		code    string
		line    int
		session string
		body    string
	}{
		"otherwise in the session":         {409, "payments.statement_line_proposed", 4, session, `"payment_ids": ["` + pays[3].payment + `"], "reason": "x"`},
		"another session's proposal":       {409, "payments.statement_line_proposed", 4, other, `"payment_ids": ["` + pays[2].payment + `"], "reason": "x"`},
		"a matched line":                   {409, "payments.statement_line_not_open", 1, session, `"exclude": true, "reason": "x"`},
		"a line on its way to the ledger":  {409, "payments.statement_line_not_open", 3, session, `"exclude": true, "reason": "x"`},
		"no such line":                     {404, "request.not_found", 99, session, `"exclude": true, "reason": "x"`},
		"a payment matched to another":     {422, "payments.statement_match_target", 5, session, `"payment_ids": ["` + pays[0].payment + `"], "reason": "x"`},
		"another company's posting rule":   {422, "payments.statement_match_target", 5, session, `"posting_rule": "INTR", "reason": "x"`},
		"nothing proposed":                 {422, "request.validation_failed", 5, session, `"exclude": false, "reason": "x"`},
		"two things proposed":              {422, "request.validation_failed", 5, session, `"exclude": true, "posting_rule": "NCHG", "reason": "x"`},
		"an ended session":                 {409, "agents.session_ended", 5, ended, `"exclude": true, "reason": "x"`},
		"a session that is none":           {422, "request.reference_not_found", 5, uuid.NewString(), `"exclude": true, "reason": "x"`},
		"a statement that is none (agent)": {404, "request.not_found", 5, session, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if c.body == "" {
				e.in(t).agent().Problem(c.status, c.code, "POST", fmt.Sprintf("/v1/bank-statements/%s/lines/5:propose", uuid.NewString()),
					`{"session_id": "`+c.session+`", "exclude": true, "reason": "x"}`, "*")
				return
			}
			got, body, _ := sc.propose(t, c.line, c.session, c.body)
			checks{"status": {got, c.status}, "code": {body["code"], c.code}}.check(t)
		})
	}
	t.Run("a person, who has no session", func(t *testing.T) {
		e.in(t).Problem(403, "agents.not_your_session", "POST", sc.line(5, "propose"), `{"session_id": "`+session+`", "exclude": true, "reason": "x"}`, "*")
	})
}

// TestAcceptProposal: a person accepts the agent's proposal as the
// person's own match by it, which the person posts and cannot approve; an
// exclusion accepted excludes the line (ADR-0047, T49g).
func TestAcceptProposal(t *testing.T) {
	sc := postScenario(t, proposalPerms...)
	e, pays := sc.e, sc.pays
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	session := e.session(t)
	if code, body, _ := sc.propose(t, 4, session, `"payment_ids": ["`+pays[2].payment+`"], "reason": "The remittance names INV-3."`); code != 201 {
		t.Fatalf("propose: got %d %v", code, body)
	}
	e.agent().Problem(403, "identity.agent_not_allowed", "POST", sc.line(4, "acceptProposal"), "", sc.tag(t))
	m, tag := e.As(e.Approver).Must(201, "POST", sc.line(4, "acceptProposal"), `{"comment": "Checked."}`, sc.tag(t))
	againCode, again, _ := sc.propose(t, 4, session, `"payment_ids": ["`+pays[2].payment+`"], "reason": "Retried."`)
	p := e.matchesOf(t, sc.statement, 4)[1]
	ev := events(t, e.Tenant, "bank_statement_line_proposal_decided")
	checks{
		"the match's kind":                 {m["kind"], "manual"},
		"the match's status":               {m["status"], "matched"},
		"the match, the accepter's":        {m["created_by"], e.Approver.ID.String()},
		"the match's targets":              {strings.Join(targetsOf(m), ""), "1:" + pays[2].payment},
		"the match's reason":               {m["reason"], "Accepted the agent's proposal: The remittance names INV-3. Checked."},
		"the proposal again once accepted": {againCode, 200},
		"the proposal again, as accepted":  {again["outcome"], "accepted"},
		"the proposal's status":            {field(p, "status"), "rejected"},
		"the proposal's outcome":           {field(p, "outcome"), "accepted"},
		"decided events":                   {len(ev), 1},
		"the decided event's outcome":      {ev[0]["outcome"], "accepted"},
		"the decided event's session":      {ev[0]["session_id"], session},
	}.check(t)
	posted, _ := e.Must(200, "POST", sc.line(4, "post"), "", tag)
	r := e.Pending(t, id(posted))
	e.As(e.Approver).Problem(403, "workflow.segregation_of_duties", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	e.approve(t, e.Approver2, id(posted))
	if got := e.lineStatuses(t, sc.statement)[3]; got != "posted" {
		t.Errorf("line 4: got %s, want posted", got)
	}
	if code, body, _ := sc.propose(t, 6, session, `"exclude": true, "reason": "Interest from another bank's correction."`); code != 201 {
		t.Fatalf("propose an exclusion: got %d %v", code, body)
	}
	x, _ := e.Must(201, "POST", sc.line(6, "acceptProposal"), "", sc.tag(t))
	if code, body, _ := sc.propose(t, 7, session, `"posting_rule": "NCHG", "reason": "Charges reversed."`); code != 201 {
		t.Fatalf("propose a posting rule: got %d %v", code, body)
	}
	rule, _ := e.Must(201, "POST", sc.line(7, "acceptProposal"), "", sc.tag(t))
	checks{
		"an exclusion accepted, the kind":   {x["kind"], "manual"},
		"an exclusion accepted, the status": {x["status"], "excluded"},
		"an exclusion accepted, the reason": {x["reason"], "Accepted the agent's proposal: Interest from another bank's correction."},
		"a posting rule accepted":           {rule["status"], "matched"},
		"a posting rule accepted, its rule": {strings.Join(targetsOf(rule), ""), "1:NCHG"},
	}.check(t)
}

// TestRejectProposal: a person rejects the agent's proposal with a reason
// and the line is unmatched again; a person's own match overrides one; a
// decision needs a proposal, and one whose payment another line took
// since is refused (ADR-0047, T49g).
func TestRejectProposal(t *testing.T) {
	sc, session := proposalScenario(t)
	e, pays := sc.e, sc.pays
	for _, c := range []struct {
		line int
		body string
	}{{4, `"payment_ids": ["` + pays[2].payment + `"]`}, {5, `"posting_rule": "NCHG"`}, {6, `"exclude": true`}} {
		if code, body, _ := sc.propose(t, c.line, session, c.body+`, "reason": "x"`); code != 201 {
			t.Fatalf("propose for line %d: got %d %v", c.line, code, body)
		}
	}
	e.agent().Problem(403, "identity.agent_not_allowed", "POST", sc.line(5, "rejectProposal"), `{"reason": "x"}`, sc.tag(t))
	r, tag := e.Must(200, "POST", sc.line(5, "rejectProposal"), `{"reason": "Not a charge: a refund."}`, sc.tag(t))
	checks{
		"the proposal's kind":    {r["kind"], "agent"},
		"the proposal's status":  {r["status"], "rejected"},
		"the proposal's outcome": {r["outcome"], "rejected"},
		"the statement's ETag":   {tag, sc.tag(t)},
	}.check(t)
	_, tag = e.Must(201, "POST", sc.line(6, "match"), `{"posting_rule": "NCHG", "reason": "Charges."}`, tag)
	if got := field(e.matchesOf(t, sc.statement, 6)[0], "outcome"); got != "overridden" {
		t.Errorf("line 6's proposal after the person's match: got %v, want overridden", got)
	}
	want := []string{"matched", "matched", "matched", "proposed", "unmatched", "matched", "unmatched", "proposed", "proposed"}
	if got := e.lineStatuses(t, sc.statement); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("line statuses: got %v, want %v", got, want)
	}
	var outcomes []string
	for _, ev := range events(t, e.Tenant, "bank_statement_line_proposal_decided") {
		outcomes = append(outcomes, ev["outcome"].(string))
	}
	if strings.Join(outcomes, ",") != "rejected,overridden" {
		t.Errorf("decided events: got %v, want rejected, overridden", outcomes)
	}
	var reason string
	pgtest.Scan(t, `SELECT detail->>'reason' FROM audit.audit_event WHERE object_id = $1 AND action = 'payments.statement_match.reject'`, []any{id(r)}, &reason)
	if reason != "Not a charge: a refund." {
		t.Errorf("audited reason: got %q", reason)
	}
	_, tag = e.Must(201, "POST", sc.line(8, "match"), `{"payment_ids": ["`+pays[2].payment+`"], "reason": "x"}`, tag) // takes line 4's proposed payment
	for name, c := range map[string]struct {
		status             int
		code, action, body string
		line               int
	}{
		"a proposal whose payment was taken": {422, "payments.statement_match_target", "acceptProposal", "", 4},
		"accepting without a proposal":       {409, "payments.statement_line_no_proposal", "acceptProposal", "", 5},
		"rejecting without a proposal":       {409, "payments.statement_line_no_proposal", "rejectProposal", `{"reason": "x"}`, 7},
		"rejecting a proposal overridden":    {409, "payments.statement_line_no_proposal", "rejectProposal", `{"reason": "x"}`, 6},
	} {
		t.Run(name, func(t *testing.T) { e.in(t).Problem(c.status, c.code, "POST", sc.line(c.line, c.action), c.body, tag) })
	}
}

// first is the first item of a list the client reads.
func first(c apitest.Client, path string) map[string]any {
	list, _ := c.Must(200, "GET", path, "", "")
	return list["items"].([]any)[0].(map[string]any)
}

// transaction is a line's first transaction.
func transaction(s map[string]any, line int) map[string]any {
	return field(s, "lines").([]any)[line-1].(map[string]any)["transactions"].([]any)[0].(map[string]any)
}

// TestStatementsMaskedForAgents: an agent sees the statement's account and
// the counterparties' accounts by their last four characters, as a list,
// a statement and a run of the rules answer them; a person sees them
// whole, and both the last four.
func TestStatementsMaskedForAgents(t *testing.T) {
	sc := matchStatementSetup(t)
	e := sc.e
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	person, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	agent, _ := e.agent().Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	run, _ := e.agent().Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	for name, c := range map[string]struct{ got, want any }{
		"the person's account":          {person["account"], "DE89370400440532013000"},
		"the agent's account":           {agent["account"], "****3000"},
		"the agent's list":              {first(e.agent().Client, "/v1/bank-statements")["account"], "****3000"},
		"the agent's run of the rules":  {run["account"], "****3000"},
		"the person's counterparty":     {transaction(person, 5)["counterparty_iban"], "GB33BUKB20201555555555"},
		"the agent's counterparty":      {transaction(agent, 5)["counterparty_iban"], nil},
		"the person's last four":        {transaction(person, 5)["counterparty_account_last4"], "5555"},
		"the agent's last four":         {transaction(agent, 5)["counterparty_account_last4"], "5555"},
		"no counterparty: no last four": {transaction(person, 1)["counterparty_account_last4"], nil},
	} {
		t.Run(name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
}

// TestListHouseBanks: the house banks of the companies the caller may access,
// by legal entity, the account by its last four characters.
func TestListHouseBanks(t *testing.T) {
	e, _ := statementSetup(t)
	bank := first(e.Client, "/v1/house-banks?legal_entity_id="+e.DE.String())
	for name, c := range map[string]struct{ got, want any }{
		"id":       {bank["id"], e.bank.String()},
		"code":     {bank["code"], "MAIN"},
		"currency": {bank["currency"], "EUR"},
		"last 4":   {bank["account_last4"], "3000"},
		"status":   {bank["status"], "active"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
	list, _ := e.Must(200, "GET", "/v1/house-banks?legal_entity_id="+e.US.String(), "", "")
	if got := len(list["items"].([]any)); got != 1 {
		t.Errorf("company 1000's house banks: got %d, want its one", got)
	}
	page, _ := e.Must(200, "GET", "/v1/house-banks?page_size=1", "", "")
	next, _ := e.Must(200, "GET", "/v1/house-banks?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	checks{
		"two pages of one, each its bank": {pageFirstID(page) != pageFirstID(next), true},
		"the last page":                   {next["next_page_token"], nil},
	}.check(t)
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/house-banks?order_by=iban", "", "")
}

// TestProposalDatabaseFailures: every statement of proposing, accepting
// and rejecting fails in turn, and none runs outside a unit of work.
func TestProposalDatabaseFailures(t *testing.T) {
	sc, session := proposalScenario(t)
	e, pays := sc.e, sc.pays
	for _, c := range []struct {
		line int
		body string
	}{{4, `"payment_ids": ["` + pays[2].payment + `"]`}, {6, `"exclude": true`}} {
		if code, body, _ := sc.propose(t, c.line, session, c.body+`, "reason": "x"`); code != 201 {
			t.Fatalf("propose for line %d: got %d %v", c.line, code, body)
		}
	}
	h := endpoints(nil)
	h.Objects = e.objects
	st, sid := uuidOf(sc.statement), uuidOf(session)
	propose := func(line int32, in payments.LineProposal) func(ctx context.Context) error {
		return func(ctx context.Context) error { _, _, err := h.ProposeLine(ctx, st, line, anyVersion, in); return err }
	}
	for name, c := range map[string]struct {
		as pgtest.Actor
		fn func(ctx context.Context) error
	}{
		"propose":           {e.Tenant.Agent, propose(5, payments.LineProposal{SessionID: sid, PostingRule: "NCHG", Reason: "x"})},
		"propose again":     {e.Tenant.Agent, propose(4, payments.LineProposal{SessionID: sid, PaymentIDs: []uuid.UUID{uuidOf(pays[2].payment)}, Reason: "x"})},
		"propose exclusion": {e.Tenant.Agent, propose(5, payments.LineProposal{SessionID: sid, Exclude: true, Reason: "x"})},
		"accept":            {e.Tenant.Human, func(ctx context.Context) error { _, err := h.AcceptProposal(ctx, st, 4, anyVersion, ""); return err }},
		"accept exclusion":  {e.Tenant.Human, func(ctx context.Context) error { _, err := h.AcceptProposal(ctx, st, 6, anyVersion, ""); return err }},
		"reject":            {e.Tenant.Human, func(ctx context.Context) error { _, err := h.RejectProposal(ctx, st, 4, anyVersion, "x"); return err }},
		"propose handler": {e.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.ProposeBankStatementLine(ctx, &gen.StatementLineProposalInput{SessionID: sid, PostingRule: gen.NewOptString("NCHG"), Reason: "x"},
				gen.ProposeBankStatementLineParams{ID: st, LineNo: 5, IfMatch: "*"})
			return err
		}},
		"propose again handler": {e.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.ProposeBankStatementLine(ctx, &gen.StatementLineProposalInput{SessionID: sid, Exclude: gen.NewOptBool(true), Reason: "x"},
				gen.ProposeBankStatementLineParams{ID: st, LineNo: 6, IfMatch: "*"})
			return err
		}},
		"accept handler": {e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.AcceptBankStatementLineProposal(ctx, gen.OptStatementLineProposalAccept{}, gen.AcceptBankStatementLineProposalParams{ID: st, LineNo: 6, IfMatch: "*"})
			return err
		}},
		"reject handler": {e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.RejectBankStatementLineProposal(ctx, &gen.ReasonInput{Reason: "x"}, gen.RejectBankStatementLineProposalParams{ID: st, LineNo: 6, IfMatch: "*"})
			return err
		}},
		"house banks": {e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListHouseBanks(ctx, gen.ListHouseBanksParams{})
			return err
		}},
		"rules": {e.Tenant.Agent, func(ctx context.Context) error {
			_, err := h.GetBankStatementRules(ctx, gen.GetBankStatementRulesParams{ID: st})
			return err
		}},
	} {
		apitest.EveryFailure(t, e.Tenant, c.as, name, c.fn)
	}
	for name, err := range map[string]error{
		"propose":     propose(5, payments.LineProposal{SessionID: sid, Exclude: true, Reason: "x"})(context.Background()),
		"accept":      func() error { _, err := h.AcceptProposal(context.Background(), st, 4, anyVersion, ""); return err }(),
		"reject":      func() error { _, err := h.RejectProposal(context.Background(), st, 4, anyVersion, "x"); return err }(),
		"house banks": func() error { _, err := h.ListHouseBanks(context.Background(), gen.ListHouseBanksParams{}); return err }(),
		"rules": func() error {
			_, err := h.GetBankStatementRules(context.Background(), gen.GetBankStatementRulesParams{ID: st})
			return err
		}(),
	} {
		t.Run(name, func(t *testing.T) { pgtest.WantErr(t, err, "") })
	}
}

// pageFirstID is the id of a page's first item.
func pageFirstID(page map[string]any) string { return id(page["items"].([]any)[0].(map[string]any)) }

// TestStatementRules: the posting and matching rules of the statement's
// company, in their order, as the agent reads them; none for a company
// the configuration does not have.
func TestStatementRules(t *testing.T) {
	sc := matchStatementSetup(t)
	e := sc.e
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	r, _ := e.agent().Must(200, "GET", "/v1/bank-statements/"+sc.statement+"/rules", "", "")
	posting, matching := r["posting_rules"].([]any), r["matching_rules"].([]any)
	pgtest.Exec(t, `UPDATE tenancy.legal_entity SET code = '2999', version = version + 1 WHERE id = $1`, e.DE)
	none, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement+"/rules", "", "")
	checks{
		"posting rules":                   {len(posting), 2},
		"the first's code":                {field(posting[0], "transaction_code"), "NCHG"},
		"the first's account":             {field(posting[0], "account"), "520000"},
		"matching rules":                  {len(matching), 4},
		"the first, in order":             {field(matching[0], "kind"), "end_to_end_id"},
		"no window but the amount rule's": {field(matching[0], "date_window_days"), nil},
		"the amount rule's tolerance":     {field(matching[3], "amount_tolerance"), "0.50"},
		"the amount rule's window":        {field(matching[3], "date_window_days"), float64(3)},
		"the amount rule's counterparty":  {field(matching[3], "counterparty"), true},
		"no company: no posting rules":    {len(none["posting_rules"].([]any)), 0},
		"no company: no matching rules":   {len(none["matching_rules"].([]any)), 0},
	}.check(t)
	e.Problem(404, "request.not_found", "GET", "/v1/bank-statements/"+uuid.NewString()+"/rules", "", "")
}
