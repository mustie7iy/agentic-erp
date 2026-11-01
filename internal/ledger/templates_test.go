package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Entry templates (ADR-0056): a person saves a manual entry without its
// dates, parks entries from it, changes and retires it; an agent's
// template waits for a person to take it on.

// templateBody is an entry template of legal entity le, in EUR, with the
// lines and, when not empty, the schedule (a JSON object).
func templateBody(le uuid.UUID, name, schedule string, lines ...line) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}}`, l.account, l.amount)
	}
	s := ""
	if schedule != "" {
		s = `, "schedule": ` + schedule
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "name": %q, "currency": "EUR", "header_text": "Monthly rent", "reference": "LEASE-1", "lines": [%s]%s}`,
		le, name, strings.Join(parts, ", "), s)
}

// etagOf is the entity tag of a representation's version.
func etagOf(m map[string]any) string { return fmt.Sprintf(`"%v"`, m["version"]) }

func TestEntryTemplates(t *testing.T) {
	e := setup(t)
	rent, bank := e.account(t, "2000", "400000"), e.account(t, "2000", "100000")
	monthly := `{"frequency": "monthly", "day_of_month": 31, "starts_on": "2026-01-15", "occurrences": 12}`
	tpl, tag := e.Must(201, "POST", "/v1/entry-templates", templateBody(e.de, "Office rent", monthly, line{rent, "4500.00", ""}, line{bank, "-4500.00", ""}), "")
	id := tpl["id"].(string)
	want(t, "template", tpl, map[string]any{"status": "active", "created_by_kind": "human", "owner_id": e.ten.Human.ID.String(), "name": "Office rent",
		"document_type": "SA", "currency": "EUR", "next_due_on": "2026-01-31", "last_occurrence_on": nil, "last_refusal": nil, "version": float64(1)})
	if tag != `"1"` {
		t.Errorf("template ETag: got %s, want \"1\"", tag)
	}
	if got := amounts(t, tpl, "amount"); got != "4500.00,-4500.00" {
		t.Errorf("template lines: got %s, want 4500.00,-4500.00", got)
	}
	want(t, "schedule", tpl["schedule"].(map[string]any), map[string]any{"frequency": "monthly", "day_of_month": float64(31), "starts_on": "2026-01-15",
		"occurrences": float64(12)})
	saved := events(t, e.ten, "entry_template_changed")
	if len(saved) != 1 {
		t.Fatalf("template events: got %d, want 1", len(saved))
	}
	want(t, "template event", saved[0], map[string]any{"entry_template_id": id, "status": "active", "previous_status": nil, "next_due_on": "2026-01-31"})
	if got, _ := e.Must(200, "GET", "/v1/entry-templates/"+id, "", ""); got["name"] != "Office rent" {
		t.Errorf("read back: got %v, want Office rent", got["name"])
	}
	t.Run("listed", func(t *testing.T) {
		e := e.in(t)
		page, _ := e.Must(200, "GET", "/v1/entry-templates?status=active&legal_entity_id="+e.de.String(), "", "")
		if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != id {
			t.Errorf("active templates: got %v, want %s", items, id)
		}
		if page, _ := e.Must(200, "GET", "/v1/entry-templates?status=retired", "", ""); len(page["items"].([]any)) != 0 {
			t.Errorf("retired templates: got %v, want none", page["items"])
		}
	})
	t.Run("refused", func(t *testing.T) {
		e := e.in(t)
		e.Problem(409, "ledger.template_name_taken", "POST", "/v1/entry-templates", templateBody(e.de, "Office rent", "", line{rent, "1.00", ""}, line{bank, "-1.00", ""}), "")
		e.Problem(422, "ledger.invalid_schedule", "POST", "/v1/entry-templates", templateBody(e.de, "Bad", `{"frequency": "weekly", "day_of_month": 1, "starts_on": "2026-01-01"}`,
			line{rent, "1.00", ""}, line{bank, "-1.00", ""}), "")
		e.Problem(422, "ledger.unbalanced", "POST", "/v1/entry-templates", templateBody(e.de, "Unbalanced", "", line{rent, "1.00", ""}, line{bank, "-2.00", ""}), "")
		e.Problem(404, "request.not_found", "GET", "/v1/entry-templates/"+uuid.NewString(), "", "")
		e.Problem(412, "request.version_mismatch", "PATCH", "/v1/entry-templates/"+id, `{"name": "Rent"}`, `"9"`)
		service := e
		service.As = e.ten.Service
		e.ten.AllCompanies(t, e.ten.Service)
		service.Problem(403, "ledger.not_template_owner", "PATCH", "/v1/entry-templates/"+id, `{"name": "Rent"}`, `"1"`)
		service.Problem(403, "ledger.not_template_owner", "POST", "/v1/entry-templates/"+id+":retire", "", `"1"`)
	})
	t.Run("parked from it", func(t *testing.T) {
		e := e.in(t)
		pe, _ := e.Must(201, "POST", "/v1/entry-templates/"+id+":park", `{"posting_date": "2026-03-10"}`, `"1"`)
		want(t, "parked entry", pe, map[string]any{"template_id": id, "occurrence_date": nil, "created_by_kind": "human", "owner_id": e.ten.Human.ID.String(),
			"posting_date": "2026-03-10", "document_date": "2026-03-10", "reference": "LEASE-1", "header_text": "Monthly rent", "status": "parked"})
		other, _ := e.Must(201, "POST", "/v1/entry-templates/"+id+":park", `{"posting_date": "2026-03-10", "document_date": "2026-03-01", "reference": "MARCH"}`, `"1"`)
		want(t, "parked entry", other, map[string]any{"document_date": "2026-03-01", "reference": "MARCH"})
		page, _ := e.Must(200, "GET", "/v1/parked-journal-entries?template_id="+id, "", "")
		if n := len(page["items"].([]any)); n != 2 {
			t.Errorf("entries of the template: got %d, want 2", n)
		}
		if tpl, _ := e.Must(200, "GET", "/v1/entry-templates/"+id, "", ""); tpl["version"] != float64(1) || tpl["last_occurrence_on"] != nil {
			t.Errorf("template after use: got version %v, last %v, want 1 and none", tpl["version"], tpl["last_occurrence_on"])
		}
		e.Problem(412, "request.version_mismatch", "POST", "/v1/entry-templates/"+id+":park", `{"posting_date": "2026-03-10"}`, `"7"`)
	})
	t.Run("changed", func(t *testing.T) {
		e := e.in(t)
		got, _ := e.Must(200, "PATCH", "/v1/entry-templates/"+id, `{"name": "Rent", "header_text": null, "schedule": null, "lines": [
			{"account_id": "`+rent+`", "amount": {"amount": "4600.00", "currency": "EUR"}}, {"account_id": "`+bank+`", "amount": {"amount": "-4600.00", "currency": "EUR"}}]}`, `"1"`)
		want(t, "changed template", got, map[string]any{"name": "Rent", "header_text": nil, "schedule": nil, "next_due_on": nil, "version": float64(2)})
		if a := amounts(t, got, "amount"); a != "4600.00,-4600.00" {
			t.Errorf("changed lines: got %s, want 4600.00,-4600.00", a)
		}
		got, _ = e.Must(200, "PATCH", "/v1/entry-templates/"+id, `{"schedule": {"frequency": "quarterly", "day_of_month": 1, "starts_on": "2026-04-01",
			"ends_on": "2026-12-31"}, "document_type": "AB", "currency": "EUR", "reference": "LEASE-2"}`, `"2"`)
		want(t, "rescheduled template", got, map[string]any{"next_due_on": "2026-04-01", "document_type": "AB", "reference": "LEASE-2"})
	})
	t.Run("in one ledger", func(t *testing.T) {
		e := e.in(t)
		body := strings.Replace(templateBody(e.de, "Ledger only", "", line{rent, "1.00", ""}, line{bank, "-1.00", ""}), `"currency"`,
			fmt.Sprintf(`"ledger_id": %q, "currency"`, tpl["ledger_id"]), 1)
		one, _ := e.Must(201, "POST", "/v1/entry-templates", body, "")
		want(t, "ledger-specific template", one, map[string]any{"ledger_specific": true, "ledger_id": tpl["ledger_id"]})
		one, _ = e.Must(200, "PATCH", "/v1/entry-templates/"+one["id"].(string), `{"header_text": "Still one ledger"}`, `"1"`)
		want(t, "changed", one, map[string]any{"ledger_specific": true})
		pe, _ := e.Must(201, "POST", "/v1/entry-templates/"+one["id"].(string)+":park", `{"posting_date": "2026-03-10"}`, `"2"`)
		want(t, "its entry", pe, map[string]any{"ledger_specific": true, "ledger_id": tpl["ledger_id"]})
		page, _ := e.Must(200, "GET", "/v1/entry-templates?page_size=1", "", "")
		if page["next_page_token"] == nil {
			t.Fatalf("first page of two: got no next page token")
		}
		next, _ := e.Must(200, "GET", "/v1/entry-templates?page_size=1&page_token="+page["next_page_token"].(string), "", "")
		if items := next["items"].([]any); len(items) != 1 {
			t.Errorf("second page: got %d templates, want 1", len(items))
		}
		e.Problem(400, "request.invalid_page_token", "GET", "/v1/entry-templates?page_token=nonsense", "", "")
		e.Problem(422, "ledger.unknown_unit", "POST", "/v1/entry-templates", fmt.Sprintf(`{"legal_entity_id": %q, "name": "Units", "currency": "EUR",
			"lines": [{"account_id": %q, "amount": {"amount": "1.00", "currency": "EUR"}, "quantity": {"value": "1", "unit": "ZZZ"}},
			{"account_id": %q, "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, e.de, rent, bank), "")
	})
	t.Run("retired", func(t *testing.T) {
		e := e.in(t)
		got, _ := e.Must(200, "POST", "/v1/entry-templates/"+id+":retire", "", `"3"`)
		want(t, "retired template", got, map[string]any{"status": "retired", "next_due_on": nil})
		retired := events(t, e.ten, "entry_template_changed")
		want(t, "retirement event", retired[len(retired)-1], map[string]any{"status": "retired", "previous_status": "active"})
		e.Problem(409, "ledger.template_retired", "PATCH", "/v1/entry-templates/"+id, `{"name": "Again"}`, `"4"`)
		e.Problem(409, "ledger.template_retired", "POST", "/v1/entry-templates/"+id+":retire", "", `"4"`)
		e.Problem(409, "ledger.template_retired", "POST", "/v1/entry-templates/"+id+":park", `{"posting_date": "2026-03-10"}`, `"4"`)
	})
}

// TestAgentTemplates: an agent's template has no owner until a person
// takes it on by changing it; the agent changes it until then.
func TestAgentTemplates(t *testing.T) {
	e := setup(t)
	rent, bank := e.account(t, "2000", "400000"), e.account(t, "2000", "100000")
	agent := e.asAgent(t)
	tpl, _ := agent.Must(201, "POST", "/v1/entry-templates", templateBody(e.de, "Proposed accrual", "", line{rent, "10.00", ""}, line{bank, "-10.00", ""}), "")
	id := tpl["id"].(string)
	want(t, "agent's template", tpl, map[string]any{"created_by_kind": "agent", "owner_id": nil})
	tpl, _ = agent.Must(200, "PATCH", "/v1/entry-templates/"+id, `{"name": "Accrual"}`, etagOf(tpl))
	want(t, "changed by its agent", tpl, map[string]any{"owner_id": nil, "name": "Accrual"})
	tpl, _ = e.Must(200, "PATCH", "/v1/entry-templates/"+id, `{"header_text": "Taken on"}`, etagOf(tpl))
	want(t, "taken on", tpl, map[string]any{"owner_id": e.ten.Human.ID.String()})
	agent.Problem(403, "ledger.not_template_owner", "PATCH", "/v1/entry-templates/"+id, `{"name": "Mine"}`, etagOf(tpl))
	var takenOn bool
	pgtest.Scan(t, `SELECT (detail->>'taken_on')::boolean FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.entry_template.update'
		AND object_id = $2 ORDER BY occurred_at DESC LIMIT 1`, []any{e.ten.ID, uuid.MustParse(id)}, &takenOn)
	if !takenOn {
		t.Error("audit of the change: got taken_on false, want true")
	}
}

// TestOwnedBy: owned_by=me lists the caller's own parked entries; an
// agent owns none, and any other value is refused.
func TestOwnedBy(t *testing.T) {
	e := setup(t)
	rent, bank := e.account(t, "2000", "400000"), e.account(t, "2000", "100000")
	mine, _ := e.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "EUR", line{rent, "1.00", ""}, line{bank, "-1.00", ""}), "")
	agent := e.asAgent(t)
	agent.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "EUR", line{rent, "2.00", ""}, line{bank, "-2.00", ""}), "")
	page, _ := e.Must(200, "GET", "/v1/parked-journal-entries?owned_by=me&status=parked", "", "")
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != mine["id"] {
		t.Errorf("my entries: got %v, want only %v", items, mine["id"])
	}
	if page, _ := agent.Must(200, "GET", "/v1/parked-journal-entries?owned_by=me", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("an agent's entries: got %v, want none", page["items"])
	}
	e.Problem(400, "request.invalid_parameter", "GET", "/v1/parked-journal-entries?owned_by=someone", "", "")
}

// TestTemplateDatabaseFailures calls every template handler outside a
// unit of work and with each of its database statements failing in turn.
func TestTemplateDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	rent, bank := uuid.MustParse(e.account(t, "2000", "400000")), uuid.MustParse(e.account(t, "2000", "100000"))
	tpl, _ := e.Must(201, "POST", "/v1/entry-templates", templateBody(e.de, "Rent", "", line{rent.String(), "1.00", ""}, line{bank.String(), "-1.00", ""}), "")
	id := uuid.MustParse(tpl["id"].(string))
	lines := []gen.JournalLineInput{{AccountID: rent, Amount: gen.Money{Amount: "2.00", Currency: "EUR"}, Quantity: gen.NewOptQuantity(gen.Quantity{Value: "1", Unit: "EA"})},
		{AccountID: bank, Amount: gen.Money{Amount: "-2.00", Currency: "EUR"}}}
	schedule := gen.EntrySchedule{Frequency: "monthly", DayOfMonth: 1, StartsOn: date("2026-01-01")}
	n := 0
	for name, call := range map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			n++
			_, err := h.CreateEntryTemplate(ctx, &gen.EntryTemplateCreate{LegalEntityID: e.de, Name: fmt.Sprintf("Template %d", n), Currency: "EUR",
				Lines: lines, Schedule: gen.NewOptEntrySchedule(schedule)}, gen.CreateEntryTemplateParams{})
			return err
		},
		"create unscheduled": func(ctx context.Context) error {
			n++
			_, err := h.CreateEntryTemplate(ctx, &gen.EntryTemplateCreate{LegalEntityID: e.de, Name: fmt.Sprintf("Template %d", n), Currency: "EUR",
				Lines: lines}, gen.CreateEntryTemplateParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetEntryTemplate(ctx, gen.GetEntryTemplateParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListEntryTemplates(ctx, gen.ListEntryTemplatesParams{})
			return err
		},
		"change": func(ctx context.Context) error {
			_, err := h.UpdateEntryTemplate(ctx, &gen.EntryTemplateUpdate{Lines: lines,
				Schedule: gen.NewOptNilNullableEntrySchedule(gen.NullableEntrySchedule(schedule))}, gen.UpdateEntryTemplateParams{ID: id, IfMatch: "*"})
			return err
		},
		"park from": func(ctx context.Context) error {
			_, err := h.ParkFromEntryTemplate(ctx, &gen.EntryTemplateUse{PostingDate: date("2026-03-15")}, gen.ParkFromEntryTemplateParams{ID: id, IfMatch: "*"})
			return err
		},
		"list parked by owner": func(ctx context.Context) error {
			_, err := h.ListParkedJournalEntries(ctx, gen.ListParkedJournalEntriesParams{OwnedBy: gen.NewOptString("me"), TemplateID: gen.NewOptUUID(id)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
	t.Run("retire", func(t *testing.T) {
		retire := func(ctx context.Context) error {
			_, err := h.RetireEntryTemplate(ctx, gen.RetireEntryTemplateParams{ID: id, IfMatch: "*"})
			return err
		}
		if err := retire(context.Background()); err == nil {
			t.Error("outside a unit of work: got no error, want one")
		}
		apitest.EveryFailure(t, e.ten, e.ten.Human, "retire", retire)
	})
}
