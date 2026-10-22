package cash_test

import (
	"fmt"
	"testing"
)

// session opens a running session of the tenant's agent, which acts in
// every company.
func (e memoEnv) session(t *testing.T) string {
	t.Helper()
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	s, _ := e.As(e.Tenant.Agent).Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Forecast receipts", "model": "erp-default"}`, "")
	return id(s)
}

// propose has the agent propose a memo record in the session.
func (e memoEnv) propose(t *testing.T, session, fields string) map[string]any {
	t.Helper()
	m, _ := e.As(e.Tenant.Agent).Must(201, "POST", "/v1/memo-record-proposals", e.body(`"session_id": "`+session+`", "reason": "Seen monthly.", `+fields), "")
	return m
}

// memoScenario writes Maple Inc's memo records, read on Friday 17 April:
//   - USD, a weekly receipt of 100.00, confirmed, from Friday 3 April: the
//     3rd's expired on the 10th; the 10th's counts until the 17th, late, on
//     the 21st (the weekend, then Monday's holiday); the 17th's overridden
//     by 150.00, expected; the 24th's cancelled;
//   - USD, a planned payment of 40.00 on Saturday the 18th, at the bank on
//     the 21st; a receipt of 10.00 expired on the 8th; an agent's proposal
//     of 500.00 on the 22nd; a receipt of 20.00 on the 22nd realised; one
//     of 30.00 cancelled; a receipt of 999.00 for the forecast only;
//   - MAIN, a receipt of CAD 300.00 on the 23rd.
func (e memoEnv) memoScenario(t *testing.T) (proposal map[string]any) {
	t.Helper()
	w, tag := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-03", "liquidity_item": "REC_CUSTOMERS", "certainty": "confirmed", "recurrence": {"frequency": "weekly"}`)
	e.create(t, e.usd("150.00")+`, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS", "recurrence_of": "`+id(w)+`", "occurrence_date": "2026-04-17"`)
	e.Must(201, "POST", path(w, "cancel"), `{"occurrence_date": "2026-04-24"}`, tag)
	e.create(t, e.usd("-40.00")+`, "value_date": "2026-04-18", "liquidity_item": "OP_SUPPLIERS", "certainty": "planned"`)
	e.create(t, e.usd("10.00")+`, "value_date": "2026-04-01", "liquidity_item": "REC_CUSTOMERS"`)
	proposal = e.propose(t, e.session(t), e.usd("500.00")+`, "value_date": "2026-04-22", "liquidity_item": "REC_CUSTOMERS"`)
	r, tag := e.create(t, e.usd("20.00")+`, "value_date": "2026-04-22", "liquidity_item": "REC_CUSTOMERS"`)
	e.Must(200, "POST", path(r, "realise"), `{"comment": "Paid by cheque."}`, tag)
	c, tag := e.create(t, e.usd("30.00")+`, "value_date": "2026-04-22", "liquidity_item": "REC_CUSTOMERS"`)
	e.Must(200, "POST", path(c, "cancel"), ``, tag)
	e.create(t, `"amount": {"amount": "999.00", "currency": "USD"}, "value_date": "2026-04-22", "liquidity_item": "REC_CUSTOMERS"`)
	e.create(t, `"house_bank_id": "`+e.main.String()+`", "amount": {"amount": "300.00", "currency": "CAD"}, "value_date": "2026-04-23", "liquidity_item": "REC_CUSTOMERS"`)
	return proposal
}

// TestMemoRecordsInThePosition: active memo records on a house bank are a
// source of its position, each occurrence of a series once, overrides in
// place of their occurrence, late ones the working day after the read;
// expired, proposed, realised, cancelled and forecast-only records count
// nowhere, until a person accepts the proposal (ADR-0051 §7-8).
func TestMemoRecordsInThePosition(t *testing.T) {
	e := setupMemo(t)
	proposal := e.memoScenario(t)
	usd := bankOf(t, e.positionOf(t, "&days=8"), "USD")
	want(t, "USD days", days(t, usd["days"]), []string{
		"2026-04-17 0.00 USD>150.00 USD (5150.00 USD) memo/expected/late=false 150.00 USD x1",
		"2026-04-18 150.00 USD>150.00 USD (5150.00 USD)", "2026-04-19 150.00 USD>150.00 USD (5150.00 USD)", "2026-04-20 150.00 USD>150.00 USD (5150.00 USD)",
		"2026-04-21 150.00 USD>210.00 USD (5210.00 USD) memo/confirmed/late=true 100.00 USD x1 memo/planned/late=false -40.00 USD x1",
		"2026-04-22 210.00 USD>210.00 USD (5210.00 USD)", "2026-04-23 210.00 USD>210.00 USD (5210.00 USD)", "2026-04-24 210.00 USD>210.00 USD (5210.00 USD)"})
	main := bankOf(t, e.positionOf(t, "&days=8"), "MAIN")
	want(t, "MAIN's 23rd", days(t, main["days"])[6], "2026-04-23 0.00 CAD>300.00 CAD (300.00 CAD) memo/expected/late=false 300.00 CAD x1")

	e.Must(200, "POST", path(proposal, "acceptProposal"), `{"comment": "Matches the plan."}`, `"1"`)
	usd = bankOf(t, e.positionOf(t, "&from=2026-04-22&days=1"), "USD")
	want(t, "accepted, from the 22nd", days(t, usd["days"]), []string{
		"2026-04-22 210.00 USD>710.00 USD (5710.00 USD) memo/expected/late=false 500.00 USD x1"})
}

// TestMemoRecordStatusOnTheReadDate: a record past its last occurrence's
// expiry is expired, listed so, and counts nowhere; a series without end
// never expires (ADR-0051 §7).
func TestMemoRecordStatusOnTheReadDate(t *testing.T) {
	e := setupMemo(t)
	old, _ := e.create(t, e.usd("10.00")+`, "value_date": "2026-04-01", "liquidity_item": "REC_CUSTOMERS"`)
	ended, _ := e.create(t, e.usd("10.00")+`, "value_date": "2026-01-31", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "monthly", "count": 2}`)
	open, _ := e.create(t, e.usd("10.00")+`, "value_date": "2026-01-31", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "quarterly"}`)
	for _, c := range []struct {
		name string
		m    map[string]any
		want string
	}{{"one-off", old, "expired"}, {"a series ended in February", ended, "expired"}, {"a series without end", open, "active"}} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := e.In(t).Must(200, "GET", path(c.m, ""), "", "")
			want(t, "status", got["status"], c.want)
		})
	}
	listed := func(status string) string {
		p, _ := e.Must(200, "GET", "/v1/memo-records?status="+status, "", "")
		var out []string
		for _, it := range p["items"].([]any) {
			out = append(out, id(it.(map[string]any)))
		}
		return fmt.Sprint(out)
	}
	want(t, "expired", listed("expired"), fmt.Sprint([]string{id(ended), id(old)}))
	want(t, "active", listed("active"), fmt.Sprint([]string{id(open)}))
	want(t, "nothing counts in April", days(t, bankOf(t, e.positionOf(t, "&days=1"), "USD")["days"]), []string{"2026-04-17 0.00 USD>0.00 USD (5000.00 USD)"})
	usd := bankOf(t, e.positionOf(t, "&from=2026-04-30&days=1"), "USD")
	want(t, "the quarterly series on 30 April", days(t, usd["days"]), []string{
		"2026-04-30 0.00 USD>10.00 USD (5010.00 USD) memo/expected/late=false 10.00 USD x1"})
}
