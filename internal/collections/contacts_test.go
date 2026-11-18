package collections_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// contactOf is the JSON of a contact with the customer at the time, with
// the outcome and, when it is not empty, a resubmission date.
func contactOf(company, customer uuid.UUID, at, outcome, resubmission string) string {
	extra := ""
	if resubmission != "" {
		extra = fmt.Sprintf(`, "resubmission_date": %q`, resubmission)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "contacted_at": %q, "contact_name": "Dana Ortiz", "outcome": %q,
		"note": "Will pay next week"%s}`, company, customer, at, outcome, extra)
}

// listed are the customers on the worklist at the key date, by id, with
// the worklist's resubmission totals.
func (e env) listed(t *testing.T, key string) (map[string]map[string]any, string) {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/collection-worklist?legal_entity_id="+e.DE.String()+"&key_date="+key, "", "")
	out := map[string]map[string]any{}
	for _, c := range objects(t, got, "items") {
		out[c["customer_id"].(string)] = c
	}
	return out, fields(t, got, "customer_count", "overdue_amount", "resubmission_count", "resubmission_overdue_amount")
}

// TestCustomerContacts (T76g acceptance): a contact reads back with what
// it names; a customer whose last contact's resubmission date is after the
// key date is left off the worklist and counted, and is back on it at
// that date with its last contact; a later contact without a resubmission
// date brings it back at once; the last contact is the latest by
// contacted_at, not the latest recorded; the contacts are listed the
// latest first, filtered; each is audited.
func TestCustomerContacts(t *testing.T) {
	e := setup(t)
	e.story(t)
	got, tag := e.Must(201, "POST", "/v1/customer-contacts", contactOf(e.DE, e.north, "2026-06-29T09:00:00Z", "reached", lastHeld), "")
	first := got["id"].(string)
	if g, w := fields(t, got, "customer_number", "customer_name", "company_code", "contacted_at", "contact_name", "outcome", "note", "resubmission_date",
		"contacted_by_name", "version"), "customer_number BP70000002, customer_name Northwind Traders, company_code 2000, contacted_at 2026-06-29T09:00:00Z, "+
		"contact_name Dana Ortiz, outcome reached, note Will pay next week, resubmission_date "+lastHeld+", contacted_by_name human, version 1"; g != w {
		t.Errorf("the contact: got %s, want %s", g, w)
	}
	if read, readTag := e.Must(200, "GET", "/v1/customer-contacts/"+first, "", ""); mustJSON(t, read) != mustJSON(t, got) || readTag != tag {
		t.Errorf("read back: got %s (%s), want %s (%s)", mustJSON(t, read), readTag, mustJSON(t, got), tag)
	}
	rows, totals := e.listed(t, keyDate)
	if _, ok := rows[e.north.String()]; ok {
		t.Error("Northwind on the worklist before its resubmission date: got it listed, want it left out")
	}
	// Southwind's 160.00 EUR is the one customer left; Northwind's 220.00 EUR overdue is counted aside.
	if w := "customer_count 1, overdue_amount 160.00 EUR, resubmission_count 1, resubmission_overdue_amount 220.00 EUR"; totals != w {
		t.Errorf("the worklist's totals: got %s, want %s", totals, w)
	}
	rows, _ = e.listed(t, lastHeld)
	if g, w := fields(t, rows[e.north.String()], "last_contacted_at", "last_contact_outcome", "resubmission_date"),
		"last_contacted_at 2026-06-29T09:00:00Z, last_contact_outcome reached, resubmission_date "+lastHeld; g != w {
		t.Errorf("Northwind on its resubmission date: got %s, want %s", g, w)
	}
	// Recorded later but dated earlier: not the last contact, so Northwind stays off.
	e.Must(201, "POST", "/v1/customer-contacts", contactOf(e.DE, e.north, "2026-06-28T09:00:00Z", "not_reached", ""), "")
	if rows, _ := e.listed(t, keyDate); rows[e.north.String()] != nil {
		t.Error("after an earlier contact: got Northwind listed, want it left out still")
	}
	e.Must(201, "POST", "/v1/customer-contacts", contactOf(e.DE, e.north, "2026-06-30T08:00:00Z", "left_message", ""), "")
	rows, totals = e.listed(t, keyDate)
	if g, w := fields(t, rows[e.north.String()], "last_contact_outcome", "resubmission_date"), "last_contact_outcome left_message, resubmission_date <nil>"; g != w {
		t.Errorf("Northwind after a later contact: got %s, want %s", g, w)
	}
	if w := "customer_count 2, overdue_amount 380.00 EUR, resubmission_count 0, resubmission_overdue_amount 0.00 EUR"; totals != w {
		t.Errorf("the worklist's totals: got %s, want %s", totals, w)
	}
	if row := rows[e.south.String()]; row["last_contacted_at"] != nil || row["last_contact_outcome"] != nil {
		t.Errorf("Southwind, never contacted: got %v, want no last contact", row)
	}
	e.Must(201, "POST", "/v1/customer-contacts", contactOf(e.DE, e.south, "2026-06-30T10:00:00Z", "reached", ""), "")
	for _, c := range []struct{ name, query, want string }{
		{"all", "", "reached left_message reached not_reached"},
		{"Northwind's", "?customer_id=" + e.north.String(), "left_message reached not_reached"},
		{"the company's, a page of one", "?legal_entity_id=" + e.DE.String() + "&page_size=1", "reached"},
		{"another company's", "?legal_entity_id=" + uuid.NewString(), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/customer-contacts"+c.query, "", "")
			var outcomes []string
			for _, x := range objects(t, page, "items") {
				outcomes = append(outcomes, x["outcome"].(string))
			}
			if g := strings.Join(outcomes, " "); g != c.want {
				t.Errorf("got %s, want %s", g, c.want)
			}
		})
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'collections.customer_contact.record'`, []any{e.Tenant.ID}, &audits)
	if audits != 4 {
		t.Errorf("audit records: got %d, want 4", audits)
	}
}

