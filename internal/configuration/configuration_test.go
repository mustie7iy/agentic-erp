package configuration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"config.change.read", "config.change.propose", "config.change.activate", "config.version.read"}

// document is the prototype's valid configuration (company 1000) with a
// supplier invoice number range.
func document(t *testing.T) *config.Config {
	t.Helper()
	b, err := os.ReadFile("../config/testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	c.NumberRanges = []config.NumberRange{{Company: "1000", DocumentType: "supplier_invoice", FiscalYear: 2026, Prefix: "SI-", First: 1, Last: 99999, Gapless: true}}
	return c
}

func jsonOf(t *testing.T, c *config.Config) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type env struct {
	apitest.Client
	ten pgtest.Tenant
	le  uuid.UUID // legal entity 1000
}

// setup creates a tenant with legal entities 1000 and 2000 and, unless
// bare, bootstraps its configuration with document(t).
func setup(t *testing.T, bare bool) env {
	t.Helper()
	ten := pgtest.NewTenant(t, perms...)
	ten.AllCompanies(t, ten.Human)
	e := env{Client: apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}, ten: ten, le: uuid.Must(uuid.NewV7())}
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '1000', 'Acme Corp US', 'US', 'USD', gen_random_uuid(), 'active', $3, $3),
		($1, gen_random_uuid(), '2000', 'Acme GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $3, $3)`, ten.ID, e.le, ten.Human.ID)
	for _, r := range document(t).RoleNames() {
		pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $2, 'active', $3, $3)`,
			ten.ID, r, ten.Human.ID)
	}
	if !bare {
		e.bootstrap(t, jsonOf(t, document(t)))
	}
	return e
}

func (e env) bootstrap(t *testing.T, doc string) error {
	t.Helper()
	return apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, deps(), json.RawMessage(doc))
		return err
	})
}

// propose creates a change request on version base.
func (e env) propose(t *testing.T, base int, doc *config.Config) map[string]any {
	t.Helper()
	cr, _ := e.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Change", "rationale": "Because", "base_version": %d, "proposed": %s}`,
		base, jsonOf(t, doc)), "")
	return cr
}

func (e env) transition(t *testing.T, cr map[string]any, action string) (int, map[string]any) {
	t.Helper()
	cur, tag := e.Must(200, "GET", "/v1/change-requests/"+cr["id"].(string), "", "")
	code, m, _ := e.Do("POST", "/v1/change-requests/"+cur["id"].(string)+":"+action, "", tag)
	return code, m
}

func events(t *testing.T, ten pgtest.Tenant, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, ten, outbox.Event{Context: "config", Name: name})
}

func TestBootstrap(t *testing.T) {
	e := setup(t, true)
	if err := e.bootstrap(t, `{"version": 1}`); err == nil || !strings.Contains(err.Error(), "config.validation_failed") {
		t.Fatalf("invalid document: %v", err)
	}
	if err := e.bootstrap(t, `{"nonsense": true}`); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("unreadable document: %v", err)
	}
	foreign := document(t)
	foreign.NumberRanges[0].Company = "9999"
	if err := e.bootstrap(t, jsonOf(t, foreign)); err == nil || !strings.Contains(err.Error(), "config.validation_failed") {
		t.Fatalf("unknown company: %v", err)
	}
	// Change requests need a configuration to change.
	e.Problem(409, "config.not_bootstrapped", "POST", "/v1/change-requests", `{"title": "T", "base_version": 1, "proposed": {}}`, "")

	if err := e.bootstrap(t, jsonOf(t, document(t))); err != nil {
		t.Fatal(err)
	}
	if err := e.bootstrap(t, jsonOf(t, document(t))); err == nil || !strings.Contains(err.Error(), "config.already_bootstrapped") {
		t.Fatalf("second bootstrap: %v", err)
	}
	list, _ := e.Must(200, "GET", "/v1/config-versions", "", "")
	items := list["items"].([]any)
	v := items[0].(map[string]any)
	if len(items) != 1 || v["number"] != float64(1) || v["source"] != "bootstrap" || v["is_active"] != true || v["change_request_id"] != nil ||
		v["document"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("versions: %v", list)
	}
	got, tag := e.Must(200, "GET", "/v1/config-versions/"+v["id"].(string), "", "")
	if got["number"] != float64(1) || len(tag) < 10 {
		t.Fatal(got, tag)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/config-versions/"+uuid.NewString(), "", "")
	ranges, _ := e.Must(200, "GET", "/v1/number-ranges", "", "")
	r := ranges["items"].([]any)[0].(map[string]any)
	if r["legal_entity_id"] != e.le.String() || r["prefix"] != "SI-" || r["next_value"] != float64(1) || r["is_gapless"] != true {
		t.Fatalf("ranges: %v", ranges)
	}
	activated := events(t, e.ten, "configuration_activated")
	if len(activated) != 1 || activated[0]["source"] != "bootstrap" || activated[0]["previous_version"] != nil || len(activated[0]["hash"].(string)) != 64 {
		t.Fatalf("events: %v", activated)
	}
}

