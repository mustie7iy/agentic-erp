package cash_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// proposals is the path the forecast agent proposes comments at.
const proposals = "/v1/liquidity-forecast-variance/comment-proposals"

// setupProposals is setupVariance's Maple Inc whose agent may propose
// comments, in a running session, and whose people may decide them.
func setupProposals(t *testing.T) (varianceEnv, string) {
	t.Helper()
	e := setupVariance(t)
	e.Tenant.Grant(t, "cash.variance.propose", e.Tenant.Agent)
	e.Tenant.Grant(t, "agents.session.record", e.Tenant.Agent)
	e.Tenant.Grant(t, "payments.statement.read", e.Tenant.Agent)
	e.Tenant.Grant(t, "cash.variance.comment", e.Tenant.Human)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	s, _ := e.As(e.Tenant.Agent).Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Explain the variance", "model": "erp-default"}`, "")
	return e, id(s)
}

// proposal is a proposal's body on Maple's weekly variance from 17 April
// to 21 May at lag 0, of the row and with the citations given.
func (e varianceEnv) proposal(session, row, citations string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "from": "2026-04-17", "to": "2026-05-21", "lag": 0, %s, "session_id": %q,
		"text": "The run of the 22nd was released a week late.", "citations": [%s]}`, e.le, row, session, citations)
}

// commentAt is a comment's custom method.
func commentAt(c map[string]any, verb string) string {
	return "/v1/liquidity-forecast-variance/comments/" + id(c) + ":" + verb
}

// cite is a citation's JSON.
func cite(kind string, id uuid.UUID) string { return fmt.Sprintf(`{"kind": %q, "id": %q}`, kind, id) }

// TestProposeVarianceComment: the agent proposes a comment on a row of the
// variance in its running session, citing documents behind its
// attribution; a person accepts it as it stands, or rejects another with
// a reason, each once; the agent never decides one (ADR-0051 §13).
func TestProposeVarianceComment(t *testing.T) {
	e, session := setupProposals(t)
	agent := e.As(e.Tenant.Agent)
	run2 := e.paymentOf(t, e.docs["run2"])
	c, tag := agent.Must(201, "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	want(t, "proposed", commentText(c), "2026-04-17..2026-05-21 lag 0 OP_SUPPLIERS (Supplier payments) 2026-04-17 proposed: The run of the 22nd was released a week late.")
	want(t, "its session, citations and author", fmt.Sprint(c["session_id"], " ", c["citations"], " ", c["created_by"], " ", c["decided_by"], " ", tag),
		fmt.Sprintf(`%s [map[id:%s kind:payment]] %s <nil> "1"`, session, run2, e.Tenant.Agent.ID))
	want(t, "its snapshot, the variance's", c["snapshot_id"], e.snapshot)
	got, tag := e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments/"+id(c), "", "")
	want(t, "read again", fmt.Sprint(got, " ", tag), fmt.Sprint(c, ` "1"`))
	want(t, "listed as proposed", e.listedFor(t, "?status=proposed"), []string{commentText(c)})

	agent.Problem(403, "identity.agent_not_allowed", "POST", commentAt(c, "acceptProposal"), ``, `"1"`)
	agent.Problem(403, "identity.agent_not_allowed", "POST", commentAt(c, "rejectProposal"), `{"reason": "no"}`, `"1"`)
	e.Problem(412, "request.version_mismatch", "POST", commentAt(c, "acceptProposal"), ``, `"2"`)
	accepted, tag := e.Must(200, "POST", commentAt(c, "acceptProposal"), ``, `"1"`)
	want(t, "accepted", fmt.Sprint(accepted["status"], " ", accepted["decided_by"], " ", accepted["citations"], " ", tag),
		fmt.Sprintf(`accepted %s [map[id:%s kind:payment]] "2"`, e.Tenant.Human.ID, run2))
	e.Problem(409, "cash.variance_comment_not_proposed", "POST", commentAt(c, "acceptProposal"), ``, tag)
	e.Problem(409, "cash.variance_comment_not_proposed", "POST", commentAt(c, "rejectProposal"), `{"reason": "no"}`, tag)

	// The receipts' row: the receipt paid short, and the line not matched by its statement.
	r, _ := agent.Must(201, "POST", proposals, e.proposal(session, `"liquidity_item": "REC_CUSTOMERS"`,
		cite("memo_record", e.docs["receipt"])+", "+cite("bank_statement", e.statementOf(t, "CAD"))), "")
	rejected, _ := e.Must(200, "POST", commentAt(r, "rejectProposal"), `{"reason": "The receipt was short by the bank's fee."}`, `"1"`)
	want(t, "rejected", fmt.Sprint(rejected["status"], " ", rejected["decision_reason"], " ", rejected["decided_by"]),
		fmt.Sprint("rejected The receipt was short by the bank's fee. ", e.Tenant.Human.ID))
	// A person accepts another in their own words; the agent's is kept.
	edited, _ := agent.Must(201, "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	e.Problem(400, "request.invalid_body", "POST", commentAt(edited, "acceptProposal"), `{"text": " "}`, `"1"`)
	got, _ = e.Must(200, "POST", commentAt(edited, "acceptProposal"), `{"text": "The run of the 22nd waited a week for its approver."}`, `"1"`)
	want(t, "accepted in the person's words", fmt.Sprint(got["status"], " | ", got["text"], " | ", got["proposed_text"]),
		"accepted | The run of the 22nd waited a week for its approver. | The run of the 22nd was released a week late.")
	same, _ := agent.Must(201, "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	got, _ = e.Must(200, "POST", commentAt(same, "acceptProposal"), `{"text": "The run of the 22nd was released a week late."}`, `"1"`)
	want(t, "the same words, accepted as it stands", fmt.Sprint(got["status"], " ", got["proposed_text"]), "accepted <nil>")
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "POST", commentAt(r, "acceptProposal"), ``, `"2"`)

	decided := map[string]int{}
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "variance_comment_decided"}) {
		decided[fmt.Sprint(ev["status"], " by ", ev["decided_by"])]++
	}
	want(t, "decisions published", decided, map[string]int{"accepted by " + e.Tenant.Human.ID.String(): 3, "rejected by " + e.Tenant.Human.ID.String(): 1})
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "variance_comment_proposed"})
	want(t, "proposals published", len(evs), 4)
	want(t, "a proposal's event", fmt.Sprint(evs[0]["session_id"], " ", evs[0]["citation_count"], " ", evs[0]["liquidity_item"]), session+" 1 OP_SUPPLIERS")
}

// listedFor renders the comments of Maple's variance listed with the query.
func (e varianceEnv) listedFor(t *testing.T, query string) []string {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments"+query, "", "")
	out := []string{}
	for _, c := range p["items"].([]any) {
		out = append(out, commentText(c.(map[string]any)))
	}
	return out
}

// statementOf is Maple's statement of the currency the banks showed from
// 17 April.
func (e varianceEnv) statementOf(t *testing.T, currency string) uuid.UUID {
	t.Helper()
	names := e.names(t)
	for id, name := range names {
		if name == currency+" statement" {
			return uuid.MustParse(id)
		}
	}
	t.Fatalf("no %s statement", currency)
	return uuid.Nil
}

// TestProposeVarianceCommentRefusals: a proposal cites only documents
// behind the attribution of its row, as the agent may see them; it is the
// calling agent's, in its running session, checked as a person's comment
// is; a person neither proposes nor an agent comments.
func TestProposeVarianceCommentRefusals(t *testing.T) {
	e, session := setupProposals(t)
	agent := e.As(e.Tenant.Agent)
	run2 := e.paymentOf(t, e.docs["run2"])
	p := agent.Problem(422, "cash.citation_not_attributed", "POST", proposals, e.proposal(session, `"liquidity_item": "REC_CUSTOMERS"`,
		cite("memo_record", e.docs["receipt"])+", "+cite("payment", run2)+", "+cite("journal_entry", uuid.New())), "")
	var fields []string
	for _, f := range p["errors"].([]any) {
		fields = append(fields, fmt.Sprint(f.(map[string]any)["field"]))
	}
	want(t, "each citation not behind the row", fields, []string{"citations[1]", "citations[2]"})
	agent.Problem(422, "cash.citation_not_attributed", "POST", proposals, e.proposal(session, `"liquidity_item": "REC_CUSTOMERS"`,
		cite("bank_statement", e.statementOf(t, "USD"))), "")
	agent.Problem(422, "cash.citation_not_attributed", "POST", proposals, e.proposal(session, `"unclassified": true`, cite("payment", run2)), "")
	agent.Problem(422, "request.reference_not_found", "POST", proposals, e.proposal(uuid.NewString(), `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	agent.Problem(422, "request.validation_failed", "POST", proposals, e.proposal(session, `"liquidity_item": "NOPE"`, cite("payment", run2)), "")
	agent.Problem(422, "request.validation_failed", "POST", proposals, strings.Replace(e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`,
		cite("payment", run2)), `"lag": 0`, `"lag": 5`, 1), "")
	agent.Problem(422, "request.validation_failed", "POST", proposals, strings.Replace(e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`,
		cite("payment", run2)), `"to": "2026-05-21"`, `"to": "2026-05-23"`, 1), "")
	agent.Problem(409, "cash.snapshot_not_taken", "POST", proposals, strings.Replace(e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`,
		cite("payment", run2)), `"lag": 0`, `"lag": 7`, 1), "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/liquidity-forecast-variance/comments",
		fmt.Sprintf(`{"legal_entity_id": %q, "from": "2026-04-17", "to": "2026-04-17", "lag": 0, "unclassified": true, "text": "Mine."}`, e.le), "")
	e.Tenant.Grant(t, "cash.variance.propose", e.Tenant.Human)
	e.Problem(403, "agents.not_your_session", "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	// Without the run's read, the agent does not see the payment it cites.
	e.Tenant.Revoke(t, "payments.run.read", e.Tenant.Agent)
	agent.Problem(422, "cash.citation_not_attributed", "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	e.Tenant.Revoke(t, "cash.variance.propose", e.Tenant.Agent)
	agent.Problem(403, "identity.permission_missing", "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	want(t, "nothing written", e.listedFor(t, ""), []string{})
}

// TestProposeVarianceCommentFailures: every database failure of a
// proposal and of its decisions is an error, as is the directory down.
func TestProposeVarianceCommentFailures(t *testing.T) {
	e, session := setupProposals(t)
	run2 := e.paymentOf(t, e.docs["run2"])
	c, _ := e.As(e.Tenant.Agent).Must(201, "POST", proposals, e.proposal(session, `"liquidity_item": "OP_SUPPLIERS"`, cite("payment", run2)), "")
	endpoints := func() *cash.Endpoints { return app.Cash(app.Config{Now: func() time.Time { return varianceAt }}) }
	from, to := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	propose := func(ctx context.Context) error {
		_, err := endpoints().ProposeComment(ctx, cash.CommentInput{LegalEntity: e.le, From: from, To: to, Item: "OP_SUPPLIERS", Text: "Late."},
			uuid.MustParse(session), []cash.Citation{{Kind: "payment", ID: run2}})
		return err
	}
	if err := propose(context.Background()); err == nil {
		t.Error("outside a unit of work: got no error, want it refused")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Agent, "propose", propose)
	written := uuid.MustParse(id(c))
	for name, fn := range map[string]func(ctx context.Context) error{
		"accept": func(ctx context.Context) error {
			text := "In my words."
			_, err := endpoints().AcceptComment(ctx, written, func(int32) error { return nil }, &text)
			return err
		},
		"reject": func(ctx context.Context) error {
			_, err := endpoints().RejectComment(ctx, written, func(int32) error { return nil }, "No.")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want it refused")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	blank := " \n"
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints().AcceptComment(ctx, written, func(int32) error { return nil }, &blank)
		return err
	}); problem.From(err).Code != "request.validation_failed" {
		t.Errorf("a blank text: got %v, want it refused", err)
	}
	ce := endpoints()
	ce.LegalEntities = failingEntities{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := ce.AcceptComment(ctx, written, func(int32) error { return nil }, nil)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "directory down") {
		t.Errorf("the directory down: got %v, want its error", err)
	}
}
