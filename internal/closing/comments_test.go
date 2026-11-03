package closing_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// commentPermissions are Carla's, who writes and decides comments, and
// the agent's (the machine role holds every one of them that is no
// approval), who proposes them in its sessions.
var commentPermissions = append(append([]string{}, closePermissions...), "closing.variance.propose", "closing.variance.comment", "agents.session.record")

// withComments is withVariances with a check task: every variance
// requiring a comment explained.
func withComments(doc *config.Config) {
	withVariances(doc)
	doc.Close.Tasks = append(doc.Close.Tasks, config.CloseTask{Code: "EXPLAINED", Name: "Variances explained", Kind: config.CloseCheck,
		Check: "variances_explained", DueDay: 4})
}

// commenting is a close of company 2000's January 2026 under test, over
// postedPeriods' entries, with an agent session.
type commenting struct {
	fixture.Env
	close, session string
	agent          apitest.Client
}

func newCommenting(t *testing.T) *commenting {
	t.Helper()
	e := fixture.NewWith(t, nil, withComments, commentPermissions...)
	postedPeriods(t, e)
	jan, _ := open(t, e.Client, e.DE, 1, "")
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	c := &commenting{Env: e, close: apitest.String(t, jan, "id"), agent: e.As(e.Tenant.Agent)}
	s, _ := c.agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Explain the variances", "model": "erp-default"}`, "")
	c.session = apitest.String(t, s, "id")
	return c
}

// entryOn is the id of company 2000's journal entry of the year and
// period with a line on the account, as a consumer finds it (GET
// /v1/journal-entries); the fixture posts one per period.
func (c *commenting) entryOn(t *testing.T, year, period int, account string) string {
	t.Helper()
	page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&fiscal_year=%d&fiscal_period=%d&account_id=%s", c.DE, year, period,
		c.Account(t, "2000", account)), "", "")
	items := apitest.List(t, page, "items")
	if len(items) != 1 {
		t.Fatalf("entries of %d/%d on %s: got %d, want 1", year, period, account, len(items))
	}
	return items[0].(map[string]any)["id"].(string)
}

// body is a comment's body on the account's row against the basis, with
// the citations; an agent's names its session.
func (c *commenting) body(t *testing.T, basis, account, text string, citations []string, session string) string {
	t.Helper()
	quoted := make([]string, len(citations))
	for i, id := range citations {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	b := fmt.Sprintf(`"close_id": %q, "basis": %q, "account_id": %q, "text": %q, "citations": [%s]`, c.close, basis, c.Account(t, "2000", account), text,
		strings.Join(quoted, ", "))
	if session != "" {
		b += fmt.Sprintf(`, "session_id": %q`, session)
	}
	return "{" + b + "}"
}

// commentLine is a comment as one line: its row, figures, status, author
// and decider, whether it is edited and stale, and its citations.
func commentLine(t *testing.T, m map[string]any) string {
	t.Helper()
	amount := func(k string) any { return m[k].(map[string]any)["amount"] }
	var cited []string
	for _, x := range apitest.List(t, m, "citations") {
		ci := x.(map[string]any)
		cited = append(cited, fmt.Sprintf("%v/%v", ci["fiscal_year"], ci["posting_date"]))
	}
	return fmt.Sprintf("%v %v %v %v/%v %v: %v %v %v %v by %v (%v) decided by %v edited=%v stale=%v %q cites %v", m["company_code"], m["basis"],
		m["account_number"], m["fiscal_year"], m["fiscal_period"], m["account_name"], amount("actual"), amount("comparison"), amount("variance"),
		m["status"], m["author_name"], m["author_kind"], m["decided_by_name"], m["edited"], m["stale"], m["text"], cited)
}

// row is the report's row of the account against the basis: its
// accepted comment's text and staleness, whether it is explained and how
// many proposals wait; the report's explained rows last.
func (c *commenting) row(t *testing.T, basis, number string) string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/period-closes/"+c.close+"/variances?basis="+basis, "", "")
	for _, x := range apitest.List(t, page, "items") {
		r := x.(map[string]any)
		if r["account_number"] != number {
			continue
		}
		note := "none"
		if k, ok := r["comment"].(map[string]any); ok {
			note = fmt.Sprintf("%q by %v accepted by %v stale=%v", k["text"], k["author_name"], k["decided_by_name"], k["stale"])
		}
		return fmt.Sprintf("explained=%v proposed=%v comment %s; %v explained", r["explained"], r["proposed_comments"], note,
			page["totals"].(map[string]any)["rows_explained"])
	}
	t.Fatalf("no row %s against %s", number, basis)
	return ""
}

