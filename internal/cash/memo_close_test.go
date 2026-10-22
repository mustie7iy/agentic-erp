package cash_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// lineBody names a statement line.
func lineBody(statement uuid.UUID, no int) string {
	return fmt.Sprintf(`{"statement_id": "%s", "line_no": %d}`, statement, no)
}

// TestRealiseMemoRecord: a record is realised by a statement line of its
// legal entity, currency, bank and direction, or by a person; a series one
// occurrence at a time, by an override of it (ADR-0051 §7).
func TestRealiseMemoRecord(t *testing.T) {
	e := setupMemo(t)
	st := e.statement(t, e.positionEnv.usd, "USD", "2026-04-16", "2026-04-16", 0, 0, nil, "imported")
	e.line(t, st, 1, "2026-04-16", 9500, "unmatched", nil)
	e.line(t, st, 2, "2026-04-16", -100, "unmatched", nil)
	cad := e.statement(t, e.main, "CAD", "2026-04-16", "2026-04-16", 0, 0, nil, "imported")
	e.line(t, cad, 1, "2026-04-16", 9500, "unmatched", nil)

	m, tag := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-16", "liquidity_item": "REC_CUSTOMERS"`)
	for name, body := range map[string]string{
		"another bank's line":  lineBody(cad, 1),
		"a line the other way": lineBody(st, 2),
		"no such line":         lineBody(st, 9),
		"no such statement":    lineBody(uuid.New(), 1),
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(422, "cash.statement_line_invalid", "POST", path(m, "realise"), body, tag)
		})
	}
	e.Problem(422, "request.validation_failed", "POST", path(m, "realise"), `{"statement_id": "`+st.String()+`"}`, tag)
	e.Problem(422, "request.validation_failed", "POST", path(m, "realise"), `{"occurrence_date": "2026-04-16"}`, tag)
	got, tag := e.Must(200, "POST", path(m, "realise"), lineBody(st, 1), tag)
	want(t, "realised", fmt.Sprint(got["status"], " ", got["realised_by"], " ", got["decided_by"] != nil, " ", tag), fmt.Sprintf("realised map[line_no:1 statement_id:%s] true \"2\"", st))
	e.Problem(409, "cash.memo_record_closed", "POST", path(m, "realise"), ``, tag)

	byPerson, tag := e.create(t, `"amount": {"amount": "-20.00", "currency": "USD"}, "value_date": "2026-04-16", "liquidity_item": "TAX"`)
	got, _ = e.Must(200, "POST", path(byPerson, "realise"), `{"comment": "Paid at the counter."}`, tag)
	want(t, "by a person", fmt.Sprint(got["status"], " ", got["realised_by"], " ", got["comment"]), "realised <nil> Paid at the counter.")

	s, stag := e.create(t, e.usd("95.00")+`, "value_date": "2026-04-09", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly"}`)
	e.Problem(422, "request.validation_failed", "POST", path(s, "realise"), lineBody(st, 1), stag)
	e.Problem(422, "cash.occurrence_invalid", "POST", path(s, "realise"), `{"occurrence_date": "2026-04-17"}`, stag)
	o, otag := e.Must(201, "POST", path(s, "realise"), `{"occurrence_date": "2026-04-16", "statement_id": "`+st.String()+`", "line_no": 1}`, stag)
	want(t, "the occurrence's override", fmt.Sprint(o["status"], " ", o["recurrence_of"], " ", o["occurrence_date"], " ", o["value_date"], " ", o["expires_on"], " ",
		amount(o["amount"]), " ", o["origin"], " ", otag), fmt.Sprintf(`realised %s 2026-04-16 2026-04-16 2026-04-23 95.00 USD manual "1"`, id(s)))
	e.Problem(409, "cash.occurrence_overridden", "POST", path(s, "realise"), `{"occurrence_date": "2026-04-16"}`, stag)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", path(s, "realise"), `{"occurrence_date": "2026-04-23"}`, stag)

	changes := map[string]int{}
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "memo_record_changed"}) {
		changes[fmt.Sprint(ev["change"])]++
	}
	want(t, "events", changes, map[string]int{"created": 3, "realised": 3})
}

// TestCancelMemoRecord: a person cancels a record, a series, or one
// occurrence of a series; a closed record and an agent's proposal are not
// cancelled (ADR-0051 §7).
func TestCancelMemoRecord(t *testing.T) {
	e := setupMemo(t)
	m, tag := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`)
	e.Problem(422, "request.validation_failed", "POST", path(m, "cancel"), `{"occurrence_date": "2026-04-21"}`, tag)
	got, tag := e.Must(200, "POST", path(m, "cancel"), `{"comment": "The customer paid early."}`, tag)
	want(t, "cancelled", fmt.Sprint(got["status"], " ", got["comment"], " ", tag), `cancelled The customer paid early. "2"`)
	e.Problem(409, "cash.memo_record_closed", "POST", path(m, "cancel"), ``, tag)

	s, stag := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-03", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly"}`)
	skipped, _ := e.Must(201, "POST", path(s, "cancel"), `{"occurrence_date": "2026-04-24"}`, stag)
	want(t, "skipped", fmt.Sprint(skipped["status"], " ", skipped["occurrence_date"]), "cancelled 2026-04-24")
	e.Problem(409, "cash.occurrence_overridden", "POST", path(s, "cancel"), `{"occurrence_date": "2026-04-24"}`, stag)
	got, _ = e.Must(200, "POST", path(s, "cancel"), ``, stag)
	want(t, "the series", got["status"], "cancelled")

	p := e.propose(t, e.session(t), e.usd("100.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`)
	e.Problem(409, "cash.memo_record_proposed", "POST", path(p, "cancel"), ``, `"1"`)
}
