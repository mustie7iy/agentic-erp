package collections_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// dispute is the JSON of a dispute case on line 1 of the entry for the
// amount, with the fields extra adds.
func dispute(company uuid.UUID, entry ledger.Entry, amount, currency, extra string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1,
		"disputed_amount": {"amount": %q, "currency": %q}, "reason": "Priced above the quotation"%s}`, company, entry.ID, amount, currency, extra)
}

// open opens a case and returns its id and entity tag.
func (e env) open(t *testing.T, body string) (id, tag string) {
	t.Helper()
	got, tag := e.Must(201, "POST", "/v1/dispute-cases", body, "")
	return apitest.String(t, got, "id"), tag
}

// caseEvents are the payloads of the DisputeCaseChanged events published.
func (e env) caseEvents(t *testing.T) []map[string]any {
	t.Helper()
	return apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dispute_case_changed"}))
}

// TestDisputeCase: a case is opened on an open customer item for part of
// its amount with an assignee and a note, and reads back with what it
// names; it is reassigned and its note removed, then closed as rejected,
// once; each change is audited and published, naming the customer by id
// and number; nothing changes on the open items (T86a acceptance).
func TestDisputeCase(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	before := e.openItems(t)
	body := dispute(e.DE, entries[0], "40.00", "EUR", fmt.Sprintf(`, "assignee_id": %q, "note": "Ask sales"`, e.Tenant.Human.ID))
	got, tag := e.Must(201, "POST", "/v1/dispute-cases", body, "")
	id := apitest.String(t, got, "id")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	for f, want := range map[string]any{"status": "open", "company_code": "2000", "company_name": "Acme GmbH", "legal_entity_id": e.DE.String(),
		"customer_id": e.north.String(), "customer_number": "BP70000002", "customer_name": "Northwind Traders", "fiscal_year": float64(2026),
		"journal_entry_id": entries[0].ID.String(), "line_no": float64(1), "document_number": entries[0].DocumentNumber, "reference": "CI-A",
		"reason": "Priced above the quotation", "assignee_id": e.Tenant.Human.ID.String(), "assignee_name": "human", "note": "Ask sales"} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	for f, want := range map[string]string{"disputed_amount": "40.00 EUR", "local_disputed_amount": "40.00 EUR"} {
		if a := amount(t, got, f); a != want {
			t.Errorf("%s: got %s, want %s", f, a, want)
		}
	}
	for _, f := range []string{"close_reason", "closed_at", "closed_by", "closed_by_name"} {
		if v, ok := got[f]; ok {
			t.Errorf("%s of an open case: got %v, want it absent", f, v)
		}
	}
	read, readTag := e.Must(200, "GET", "/v1/dispute-cases/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b || readTag != tag {
		t.Errorf("read back: got %s (%s), want %s (%s)", a, readTag, b, tag)
	}
	patched, tag := e.Must(200, "PATCH", "/v1/dispute-cases/"+id, `{"assignee_id": null, "note": null}`, tag)
	if _, ok := patched["assignee_id"]; ok || patched["note"] != nil || patched["version"] != float64(2) {
		t.Errorf("patched: got assignee %v, note %v, version %v; want neither, version 2", patched["assignee_id"], patched["note"], patched["version"])
	}
	closed, tag := e.Must(200, "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "rejected", "reason": "Priced as quoted"}`, tag)
	for f, want := range map[string]any{"status": "rejected", "close_reason": "Priced as quoted", "closed_by": e.Tenant.Human.ID.String(),
		"closed_by_name": "human", "version": float64(3)} {
		if closed[f] != want {
			t.Errorf("closed, %s: got %v, want %v", f, closed[f], want)
		}
	}
	if _, ok := closed["closed_at"]; !ok {
		t.Error("closed_at: got none, want the time it was closed")
	}
	t.Run("closed once", func(t *testing.T) {
		e.In(t).Problem(409, "collections.dispute_closed", "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "accepted", "reason": "x"}`, tag)
		e.In(t).Problem(409, "collections.dispute_closed", "PATCH", "/v1/dispute-cases/"+id, `{"note": "Too late"}`, tag)
	})
	if after := e.openItems(t); after != before {
		t.Errorf("the open items: got %s, want them unchanged: %s", after, before)
	}
	e.caseRecorded(t, id, entries[0])
}