func TestNumberRangeChange(t *testing.T) {
	e := setup(t, false)
	doc := document(t)
	doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "payment", FiscalYear: 2026, First: 100, Last: 999})
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/change-requests", Tenant: e.ten.ID, As: e.ten.Human, Key: apitest.NewKey(),
		Body: fmt.Sprintf(`{"title": "Payment numbers 2026", "base_version": 1, "proposed": %s}`, jsonOf(t, doc))})
	cr := apitest.JSON(t, r)
	if r.Code != 201 || r.Header().Get("Location") != "/v1/change-requests/"+cr["id"].(string) || cr["number"] != "CR-000001" ||
		cr["status"] != "draft" || cr["author_id"] != e.ten.Human.ID.String() || len(cr["items"].([]any)) != 0 || cr["rationale"] != "" {
		t.Fatalf("create: %d %v", r.Code, cr)
	}
	path := "/v1/change-requests/" + cr["id"].(string)
	got, _ := e.Must(200, "PATCH", path, `{"title": "Payment number range 2026", "rationale": "New document type"}`, `"1"`)
	if got["title"] != "Payment number range 2026" || got["version"] != float64(2) {
		t.Fatal(got)
	}
	got, _ = e.Must(200, "POST", path+":submit", "", `"2"`)
	items := got["items"].([]any)
	if got["status"] != "approved" || got["tier"] != float64(1) || len(items) != 1 ||
		items[0].(map[string]any)["path"] != "number_ranges[1000/payment/2026]" || items[0].(map[string]any)["new_value"] != "(added)" ||
		items[0].(map[string]any)["old_value"] != nil {
		t.Fatalf("submit: %v", got)
	}
	e.Problem(409, "config.invalid_status", "POST", path+":submit", "", `"3"`)
	got, _ = e.Must(200, "POST", path+":activate", "", `"3"`)
	if got["status"] != "active" || got["activated_version"] != float64(2) {
		t.Fatalf("activate: %v", got)
	}
	e.Problem(409, "config.invalid_status", "POST", path+":activate", "", `"4"`)

	versions, _ := e.Must(200, "GET", "/v1/config-versions", "", "")
	vs := versions["items"].([]any)
	if len(vs) != 2 || vs[0].(map[string]any)["number"] != float64(2) || vs[0].(map[string]any)["is_active"] != true ||
		vs[0].(map[string]any)["change_request_id"] != cr["id"] || vs[1].(map[string]any)["is_active"] != false {
		t.Fatalf("versions: %v", versions)
	}
	payment, _ := e.Must(200, "GET", "/v1/number-ranges?document_type=payment", "", "")
	p := payment["items"].([]any)[0].(map[string]any)
	if p["first_value"] != float64(100) || p["next_value"] != float64(100) || p["prefix"] != "" || p["is_gapless"] != false {
		t.Fatalf("payment range: %v", p)
	}

	// Raising the limit of a range keeps its issued numbers.
	pgtest.Exec(t, `SELECT config.next_document_number($1, $2, 'supplier_invoice', 2026::smallint)`, e.ten.ID, e.le)
	raised := document(t)
	raised.NumberRanges = append(raised.NumberRanges, doc.NumberRanges[1])
	raised.NumberRanges[0].Last = 199999
	cr2 := e.propose(t, 2, raised)
	if code, m := e.transition(t, cr2, "submit"); code != 200 || m["tier"] != float64(1) {
		t.Fatalf("submit raise: %d %v", code, m)
	}
	if code, m := e.transition(t, cr2, "activate"); code != 200 {
		t.Fatalf("activate raise: %d %v", code, m)
	}
	si, _ := e.Must(200, "GET", "/v1/number-ranges?document_type=supplier_invoice&fiscal_year=2026&legal_entity_id="+e.le.String(), "", "")
	s := si["items"].([]any)[0].(map[string]any)
	if s["last_value"] != float64(199999) || s["next_value"] != float64(2) || s["version"] != float64(3) {
		t.Fatalf("raised range: %v", s)
	}
	_, tag := e.Must(200, "GET", "/v1/number-ranges/"+s["id"].(string), "", "")
	if tag != `"3"` {
		t.Fatal(tag)
	}

	submitted := events(t, e.ten, "change_request_submitted")
	validated := events(t, e.ten, "change_request_validated")
	approved := events(t, e.ten, "change_request_approved")
	activated := events(t, e.ten, "configuration_activated")
	if len(submitted) != 2 || submitted[0]["item_count"] != float64(1) || submitted[0]["title"] != "Payment number range 2026" ||
		len(validated) != 2 || validated[0]["outcome"] != "approved" || len(approved) != 2 ||
		len(activated) != 3 || activated[1]["previous_version"] != float64(1) || activated[1]["change_request_id"] != cr["id"] {
		t.Fatalf("events: %v %v %v %v", submitted, validated, approved, activated)
	}
}

