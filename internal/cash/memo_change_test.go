package cash_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestUpdateMemoRecord: a person moves, confirms or changes a record with
// its ETag, checked as a new one is; a series stays a series (ADR-0051 §7).
func TestUpdateMemoRecord(t *testing.T) {
	e := setupMemo(t)
	m, tag := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-21", "expires_on": "2026-04-23", "liquidity_item": "REC_CUSTOMERS", "description": "Northwind"`)
	got, tag := e.Must(200, "PATCH", path(m, ""), `{"value_date": "2026-05-04"}`, tag)
	want(t, "moved, its expiry with it", fmt.Sprint(got["value_date"], " ", got["expires_on"], " ", tag), `2026-05-04 2026-05-06 "2"`)
	got, tag = e.Must(200, "PATCH", path(m, ""), `{"expires_on": "2026-05-31", "certainty": "confirmed", "amount": {"amount": "120.00", "currency": "USD"},
		"description": null, "reference": "INV-9", "house_bank_id": null}`, tag)
	want(t, "changed", fmt.Sprint(got["expires_on"], " ", got["certainty"], " ", amount(got["amount"]), " ", got["description"], " ", got["reference"], " ",
		got["house_bank_id"]), "2026-05-31 confirmed 120.00 USD <nil> INV-9 <nil>")
	e.Problem(412, "request.version_mismatch", "PATCH", path(m, ""), `{"certainty": "planned"}`, `"1"`)
	e.Problem(422, "cash.liquidity_item_invalid", "PATCH", path(m, ""), `{"liquidity_item": "TAX"}`, tag)
	e.Problem(422, "request.validation_failed", "PATCH", path(m, ""), `{"recurrence": {"frequency": "weekly"}}`, tag)
	e.Problem(404, "request.not_found", "PATCH", "/v1/memo-records/"+uuid.NewString(), `{"certainty": "planned"}`, tag)

	s, stag := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-03", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly"}`)
	got, _ = e.Must(200, "PATCH", path(s, ""), `{"recurrence": {"frequency": "monthly", "until": "2026-06-30"}}`, stag)
	want(t, "a series' rule", got["recurrence"], map[string]any{"frequency": "monthly", "until": "2026-06-30"})

	_, tag = e.Must(200, "POST", path(m, "cancel"), `{"comment": "Not expected any more."}`, tag)
	e.Problem(409, "cash.memo_record_closed", "PATCH", path(m, ""), `{"certainty": "planned"}`, tag)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "PATCH", path(s, ""), `{"certainty": "planned"}`, `"2"`)
}

// TestOverrideMemoRecordOccurrence: an override replaces one occurrence of
// an active series of its legal entity and currency, once (ADR-0051 §7).
func TestOverrideMemoRecordOccurrence(t *testing.T) {
	e := setupMemo(t)
	s, _ := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-03", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly", "count": 4}`)
	one, _ := e.create(t, e.usd("100.00")+`, "value_date": "2026-04-03", "liquidity_item": "REC_CUSTOMERS"`)
	override := func(series, day string) string {
		return e.usd("90.00") + `, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS", "recurrence_of": "` + series + `", "occurrence_date": "` + day + `"`
	}
	got, _ := e.create(t, override(id(s), "2026-04-10"))
	want(t, "override", fmt.Sprint(got["recurrence_of"], " ", got["occurrence_date"], " ", got["value_date"]), id(s)+" 2026-04-10 2026-04-17")
	e.Problem(409, "cash.occurrence_overridden", "POST", "/v1/memo-records", e.body(override(id(s), "2026-04-10")), "")
	for name, c := range map[string]struct{ body, code string }{
		"not an occurrence":    {override(id(s), "2026-04-11"), "cash.occurrence_invalid"},
		"past the series' end": {override(id(s), "2026-05-01"), "cash.occurrence_invalid"},
		"of a one-off record":  {override(id(one), "2026-04-03"), "cash.occurrence_invalid"},
		"of no record":         {override(uuid.NewString(), "2026-04-03"), "cash.occurrence_invalid"},
		"in another currency": {`"amount": {"amount": "90.00", "currency": "CAD"}, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS", "recurrence_of": "` +
			id(s) + `", "occurrence_date": "2026-04-17"`, "cash.occurrence_invalid"},
		"without its occurrence": {e.usd("90.00") + `, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS", "recurrence_of": "` + id(s) + `"`,
			"cash.occurrence_invalid"},
		"a series itself": {override(id(s), "2026-04-17") + `, "recurrence": {"frequency": "weekly"}`, "request.validation_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(422, c.code, "POST", "/v1/memo-records", e.body(c.body), "")
		})
	}
}