// check runs the close's variances_explained check and returns its
// result as one line: passed, the count and each row named.
func (c *commenting) check(t *testing.T) string {
	t.Helper()
	_, tag := c.Must(200, "GET", "/v1/period-closes/"+c.close, "", "")
	task, _ := c.Must(200, "POST", "/v1/period-closes/"+c.close+"/tasks/EXPLAINED:check", "", tag)
	r := task["check_result"].(map[string]any)
	out := fmt.Sprintf("%v %v: %v |", task["status"], r["passed"], r["unexplained_rows_count"])
	for _, x := range apitest.List(t, r, "unexplained_rows") {
		u := x.(map[string]any)
		out += fmt.Sprintf(" %v %v %v %v stale=%v;", u["basis"], u["account_number"], u["account_name"], u["variance"].(map[string]any)["amount"], u["stale_comment"])
	}
	return out
}

// TestVarianceComments: the agent proposes a comment on a flagged row
// citing the entries its figures are made of, in its session; the same
// proposal again answers it, another is refused. A person accepts it with
// a text of her own, which explains the row; the check passes once every
// flagged row of each basis is explained. A comment written by a person
// supersedes the row's accepted one; an entry posted since makes it
// stale, and the row is unexplained again.
func TestVarianceComments(t *testing.T) {
	c := newCommenting(t)
	jan26, dec25 := c.entryOn(t, 2026, 1, "520000"), c.entryOn(t, 2025, 12, "520000")
	proposal := c.body(t, "prior_period", "520000", "More office supplies bought in January.", []string{jan26, dec25}, c.session)
	p, tag := c.agent.Must(201, "POST", "/v1/variance-comment-proposals", proposal, "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	want := `2000 prior_period 520000 2026/1 Office Supplies: 1500.00 1000.00 500.00 proposed by agent (agent) decided by <nil> edited=false stale=false ` +
		`"More office supplies bought in January." cites [2026/2026-01-20 2025/2025-12-10]`
	if got := commentLine(t, p); got != want {
		t.Errorf("proposal:\ngot  %s\nwant %s", got, want)
	}
	again, _ := c.agent.Must(200, "POST", "/v1/variance-comment-proposals", proposal, "")
	if again["id"] != p["id"] {
		t.Errorf("the same proposal again: got %v, want %v", again["id"], p["id"])
	}
	c.agent.Problem(409, "closing.comment_already_proposed", "POST", "/v1/variance-comment-proposals",
		c.body(t, "prior_period", "520000", "Another reason.", []string{jan26}, c.session), "")
	if got := c.row(t, "prior_period", "520000"); got != "explained=false proposed=1 comment none; 0 explained" {
		t.Errorf("the row with a proposal: got %s", got)
	}
	want = "open false: 4 | prior_period 520000 Office Supplies 500.00 stale=false; prior_year 400000 Sales Revenue -500.00 stale=false;" +
		" prior_year 510000 Raw Materials Consumed 5000.00 stale=false; prior_year 520000 Office Supplies 1300.00 stale=false;"
	if got := c.check(t); got != want {
		t.Errorf("the check before any comment is accepted:\ngot  %s\nwant %s", got, want)
	}

	path := "/v1/variance-comments/" + apitest.String(t, p, "id")
	c.Problem(412, "request.version_mismatch", "POST", path+":acceptProposal", "", `"2"`)
	accepted, tag := c.Must(200, "POST", path+":acceptProposal", `{"text": "Office Supplies for the new team, bought in January."}`, `"1"`)
	wantFields(t, "accepted", accepted, map[string]any{"status": "accepted", "edited": true, "proposed_text": "More office supplies bought in January.",
		"text": "Office Supplies for the new team, bought in January.", "decided_by_name": "human"})
	if tag != `"2"` {
		t.Errorf("ETag after the acceptance: got %s, want \"2\"", tag)
	}
	c.Problem(409, "closing.comment_not_proposed", "POST", path+":acceptProposal", "", tag)
	c.Problem(409, "closing.comment_not_proposed", "POST", path+":rejectProposal", `{"reason": "No"}`, tag)
	if got, want := c.row(t, "prior_period", "520000"), `explained=true proposed=0 comment "Office Supplies for the new team, bought in January." `+
		`by agent accepted by human stale=false; 1 explained`; got != want {
		t.Errorf("the row explained:\ngot  %s\nwant %s", got, want)
	}

	jan25 := c.entryOn(t, 2025, 1, "520000")
	for _, row := range []struct {
		number string
		cites  []string
	}{{"400000", []string{jan26}}, {"510000", nil}} {
		c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", row.number, "A first year of trading.", row.cites, ""), "")
	}
	if got, want := c.check(t), "open false: 1 | prior_year 520000 Office Supplies 1300.00 stale=false;"; got != want {
		t.Errorf("the check with one row left:\ngot  %s\nwant %s", got, want)
	}
	c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", "520000", "A first year of trading.", []string{jan25, jan26}, ""), "")
	if got := c.check(t); got != "done true: 0 |" {
		t.Errorf("the check with every flagged row explained: got %s", got)
	}

	written, _ := c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_period", "520000", "Supplies for the new team.", nil, ""), "")
	wantFields(t, "a person's comment", written, map[string]any{"status": "accepted", "author_kind": "human", "decided_by_name": "human", "edited": false})
	before, _ := c.Must(200, "GET", path, "", "")
	wantFields(t, "the comment accepted before", before, map[string]any{"status": "superseded", "stale": false})

	post(t, c.Env, "2026-01-25", map[string]money.Minor{"520000": 10000})
	if got, want := c.row(t, "prior_period", "520000"), `explained=false proposed=0 comment "Supplies for the new team." by human accepted by human stale=true; 0 explained`; got != want {
		t.Errorf("the row after an entry posted since:\ngot  %s\nwant %s", got, want)
	}
	now, _ := c.Must(200, "GET", "/v1/variance-comments/"+apitest.String(t, written, "id"), "", "")
	wantFields(t, "the comment after an entry posted since", now, map[string]any{"stale": true})
	page, _ := c.Must(200, "GET", "/v1/period-closes/"+c.close+"/variances?basis=prior_period&unexplained=true&requires_comment=true", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 1 || items[0].(map[string]any)["account_number"] != "520000" {
		t.Errorf("the unexplained flagged rows: got %v, want 520000's alone", items)
	}
	page, _ = c.Must(200, "GET", "/v1/period-closes/"+c.close+"/variances?basis=prior_period&unexplained=false", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("the explained rows against the prior period: got %v, want none", items)
	}
	want = "open false: 2 | prior_period 520000 Office Supplies 600.00 stale=true; prior_year 520000 Office Supplies 1400.00 stale=true;"
	if got := c.check(t); got != want {
		t.Errorf("the check with a stale comment:\ngot  %s\nwant %s", got, want)
	}

	changes := map[string]int{}
	for _, ev := range apitest.Published(t, c.Tenant, outbox.Event{Context: "closing", Name: "variance_comment_changed"}) {
		changes[fmt.Sprint(ev["change"], " ", ev["author_kind"], " edited=", ev["edited"])]++
	}
	wantEvents := map[string]int{"proposed agent edited=false": 1, "accepted agent edited=true": 1, "accepted human edited=false": 4,
		"superseded agent edited=true": 1}
	if fmt.Sprint(changes) != fmt.Sprint(wantEvents) {
		t.Errorf("events: got %v, want %v", changes, wantEvents)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'closing.variance_comment.%'`, []any{c.Tenant.ID}, &audited)
	if audited != 7 {
		t.Errorf("audit records: got %d, want 7, one per change", audited)
	}
}

// TestVarianceCommentRejected: a person rejects a proposal with a reason;
// the row stays unexplained, the agent's next session proposes again.
func TestVarianceCommentRejected(t *testing.T) {
	c := newCommenting(t)
	p, tag := c.agent.Must(201, "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Prices rose.", nil, c.session), "")
	path := "/v1/variance-comments/" + apitest.String(t, p, "id")
	c.Problem(422, "request.validation_failed", "POST", path+":rejectProposal", `{"reason": "  "}`, tag)
	c.Problem(422, "request.validation_failed", "POST", path+":acceptProposal", `{"text": "  "}`, tag)
	got, _ := c.Must(200, "POST", path+":rejectProposal", `{"reason": "Prices did not rise; volumes did."}`, tag)
	wantFields(t, "rejected", got, map[string]any{"status": "rejected", "decision_reason": "Prices did not rise; volumes did.", "decided_by_name": "human"})
	if row := c.row(t, "prior_period", "520000"); row != "explained=false proposed=0 comment none; 0 explained" {
		t.Errorf("the row after the rejection: got %s", row)
	}
	s, _ := c.agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Explain again", "model": "erp-default"}`, "")
	c.agent.Must(201, "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Volumes rose.", nil, apitest.String(t, s, "id")), "")
}

// TestVarianceCommentRefusals: a comment explains a row of the report
// against a basis the company configures, citing entries with a line on
// the row's account in the periods it compares; only an agent proposes,
// in its running session, and only a person writes, accepts or rejects;
// a person who may not act in the company does not find the close.
func TestVarianceCommentRefusals(t *testing.T) {
	c := newCommenting(t)
	jan26, dec25, jan25, feb26 := c.entryOn(t, 2026, 1, "520000"), c.entryOn(t, 2025, 12, "520000"), c.entryOn(t, 2025, 1, "520000"),
		c.entryOn(t, 2026, 2, "520000")
	cases := []struct {
		name, basis, account string
		cites                []string
		status               int
		code, field          string
	}{
		{"an entry of a period the row does not compare", "prior_period", "520000", []string{jan26, feb26}, 422, "closing.citation_invalid", "citations[1]"},
		{"an entry of the prior period against the prior year", "prior_year", "520000", []string{dec25}, 422, "closing.citation_invalid", "citations[0]"},
		{"an entry without a line on the account", "prior_year", "400000", []string{jan25}, 422, "closing.citation_invalid", "citations[0]"},
		{"no entry", "prior_period", "520000", []string{uuid.NewString()}, 422, "closing.citation_invalid", "citations[0]"},
		{"an account without a row", "prior_period", "610000", nil, 422, "closing.variance_row_unknown", "account_id"},
		{"a balance sheet account", "prior_period", "100000", nil, 422, "closing.variance_row_unknown", "account_id"},
		{"a basis not configured", "plan", "520000", nil, 422, "closing.basis_not_configured", "basis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for who, call := range map[string]func() map[string]any{
				"agent": func() map[string]any {
					return c.agent.In(t).Problem(tc.status, tc.code, "POST", "/v1/variance-comment-proposals", c.body(t, tc.basis, tc.account, "Why.", tc.cites, c.session), "")
				},
				"person": func() map[string]any {
					return c.In(t).Problem(tc.status, tc.code, "POST", "/v1/variance-comments", c.body(t, tc.basis, tc.account, "Why.", tc.cites, ""), "")
				},
			} {
				p := call()
				if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field {
					t.Errorf("%s: field errors: got %v, want one on %s", who, p["errors"], tc.field)
				}
			}
		})
	}
	proposal := c.body(t, "prior_period", "520000", "Why.", nil, c.session)
	c.Problem(403, "agents.not_an_agent", "POST", "/v1/variance-comment-proposals", proposal, "")
	c.agent.Problem(422, "request.reference_not_found", "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Why.", nil, uuid.NewString()), "")
	c.agent.Problem(422, "request.validation_failed", "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", " ", nil, c.session), "")
	c.agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/variance-comments", c.body(t, "prior_period", "520000", "Why.", nil, ""), "")
	p, tag := c.agent.Must(201, "POST", "/v1/variance-comment-proposals", proposal, "")
	path := "/v1/variance-comments/" + apitest.String(t, p, "id")
	c.agent.Problem(403, "identity.agent_not_allowed", "POST", path+":acceptProposal", "", tag)
	c.agent.Problem(403, "identity.agent_not_allowed", "POST", path+":rejectProposal", `{"reason": "Mine"}`, tag)
	c.Problem(428, "request.precondition_required", "POST", path+":acceptProposal", "", "")
	c.agent.Must(200, "POST", "/v1/agent-sessions/"+c.session+":end", `{"status": "completed", "input_tokens": 0, "output_tokens": 0}`, "*")
	c.agent.Problem(409, "agents.session_ended", "POST", "/v1/variance-comment-proposals", proposal, "")

	reader := c.As(c.Reader(t, "Reader of 1000", c.US))
	reader.Problem(404, "request.not_found", "GET", path, "", "")
	reader.Problem(404, "request.not_found", "POST", path+":acceptProposal", "", tag)
	reader.Problem(404, "request.not_found", "POST", "/v1/variance-comments", c.body(t, "prior_period", "520000", "Why.", nil, ""), "")
	list, _ := reader.Must(200, "GET", "/v1/variance-comments", "", "")
	if items := apitest.List(t, list, "items"); len(items) != 0 {
		t.Errorf("a reader of 1000's comments: got %d, want none", len(items))
	}
	c.Tenant.Revoke(t, "ledger.entry.read", c.Tenant.Human)
	c.Problem(403, "identity.permission_missing", "GET", path, "", "")
}

