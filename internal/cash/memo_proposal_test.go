package cash_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestProposeMemoRecord: the agent proposes a memo record in its running
// session with its reason and evidence; a person accepts or rejects it;
// the agent never decides one (ADR-0051 §13).
func TestProposeMemoRecord(t *testing.T) {
	e := setupMemo(t)
	st := e.statement(t, e.positionEnv.usd, "USD", "2026-03-31", "2026-03-31", 0, 0, nil, "imported")
	e.line(t, st, 1, "2026-03-31", 1250000, "unmatched", nil)
	session := e.session(t)
	p := e.propose(t, session, e.usd("12500.00")+`, "value_date": "2026-04-30", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "monthly", "count": 3},
		"evidence": [`+lineBody(st, 1)+`]`)
	want(t, "proposal", fmt.Sprint(p["status"], " ", p["origin"], " ", p["session_id"], " ", p["reason"], " ", p["evidence"]),
		fmt.Sprintf("proposed agent %s Seen monthly. [map[line_no:1 statement_id:%s]]", session, st))

	agent := e.As(e.Tenant.Agent)
	agent.Problem(403, "identity.agent_not_allowed", "POST", path(p, "acceptProposal"), ``, `"1"`)
	agent.Problem(403, "identity.agent_not_allowed", "POST", path(p, "rejectProposal"), `{"reason": "no"}`, `"1"`)
	e.Problem(412, "request.version_mismatch", "POST", path(p, "acceptProposal"), ``, `"2"`)
	got, tag := e.Must(200, "POST", path(p, "acceptProposal"), ``, `"1"`)
	want(t, "accepted", fmt.Sprint(got["status"], " ", got["decided_by"], " ", tag), fmt.Sprint("active ", e.Tenant.Human.ID, ` "2"`))
	e.Problem(409, "cash.memo_record_not_proposed", "POST", path(p, "acceptProposal"), ``, tag)
	e.Problem(409, "cash.memo_record_not_proposed", "POST", path(p, "rejectProposal"), `{"reason": "no"}`, tag)

	r := e.propose(t, session, e.usd("700.00")+`, "value_date": "2026-04-30", "liquidity_item": "REC_CUSTOMERS"`)
	got, _ = e.Must(200, "POST", path(r, "rejectProposal"), `{"reason": "Northwind has stopped paying monthly."}`, `"1"`)
	want(t, "rejected", fmt.Sprint(got["status"], " ", got["comment"]), "rejected Northwind has stopped paying monthly.")

	// A proposal is checked again when accepted: its bank closed since.
	b := e.propose(t, session, e.usd("700.00")+`, "value_date": "2026-04-30", "liquidity_item": "REC_CUSTOMERS"`)
	got, tag = e.Must(200, "PATCH", path(b, ""), `{"amount": {"amount": "750.00", "currency": "USD"}}`, `"1"`)
	want(t, "a person changes a proposal", fmt.Sprint(got["status"], " ", amount(got["amount"])), "proposed 750.00 USD")
	pgtest.Exec(t, `UPDATE payments.house_bank SET status = 'inactive', active_gl_account_id = NULL, version = version + 1 WHERE id = $1`, e.positionEnv.usd)
	e.Problem(422, "cash.house_bank_invalid", "POST", path(b, "acceptProposal"), ``, tag)

	changes := map[string]int{}
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "memo_record_changed"}) {
		changes[fmt.Sprint(ev["change"])]++
	}
	want(t, "events", changes, map[string]int{"proposed": 3, "accepted": 1, "rejected": 1, "updated": 1})
}

// TestProposeMemoRecordRefusals: a proposal is the calling agent's, in its
// running session, citing lines of the legal entity; a person does not
// propose (ADR-0051 §13).
func TestProposeMemoRecordRefusals(t *testing.T) {
	e := setupMemo(t)
	other := e.statement(t, e.positionEnv.usd, "USD", "2026-03-31", "2026-03-31", 0, 0, nil, "imported")
	e.line(t, other, 1, "2026-03-31", 100, "unmatched", nil)
	pgtest.Exec(t, `UPDATE payments.bank_statement SET legal_entity_id = $2, version = version + 1 WHERE id = $1`, other, e.US)
	session := e.session(t)
	agent := e.As(e.Tenant.Agent)
	body := func(session, more string) string {
		return e.body(`"session_id": "` + session + `", "reason": "Seen monthly.", ` + e.usd("10.00") + `, "value_date": "2026-04-30", "liquidity_item": "REC_CUSTOMERS"` + more)
	}
	agent.Problem(422, "cash.statement_line_invalid", "POST", "/v1/memo-record-proposals", body(session, `, "evidence": [`+lineBody(other, 1)+`]`), "")
	agent.Problem(422, "request.reference_not_found", "POST", "/v1/memo-record-proposals", body(uuid.NewString(), ""), "")
	agent.Problem(422, "cash.liquidity_item_invalid", "POST", "/v1/memo-record-proposals",
		e.body(`"session_id": "`+session+`", "reason": "x", `+e.usd("-10.00")+`, "value_date": "2026-04-30", "liquidity_item": "REC_CUSTOMERS"`), "")
	e.Problem(403, "agents.not_your_session", "POST", "/v1/memo-record-proposals", body(session, ""), "")
}