// caseRecorded checks the audit records and events of TestDisputeCase's
// case: opened, updated and closed, each naming the customer by id and
// number and carrying no free text.
func (e env) caseRecorded(t *testing.T, id string, entry ledger.Entry) {
	t.Helper()
	var audits string
	pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY occurred_at, action) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits)
	if want := "collections.dispute_case.open collections.dispute_case.update collections.dispute_case.close"; audits != want {
		t.Errorf("audit records: got %q, want %q", audits, want)
	}
	events := e.caseEvents(t)
	if len(events) != 3 {
		t.Fatalf("DisputeCaseChanged events: got %d, want 3", len(events))
	}
	want := map[string]any{"dispute_case_id": id, "change": "opened", "legal_entity_id": e.DE.String(), "legal_entity_code": "2000",
		"customer_id": e.north.String(), "customer_number": "BP70000002", "fiscal_year": float64(2026), "journal_entry_id": entry.ID.String(),
		"line_no": float64(1), "document_number": entry.DocumentNumber, "disputed_amount": map[string]any{"amount": "40.00", "currency": "EUR"},
		"assignee_id": e.Tenant.Human.ID.String(), "status": "open", "status_before": nil, "version": float64(1),
		"items": []any{map[string]any{"item_no": float64(1), "fiscal_year": float64(2026), "journal_entry_id": entry.ID.String(), "line_no": float64(1),
			"document_number": entry.DocumentNumber, "disputed_amount": map[string]any{"amount": "40.00", "currency": "EUR"}}}}
	if a, b := mustJSON(t, events[0]), mustJSON(t, want); a != b {
		t.Errorf("the opened event: got %s, want %s", a, b)
	}
	for i, w := range []string{"updated <nil> open open 2", "closed <nil> rejected open 3"} {
		ev := events[i+1]
		if got := fmt.Sprintf("%v %v %v %v %v", ev["change"], ev["assignee_id"], ev["status"], ev["status_before"], ev["version"]); got != w {
			t.Errorf("event %d: got %q, want %q", i+2, got, w)
		}
	}
	for _, text := range []string{"Northwind", "Priced", "Ask sales"} {
		apitest.NeverPublished(t, e.Tenant, "collections", text)
	}
}

// TestDisputeAccepted: a case closed as accepted posts nothing; an item in
// another currency is disputed in it, with its share of the item's amount
// in the company's currency as booked, rounded half away from zero; a
// case stays open when its item is cleared meanwhile, and is closed by a
// person; once it is closed the item takes a case again.
func TestDisputeAccepted(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	// 200.00 USD booked at 160.00 EUR: 33.33 USD is 26.664 EUR.
	id, tag := e.open(t, dispute(e.DE, entries[7], "33.33", "USD", ""))
	got, _ := e.Must(200, "GET", "/v1/dispute-cases/"+id, "", "")
	if a, b := amount(t, got, "disputed_amount"), amount(t, got, "local_disputed_amount"); a != "33.33 USD" || b != "26.66 EUR" {
		t.Errorf("amounts: got %s and %s, want 33.33 USD and 26.66 EUR", a, b)
	}
	var entriesBefore int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &entriesBefore)
	closed, _ := e.Must(200, "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "accepted", "reason": "Short delivery"}`, tag)
	if closed["status"] != "accepted" {
		t.Errorf("status: got %v, want accepted", closed["status"])
	}
	var entriesAfter int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &entriesAfter)
	if entriesAfter != entriesBefore {
		t.Errorf("journal entries: got %d after closing, want %d: closing posts nothing", entriesAfter, entriesBefore)
	}
	again, _ := e.open(t, dispute(e.DE, entries[7], "200.00", "USD", ""))
	got, _ = e.Must(200, "GET", "/v1/dispute-cases/"+again, "", "")
	if b := amount(t, got, "local_disputed_amount"); b != "160.00 EUR" {
		t.Errorf("all of the item disputed: got %s, want its local amount, 160.00 EUR", b)
	}
	t.Run("an item cleared meanwhile", func(t *testing.T) {
		cid, ctag := e.In(t).open(t, dispute(e.DE, entries[1], "50.00", "EUR", ""))
		payment := e.post(t, item{customer: e.north, amount: -5000, currency: "EUR", day: "2026-06-28", noTerms: true, ref: "PAYMENT"})[0]
		e.In(t).Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-06-30", "items": [
			{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
			e.DE, entries[1].ID, payment.ID), "")
		if got, _ := e.In(t).Must(200, "GET", "/v1/dispute-cases/"+cid, "", ""); got["status"] != "open" {
			t.Errorf("the case of the cleared item: got %v, want it open", got["status"])
		}
		e.In(t).Must(200, "POST", "/v1/dispute-cases/"+cid+":close", `{"outcome": "accepted", "reason": "Paid"}`, ctag)
	})
}