func TestSubmitRefusals(t *testing.T) {
	e := setup(t, false)
	refuse := func(name string, doc string, code, field string) {
		t.Helper()
		cr, _ := e.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "T", "base_version": 1, "proposed": %s}`, doc), "")
		code2, m := e.transition(t, cr, "submit")
		if code2 != 422 || m["code"] != code || (field != "" && !strings.Contains(fmt.Sprint(m["errors"]), field)) {
			t.Errorf("%s: %d %v", name, code2, m)
		}
		if got, _ := e.Must(200, "GET", "/v1/change-requests/"+cr["id"].(string), "", ""); got["status"] != "draft" {
			t.Errorf("%s: status %v", name, got["status"])
		}
	}
	refuse("no change", jsonOf(t, document(t)), "config.no_changes", "")
	refuse("unreadable", `{"nonsense": 1}`, "config.validation_failed", "unreadable")
	invalid := document(t)
	invalid.Output.PurchaseOrderChannel = "pigeon"
	refuse("invalid", jsonOf(t, invalid), "config.validation_failed", "proposed.output.purchase_order_channel")
	removed := document(t)
	removed.NumberRanges = nil
	refuse("range removed", jsonOf(t, removed), "config.validation_failed", "cannot be removed")
	prefix := document(t)
	prefix.NumberRanges[0].Prefix = "XX-"
	refuse("prefix changed", jsonOf(t, prefix), "config.validation_failed", "only the last number")
	lowered := document(t)
	lowered.NumberRanges[0].Last = 10
	refuse("limit lowered", jsonOf(t, lowered), "config.validation_failed", "can only be raised")
	foreign := document(t)
	foreign.NumberRanges = append(foreign.NumberRanges, config.NumberRange{Company: "3000", DocumentType: "payment", FiscalYear: 2026, First: 1, Last: 9})
	refuse("unknown company", jsonOf(t, foreign), "config.validation_failed", "no legal entity has company code 3000")

	e.Problem(422, "config.unknown_base_version", "POST", "/v1/change-requests", `{"title": "T", "base_version": 7, "proposed": {}}`, "")
	e.Problem(404, "request.not_found", "GET", "/v1/change-requests/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "POST", "/v1/change-requests/"+uuid.NewString()+":submit", "", `"1"`)
	cr := e.propose(t, 1, document(t))
	e.Problem(412, "request.version_mismatch", "POST", "/v1/change-requests/"+cr["id"].(string)+":submit", "", `"9"`)
}

func TestTiersEvidenceAndWithdrawal(t *testing.T) {
	e := setup(t, false)
	terms := document(t)
	terms.PaymentTerms[0].NetDays += 15 // tier 2: operational
	pending := e.propose(t, 1, terms)
	if code, m := e.transition(t, pending, "submit"); code != 200 || m["status"] != "pending_approval" || m["tier"] != float64(2) || m["workflow_request_id"] == nil {
		t.Fatalf("tier 2: %d %v", code, m)
	}
	if code, m := e.transition(t, pending, "activate"); code != 409 || m["code"] != "config.invalid_status" {
		t.Fatalf("activate pending: %d %v", code, m)
	}
	if code, m := e.transition(t, pending, "simulate"); code != 409 {
		t.Fatalf("simulate pending: %d %v", code, m)
	}

	tax := document(t)
	tax.Companies[0].TaxCodes[0].RatePercent = "9" // tier 3: posting-relevant
	evidence := e.propose(t, 1, tax)
	if code, m := e.transition(t, evidence, "submit"); code != 200 || m["status"] != "awaiting_evidence" || m["tier"] != float64(3) {
		t.Fatalf("tier 3: %d %v", code, m)
	}
	cur, tag := e.Must(200, "GET", "/v1/change-requests/"+evidence["id"].(string), "", "")
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/change-requests/" + cur["id"].(string) + ":simulate", Tenant: e.ten.ID, As: e.ten.Human,
		Key: apitest.NewKey(), IfMatch: tag})
	op := apitest.JSON(t, r)
	if r.Code != 202 || op["kind"] != "config.simulate" || op["status"] != "queued" || r.Header().Get("Location") != "/v1/operations/"+op["id"].(string) {
		t.Fatalf("simulate: %d %v", r.Code, op)
	}
	e.Problem(412, "request.version_mismatch", "POST", "/v1/change-requests/"+cur["id"].(string)+":simulate", "", `"99"`)
	e.Problem(404, "request.not_found", "POST", "/v1/change-requests/"+uuid.NewString()+":simulate", "", `"1"`)

	// Only the author changes or withdraws a request.
	other := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	other.Subject = "human|" + other.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, 'Other', 'active', $2, $2)`, e.ten.ID, other.ID, other.Subject)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, role_id, current_date - 1, $2 FROM identity.principal_role WHERE principal_id = $3`, e.ten.ID, other.ID, e.ten.Human.ID)
	colleague := apitest.Client{T: t, API: e.API, Tenant: e.ten, As: other}
	draft := e.propose(t, 1, terms)
	colleague.Problem(403, "config.not_author", "PATCH", "/v1/change-requests/"+draft["id"].(string), `{"title": "Mine"}`, `"1"`)
	colleague.Problem(403, "config.not_author", "POST", "/v1/change-requests/"+draft["id"].(string)+":withdraw", `{"reason": "r"}`, `"1"`)

	got, _ := e.Must(200, "POST", "/v1/change-requests/"+pending["id"].(string)+":withdraw", `{"reason": "Superseded"}`, `"2"`)
	if got["status"] != "withdrawn" {
		t.Fatal(got)
	}
	e.Problem(409, "config.invalid_status", "POST", "/v1/change-requests/"+pending["id"].(string)+":withdraw", `{"reason": "r"}`, `"3"`)
	e.Problem(409, "config.invalid_status", "PATCH", "/v1/change-requests/"+pending["id"].(string), `{"title": "x"}`, `"3"`)
	e.Problem(412, "request.version_mismatch", "PATCH", "/v1/change-requests/"+draft["id"].(string), `{"title": "x"}`, `"7"`)
	e.Problem(404, "request.not_found", "PATCH", "/v1/change-requests/"+uuid.NewString(), `{"title": "x"}`, `"1"`)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/change-requests/"+draft["id"].(string)+":withdraw", `{"reason": "r"}`, `"7"`)
	e.Problem(404, "request.not_found", "POST", "/v1/change-requests/"+uuid.NewString()+":withdraw", `{"reason": "r"}`, `"1"`)
	// An invalid request (from asynchronous validation) returns to draft when changed.
	pgtest.Exec(t, `UPDATE config.change_request SET status = 'invalid', version = version + 1 WHERE id = $1`, uuid.MustParse(draft["id"].(string)))
	got, _ = e.Must(200, "PATCH", "/v1/change-requests/"+draft["id"].(string), fmt.Sprintf(`{"base_version": 1, "proposed": %s}`, jsonOf(t, tax)), `"2"`)
	if got["status"] != "draft" || got["proposed"].(map[string]any)["companies"] == nil {
		t.Fatal(got)
	}

	withdrawn := events(t, e.ten, "change_request_withdrawn")
	validated := events(t, e.ten, "change_request_validated")
	if len(withdrawn) != 1 || withdrawn[0]["previous_status"] != "pending_approval" ||
		len(validated) != 2 || validated[0]["outcome"] != "pending_approval" || validated[1]["outcome"] != "awaiting_evidence" ||
		len(events(t, e.ten, "change_request_approved")) != 0 {
		t.Fatalf("events: %v %v", withdrawn, validated)
	}

	// Lists of change requests: newest first, by status.
	list := func(q string) []any {
		m, _ := e.Must(200, "GET", "/v1/change-requests"+q, "", "")
		return m["items"].([]any)
	}
	if all := list(""); len(all) != 3 || all[0].(map[string]any)["id"] != draft["id"] {
		t.Fatalf("all: %v", all)
	}
	if w := list("?status=withdrawn"); len(w) != 1 {
		t.Fatalf("withdrawn: %v", w)
	}
	page, _ := e.Must(200, "GET", "/v1/change-requests?order_by=created_at%20asc&page_size=2", "", "")
	rest, _ := e.Must(200, "GET", "/v1/change-requests?order_by=created_at%20asc&page_size=2&page_token="+page["next_page_token"].(string), "", "")
	if page["items"].([]any)[0].(map[string]any)["id"] != pending["id"] || len(rest["items"].([]any)) != 1 {
		t.Fatalf("pages: %v %v", page, rest)
	}
}

func TestStaleBaseVersion(t *testing.T) {
	e := setup(t, false)
	first, second := document(t), document(t)
	first.NumberRanges[0].Last = 100000
	second.NumberRanges[0].Last = 100001
	a, b := e.propose(t, 1, first), e.propose(t, 1, second)
	e.transition(t, a, "submit")
	e.transition(t, b, "submit")
	if code, m := e.transition(t, a, "activate"); code != 200 {
		t.Fatalf("first: %d %v", code, m)
	}
	if code, m := e.transition(t, b, "activate"); code != 409 || m["code"] != "config.base_version_stale" {
		t.Fatalf("second: %d %v", code, m)
	}
	e.Problem(404, "request.not_found", "POST", "/v1/change-requests/"+uuid.NewString()+":activate", "", `"1"`)
	cr, tag := e.Must(200, "GET", "/v1/change-requests/"+b["id"].(string), "", "")
	if tag == `"99"` {
		t.Fatal(tag)
	}
	e.Problem(412, "request.version_mismatch", "POST", "/v1/change-requests/"+cr["id"].(string)+":activate", "", `"99"`)
}

func TestVersionAndRangeLists(t *testing.T) {
	e := setup(t, false)
	// Version 2 adds payment numbers for 2026, version 3 for 2027.
	doc := document(t)
	for i, year := range []int{2026, 2027} {
		doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "payment", FiscalYear: year, First: 1, Last: 9})
		cr := e.propose(t, i+1, doc)
		e.transition(t, cr, "submit")
		if code, m := e.transition(t, cr, "activate"); code != 200 {
			t.Fatalf("activate %d: %d %v", i, code, m)
		}
	}
	numbers := func(q string) (out []any) {
		m, _ := e.Must(200, "GET", "/v1/config-versions"+q, "", "")
		for _, it := range m["items"].([]any) {
			out = append(out, it.(map[string]any)["number"])
		}
		if tok, ok := m["next_page_token"]; ok {
			out = append(out, tok)
		}
		return out
	}
	if got := numbers("?is_active=false"); fmt.Sprint(got) != "[2 1]" {
		t.Fatalf("inactive: %v", got)
	}
	if got := numbers("?number=2"); fmt.Sprint(got) != "[2]" {
		t.Fatalf("number: %v", got)
	}
	page := numbers("?order_by=number%20asc&page_size=2")
	if fmt.Sprint(page[:2]) != "[1 2]" || len(page) != 3 {
		t.Fatalf("page: %v", page)
	}
	if rest := numbers("?order_by=number%20asc&page_size=2&page_token=" + page[2].(string)); fmt.Sprint(rest) != "[3]" {
		t.Fatalf("rest: %v", rest)
	}
	years := func(q string) (out []string) {
		m, _ := e.Must(200, "GET", "/v1/number-ranges"+q, "", "")
		for _, it := range m["items"].([]any) {
			r := it.(map[string]any)
			out = append(out, fmt.Sprintf("%s/%v", r["document_type"], r["fiscal_year"]))
		}
		if tok, ok := m["next_page_token"].(string); ok {
			out = append(out, tok)
		}
		return out
	}
	byYear := years("?order_by=fiscal_year%20desc&page_size=1")
	if byYear[0] != "payment/2027" {
		t.Fatalf("by year: %v", byYear)
	}
	if rest := years("?order_by=fiscal_year%20desc&page_token=" + byYear[1]); len(rest) != 2 {
		t.Fatalf("by year, rest: %v", rest)
	}
	byType := years("?page_size=2")
	if strings.Join(byType[:2], ",") != "payment/2026,payment/2027" && strings.Join(byType[:2], ",") != "payment/2027,payment/2026" {
		t.Fatalf("by type: %v", byType)
	}
	if rest := years("?page_size=2&page_token=" + byType[2]); fmt.Sprint(rest) != "[supplier_invoice/2026]" {
		t.Fatalf("by type, rest: %v", rest)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/number-ranges?order_by=prefix", "", "")

	// Ranges of legal entities outside the caller's access are invisible.
	agent := apitest.Client{T: t, API: e.API, Tenant: e.ten, As: e.ten.Agent}
	list, _ := agent.Must(200, "GET", "/v1/number-ranges", "", "")
	if len(list["items"].([]any)) != 0 {
		t.Fatalf("agent sees %v", list)
	}
	all, _ := e.Must(200, "GET", "/v1/number-ranges", "", "")
	agent.Problem(404, "request.not_found", "GET", "/v1/number-ranges/"+all["items"].([]any)[0].(map[string]any)["id"].(string), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/number-ranges/"+uuid.NewString(), "", "")
}