// TestListVarianceComments: comments are listed oldest first, filtered by
// close, basis, account, status and session, and paged; a page token is
// valid only with the filters it was made with.
func TestListVarianceComments(t *testing.T) {
	c := newCommenting(t)
	p, _ := c.agent.Must(201, "POST", "/v1/variance-comment-proposals", c.body(t, "prior_period", "520000", "Agent's.", nil, c.session), "")
	c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", "400000", "First.", nil, ""), "")
	c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", "400000", "Second.", nil, ""), "")
	texts := func(query string) []string {
		t.Helper()
		page, _ := c.Must(200, "GET", "/v1/variance-comments?"+query, "", "")
		var out []string
		for _, x := range apitest.List(t, page, "items") {
			m := x.(map[string]any)
			out = append(out, fmt.Sprintf("%v %v", m["text"], m["status"]))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			out = append(out, "next:"+tok)
		}
		return out
	}
	cases := map[string][]string{
		"close_id=" + c.close: {"Agent's. proposed", "First. superseded", "Second. accepted"},
		"basis=prior_year":    {"First. superseded", "Second. accepted"},
		"account_id=" + c.Account(t, "2000", "520000").String(): {"Agent's. proposed"},
		"status=accepted":              {"Second. accepted"},
		"session_id=" + c.session:      {"Agent's. proposed"},
		"close_id=" + uuid.NewString(): nil,
	}
	for query, want := range cases {
		t.Run(query, func(t *testing.T) { wantLines(t, "comments", texts(query), want) })
	}
	first := texts("page_size=2")
	wantLines(t, "first page", first[:2], []string{"Agent's. proposed", "First. superseded"})
	token := url.QueryEscape(strings.TrimPrefix(first[2], "next:"))
	wantLines(t, "second page", texts("page_size=2&page_token="+token), []string{"Second. accepted"})
	c.Problem(400, "request.invalid_page_token", "GET", "/v1/variance-comments?basis=plan&page_token="+token, "", "")
	got, tag := c.Must(200, "GET", "/v1/variance-comments/"+apitest.String(t, p, "id"), "", "")
	if tag != `"1"` || got["session_id"] != c.session {
		t.Errorf("the proposal: got ETag %s and session %v, want \"1\" and %s", tag, got["session_id"], c.session)
	}
}

// TestVarianceCommentWithoutTheBasis: a comment whose basis the company
// no longer compares against is shown without stale; the check of a
// company that configures no basis fails, saying so.
func TestVarianceCommentWithoutTheBasis(t *testing.T) {
	c := newCommenting(t)
	written, _ := c.Must(201, "POST", "/v1/variance-comments", c.body(t, "prior_year", "400000", "First.", nil, ""), "")
	activate(t, c.Tenant.ID, func(doc *config.Config) { doc.Company("2000").Close.Variance = nil })
	got, _ := c.Must(200, "GET", "/v1/variance-comments/"+apitest.String(t, written, "id"), "", "")
	if _, ok := got["stale"]; ok {
		t.Errorf("stale without the basis: got %v, want it absent", got["stale"])
	}
	_, tag := c.Must(200, "GET", "/v1/period-closes/"+c.close, "", "")
	task, _ := c.Must(200, "POST", "/v1/period-closes/"+c.close+"/tasks/EXPLAINED:check", "", tag)
	r := task["check_result"].(map[string]any)
	wantFields(t, "the check without a basis", r, map[string]any{"passed": false,
		"summary": "Company 2000 compares its variances against no basis; configure companies[].close.variance first."})
}