// TestDisputeRefusals: what a case is opened on and with, and who may
// work it; one open case per item; a patch that changes nothing; a stale
// version; another company's cases; the permissions.
func TestDisputeRefusals(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	outsider := e.Reader(t, "A person of company 1000", e.US)
	for _, c := range []struct {
		name, body, code, field string
		status                  int
	}{
		{"a line that is no open item", strings.Replace(dispute(e.DE, entries[0], "1.00", "EUR", ""), `"line_no": 1`, `"line_no": 2`, 1),
			"collections.item_not_open", "journal_entry_id", 422},
		{"a credit item", dispute(e.DE, entries[5], "1.00", "EUR", ""), "collections.item_not_open", "journal_entry_id", 422},
		{"another currency than the item's", dispute(e.DE, entries[0], "1.00", "USD", ""), "collections.currency_mismatch", "disputed_amount.currency", 422},
		{"an amount of the wrong form", dispute(e.DE, entries[0], "1.5", "EUR", ""), "request.invalid_amount", "disputed_amount.amount", 400},
		{"nothing disputed", dispute(e.DE, entries[0], "0.00", "EUR", ""), "collections.invalid_disputed_amount", "disputed_amount.amount", 422},
		{"more than is open", dispute(e.DE, entries[0], "100.01", "EUR", ""), "collections.invalid_disputed_amount", "disputed_amount.amount", 422},
		{"an agent as assignee", dispute(e.DE, entries[0], "1.00", "EUR", fmt.Sprintf(`, "assignee_id": %q`, e.Tenant.Agent.ID)),
			"collections.assignee_invalid", "assignee_id", 422},
		{"an assignee without the company's access", dispute(e.DE, entries[0], "1.00", "EUR", fmt.Sprintf(`, "assignee_id": %q`, outsider.ID)),
			"collections.assignee_invalid", "assignee_id", 422},
	} {
		t.Run(c.name, func(t *testing.T) {
			if f := field(e.In(t).Problem(c.status, c.code, "POST", "/v1/dispute-cases", c.body, "")); f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
		})
	}
	t.Run("a company the caller may not act in", func(t *testing.T) {
		got := e.As(outsider).In(t).Problem(422, "request.reference_not_found", "POST", "/v1/dispute-cases", dispute(e.DE, entries[0], "1.00", "EUR", ""), "")
		if f := field(got); f != "legal_entity_id" {
			t.Errorf("field: got %v, want legal_entity_id", f)
		}
	})
	id, tag := e.open(t, dispute(e.DE, entries[0], "100.00", "EUR", ""))
	t.Run("a second open case of the item", func(t *testing.T) {
		e.In(t).Problem(409, "collections.dispute_exists", "POST", "/v1/dispute-cases", dispute(e.DE, entries[0], "1.00", "EUR", ""), "")
	})
	t.Run("a patch that changes nothing", func(t *testing.T) {
		e.In(t).Problem(422, "collections.nothing_to_change", "PATCH", "/v1/dispute-cases/"+id, `{"assignee_id": null}`, tag)
	})
	t.Run("an assignee patched without the company's access", func(t *testing.T) {
		got := e.In(t).Problem(422, "collections.assignee_invalid", "PATCH", "/v1/dispute-cases/"+id, fmt.Sprintf(`{"assignee_id": %q}`, outsider.ID), tag)
		if f := field(got); f != "assignee_id" {
			t.Errorf("field: got %v, want assignee_id", f)
		}
	})
	t.Run("texts of white space and an unknown outcome", func(t *testing.T) {
		for _, c := range []struct{ method, path, body, code, field string }{
			{"POST", "/v1/dispute-cases", strings.Replace(dispute(e.DE, entries[1], "1.00", "EUR", ""), `"Priced above the quotation"`, `" "`, 1),
				"request.validation_failed", "reason"},
			{"POST", "/v1/dispute-cases", dispute(e.DE, entries[1], "1.00", "EUR", `, "note": "  "`), "request.validation_failed", "note"},
			{"PATCH", "/v1/dispute-cases/" + id, `{"note": " "}`, "request.validation_failed", "note"},
			{"POST", "/v1/dispute-cases/" + id + ":close", `{"outcome": "withdrawn", "reason": "x"}`, "collections.unknown_outcome", "outcome"},
			{"POST", "/v1/dispute-cases/" + id + ":close", `{"outcome": "accepted", "reason": " "}`, "request.validation_failed", "reason"},
		} {
			if f := field(e.In(t).Problem(422, c.code, c.method, c.path, c.body, tag)); f != c.field {
				t.Errorf("%s %s: field: got %v, want %s", c.method, c.path, f, c.field)
			}
		}
	})
	t.Run("texts longer than the contract allows", func(t *testing.T) {
		long := strings.Repeat("x", 501)
		e.In(t).Problem(400, "request.invalid_body", "POST", "/v1/dispute-cases",
			strings.Replace(dispute(e.DE, entries[1], "1.00", "EUR", ""), "Priced above the quotation", long, 1), "")
		e.In(t).Problem(400, "request.invalid_body", "PATCH", "/v1/dispute-cases/"+id, `{"note": "`+strings.Repeat("x", 2001)+`"}`, tag)
		e.In(t).Problem(400, "request.invalid_body", "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "accepted", "reason": "`+long+`"}`, tag)
	})
	t.Run("a stale version", func(t *testing.T) {
		e.In(t).Problem(412, "request.version_mismatch", "PATCH", "/v1/dispute-cases/"+id, `{"note": "x"}`, `"9"`)
		e.In(t).Problem(412, "request.version_mismatch", "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "rejected", "reason": "x"}`, `"9"`)
	})
	other := e.As(outsider)
	t.Run("another company's case", func(t *testing.T) {
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/dispute-cases/"+id, "", "")
		other.In(t).Problem(404, "request.not_found", "PATCH", "/v1/dispute-cases/"+id, `{"note": "x"}`, tag)
		other.In(t).Problem(404, "request.not_found", "POST", "/v1/dispute-cases/"+id+":close", `{"outcome": "rejected", "reason": "x"}`, tag)
		if page, _ := other.In(t).Must(200, "GET", "/v1/dispute-cases", "", ""); len(objects(t, page, "items")) != 0 {
			t.Errorf("listed: got %d cases, want none", len(objects(t, page, "items")))
		}
	})
	t.Run("a case that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/dispute-cases/"+uuid.NewString(), "", "")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'collections.dispute.write'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/dispute-cases", dispute(e.DE, entries[1], "1.00", "EUR", ""), "")
	})
}