// TestContactRefusals: a contact after now, an unknown outcome, texts of
// white space, a resubmission date not after today or after the latest
// key date, a customer that is no business partner and a company the
// caller may not act in are refused, each naming its field; an unknown
// contact is not found.
func TestContactRefusals(t *testing.T) {
	e := setup(t)
	body := contactOf(e.DE, e.north, "2026-06-29T09:00:00Z", "reached", lastHeld)
	for _, c := range []struct {
		name, body, code, field string
	}{
		{"a contact after now", strings.Replace(body, "2026-06-29T09:00:00Z", "2100-01-01T00:00:00Z", 1), "collections.invalid_contact_time", "contacted_at"},
		{"an unknown outcome", strings.Replace(body, `"reached"`, `"emailed"`, 1), "collections.unknown_outcome", "outcome"},
		{"a contact name of white space", strings.Replace(body, "Dana Ortiz", " ", 1), "request.validation_failed", "contact_name"},
		{"a note of white space", strings.Replace(body, "Will pay next week", " ", 1), "request.validation_failed", "note"},
		{"a resubmission date in the past", strings.Replace(body, lastHeld, "2000-01-01", 1), "collections.invalid_resubmission_date", "resubmission_date"},
		{"a resubmission date after the latest key date", strings.Replace(body, lastHeld, "2101-01-01", 1), "collections.invalid_resubmission_date",
			"resubmission_date"},
		{"a customer that is no business partner", strings.Replace(body, e.north.String(), uuid.NewString(), 1), "request.reference_not_found",
			"customer_id"},
		{"a company the caller may not act in", strings.Replace(body, e.DE.String(), uuid.NewString(), 1), "request.reference_not_found",
			"legal_entity_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(422, c.code, "POST", "/v1/customer-contacts", c.body, "")
			if !strings.Contains(mustJSON(t, p["errors"]), `"field":"`+c.field+`"`) {
				t.Errorf("errors: got %v, want one naming %s", p["errors"], c.field)
			}
		})
	}
	e.Problem(404, "request.not_found", "GET", "/v1/customer-contacts/"+uuid.NewString(), "", "")
}

// TestResubmissionAgainstToday: the resubmission date and the contact's
// time are judged by the endpoints' clock: today is refused naming it,
// tomorrow accepted; a contact a second after now is refused; a zone that
// does not load fails, as no refusal.
func TestResubmissionAgainstToday(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	h.Now = func() time.Time { return now }
	record := func(at time.Time, resubmission string) error {
		return e.inRequest(t, func(ctx context.Context) error {
			d := date(t, resubmission)
			_, err := h.RecordContact(ctx, collections.ContactInput{LegalEntityID: e.DE, CustomerID: e.north, ContactedAt: at, Outcome: "reached",
				Resubmission: &d})
			return err
		})
	}
	if p := problem.From(record(now, "2026-07-01")); p.Code != "collections.invalid_resubmission_date" || !strings.Contains(p.Detail, "2026-07-01") {
		t.Errorf("today: got %v, want collections.invalid_resubmission_date naming 2026-07-01", p)
	}
	if err := record(now, "2026-07-02"); err != nil {
		t.Errorf("tomorrow, contacted now: got %v, want the contact recorded", err)
	}
	if p := problem.From(record(now.Add(time.Second), "2026-07-02")); p.Code != "collections.invalid_contact_time" {
		t.Errorf("a second after now: got %v, want collections.invalid_contact_time", p)
	}
	h.Settings = badZone{h.Settings}
	var refusal *problem.Problem
	if err := record(now, "2026-07-02"); err == nil || errors.As(err, &refusal) {
		t.Errorf("a zone that does not load: got %v, want an error that is no refusal", err)
	}
}