// TestDisputeLists: cases are listed the latest first, filtered by
// company, customer, status and assignee, and paged.
func TestDisputeLists(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	assigned := fmt.Sprintf(`, "assignee_id": %q`, e.Tenant.Human.ID)
	a, _ := e.open(t, dispute(e.DE, entries[0], "1.00", "EUR", assigned))
	b, bTag := e.open(t, dispute(e.DE, entries[1], "1.00", "EUR", ""))
	c, _ := e.open(t, dispute(e.DE, entries[7], "1.00", "USD", ""))
	e.Must(200, "POST", "/v1/dispute-cases/"+b+":close", `{"outcome": "rejected", "reason": "x"}`, bTag)
	names := map[string]string{a: "a", b: "b", c: "c"}
	list := func(t *testing.T, query string) (string, map[string]any) {
		t.Helper()
		page, _ := e.In(t).Must(200, "GET", "/v1/dispute-cases"+query, "", "")
		var out []string
		for _, x := range objects(t, page, "items") {
			out = append(out, names[apitest.String(t, x, "id")])
		}
		return strings.Join(out, " "), page
	}
	for _, x := range []struct{ name, query, want string }{
		{"every case, the latest first", "", "c b a"},
		{"the oldest first", "?order_by=created_at%20asc", "a b c"},
		{"of the company", "?legal_entity_id=" + e.DE.String(), "c b a"},
		{"of another company", "?legal_entity_id=" + e.US.String(), ""},
		{"of a customer", "?customer_id=" + e.north.String(), "b a"},
		{"open", "?status=open", "c a"},
		{"rejected", "?status=rejected", "b"},
		{"assigned to a person", "?assignee_id=" + e.Tenant.Human.ID.String(), "a"},
	} {
		t.Run(x.name, func(t *testing.T) {
			if got, _ := list(t, x.query); got != x.want {
				t.Errorf("cases: got %q, want %q", got, x.want)
			}
		})
	}
	t.Run("paged", func(t *testing.T) {
		first, page := list(t, "?page_size=2")
		second, last := list(t, "?page_size=2&page_token="+apitest.String(t, page, "next_page_token"))
		if first != "c b" || second != "a" || last["next_page_token"] != nil {
			t.Errorf("pages: got %q then %q with next page %v, want \"c b\" then \"a\", the last", first, second, last["next_page_token"])
		}
	})
}

// TestDisputeRace: of two cases opened on one item at once, the second
// waits for the first and is refused, 409 collections.dispute_exists; of
// two closes of one case at once, the second waits and finds it closed.
func TestDisputeRace(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	h := endpoints(t)
	in := collections.CaseInput{LegalEntityID: e.DE, Items: []collections.CaseItemInput{{Item: ledger.ItemRef{FiscalYear: 2026, EntryID: entries[0].ID,
		LineNo: 1}, Amount: "1.00", Currency: "EUR"}}, Reason: "Twice"}
	open := func(ctx context.Context) error {
		_, err := h.OpenCase(ctx, in)
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, open, open)
	if p := problem.From(second); first != nil || p.Code != "collections.dispute_exists" || p.Status != 409 {
		t.Errorf("got %v and %v, want the first opened and the second refused as 409 collections.dispute_exists", first, second)
	}
	page, _ := e.Must(200, "GET", "/v1/dispute-cases", "", "")
	id := uuid.MustParse(apitest.String(t, objects(t, page, "items")[0], "id"))
	closeIt := func(ctx context.Context) error {
		_, err := h.CloseCase(ctx, id, func(int32) error { return nil }, collections.Rejected, "Twice")
		return err
	}
	first, second = apitest.Serialized(t, e.Tenant, e.Tenant.Human, closeIt, closeIt)
	if p := problem.From(second); first != nil || p.Code != "collections.dispute_closed" {
		t.Errorf("got %v and %v, want the first closed and the second refused as collections.dispute_closed", first, second)
	}
}

// TestIdempotentDispute: the same Idempotency-Key opens one case and
// answers it again.
func TestIdempotentDispute(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	req := apitest.Req{Method: "POST", Path: "/v1/dispute-cases", Body: dispute(e.DE, entries[0], "1.00", "EUR", ""), Tenant: e.Tenant.ID,
		As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || second.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice: %s", first.Code, second.Code, second.Body)
	}
	if a, b := apitest.JSON(t, first), apitest.JSON(t, second); a["id"] != b["id"] {
		t.Errorf("replay: got case %v, want the first answer's %v", b["id"], a["id"])
	}
	if loc, want := first.Header().Get("Location"), "/v1/dispute-cases/"+apitest.String(t, apitest.JSON(t, first), "id"); loc != want {
		t.Errorf("Location: got %s, want %s", loc, want)
	}
}
