package configuration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
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
	c.NumberRanges = []config.NumberRange{{Company: "1000", DocumentType: "supplier_invoice", FiscalYear: 2026, Prefix: "SI-", First: 1, Last: 99999, Gapless: true},
		{Company: "1000", DocumentType: "journal_kg", FiscalYear: 2026, First: 1, Last: 99999}}
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
// bare, bootstraps its configuration with document(t). Its principals hold
// perms and extra.
func setup(t *testing.T, bare bool, extra ...string) env {
	t.Helper()
	ten := pgtest.NewTenant(t, append(slices.Clone(perms), extra...)...)
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
		if err := e.bootstrap(t, jsonOf(t, document(t))); err != nil {
			t.Fatalf("bootstrap the configuration: %v", err)
		}
	}
	return e
}

func (e env) bootstrap(t *testing.T, doc string) error {
	t.Helper()
	return apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, deps(), json.RawMessage(doc), "configuration")
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
	pgtest.WantErr(t, e.bootstrap(t, `{"version": 1}`), "config.validation_failed")
	pgtest.WantErr(t, e.bootstrap(t, `{"nonsense": true}`), "cannot be read")
	foreign := document(t)
	foreign.NumberRanges[0].Company = "9999"
	pgtest.WantErr(t, e.bootstrap(t, jsonOf(t, foreign)), "config.validation_failed")
	// Change requests need a configuration to change.
	e.Problem(409, "config.not_bootstrapped", "POST", "/v1/change-requests", `{"title": "T", "base_version": 1, "proposed": {}}`, "")

	if err := e.bootstrap(t, jsonOf(t, document(t))); err != nil {
		t.Fatal(err)
	}
	pgtest.WantErr(t, e.bootstrap(t, jsonOf(t, document(t))), "config.already_bootstrapped")
	list, _ := e.Must(200, "GET", "/v1/config-versions", "", "")
	items := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("versions: got %d %v, want 1", len(items), items)
	}
	v := items[0].(map[string]any)
	for field, want := range map[string]any{"number": float64(1), "source": "bootstrap", "is_active": true, "change_request_id": nil} {
		if v[field] != want {
			t.Errorf("version %s: got %v, want %v", field, v[field], want)
		}
	}
	if got := v["document"].(map[string]any)["version"]; got != float64(1) {
		t.Errorf("version document version: got %v, want 1", got)
	}
	got, tag := e.Must(200, "GET", "/v1/config-versions/"+v["id"].(string), "", "")
	if got["number"] != float64(1) {
		t.Errorf("get version number: got %v, want 1", got["number"])
	}
	if len(tag) < 10 {
		t.Errorf("get version tag: got %q, want at least 10 characters", tag)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/config-versions/"+uuid.NewString(), "", "")
	ranges, _ := e.Must(200, "GET", "/v1/number-ranges?document_type=supplier_invoice", "", "")
	r := ranges["items"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{"legal_entity_id": e.le.String(), "prefix": "SI-", "next_value": float64(1), "is_gapless": true} {
		if r[field] != want {
			t.Errorf("range %s: got %v, want %v", field, r[field], want)
		}
	}
	activated := events(t, e.ten, "configuration_activated")
	if len(activated) != 1 {
		t.Fatalf("activated events: got %d %v, want 1", len(activated), activated)
	}
	if activated[0]["source"] != "bootstrap" {
		t.Errorf("activated event source: got %v, want bootstrap", activated[0]["source"])
	}
	if activated[0]["previous_version"] != nil {
		t.Errorf("activated event previous_version: got %v, want none", activated[0]["previous_version"])
	}
	if hash := activated[0]["hash"].(string); len(hash) != 64 {
		t.Errorf("activated event hash: got %q, want 64 characters", hash)
	}
}

func TestNumberRangeChange(t *testing.T) {
	e := setup(t, false)
	doc := document(t)
	doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "payment", FiscalYear: 2026, First: 100, Last: 999})
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/change-requests", Tenant: e.ten.ID, As: e.ten.Human, Key: apitest.NewKey(),
		Body: fmt.Sprintf(`{"title": "Payment numbers 2026", "base_version": 1, "proposed": %s}`, jsonOf(t, doc))})
	cr := apitest.JSON(t, r)
	if r.Code != 201 {
		t.Fatalf("create: got %d %v, want 201", r.Code, cr)
	}
	if got, want := r.Header().Get("Location"), "/v1/change-requests/"+cr["id"].(string); got != want {
		t.Errorf("create Location: got %s, want %s", got, want)
	}
	for field, want := range map[string]any{"number": "CR-000001", "status": "draft", "author_id": e.ten.Human.ID.String(), "rationale": ""} {
		if cr[field] != want {
			t.Errorf("create %s: got %v, want %v", field, cr[field], want)
		}
	}
	if items := cr["items"].([]any); len(items) != 0 {
		t.Errorf("create items: got %v, want none", items)
	}
	path := "/v1/change-requests/" + cr["id"].(string)
	got, _ := e.Must(200, "PATCH", path, `{"title": "Payment number range 2026", "rationale": "New document type"}`, `"1"`)
	if got["title"] != "Payment number range 2026" {
		t.Errorf("patched title: got %v, want Payment number range 2026", got["title"])
	}
	if got["version"] != float64(2) {
		t.Errorf("patched version: got %v, want 2", got["version"])
	}
	got, _ = e.Must(200, "POST", path+":submit", "", `"2"`)
	if got["status"] != "approved" {
		t.Errorf("submit status: got %v, want approved", got["status"])
	}
	if got["tier"] != float64(1) {
		t.Errorf("submit tier: got %v, want 1", got["tier"])
	}
	items := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("submit items: got %d %v, want 1", len(items), items)
	}
	item := items[0].(map[string]any)
	for field, want := range map[string]any{"path": "number_ranges[1000/payment/2026]", "new_value": "(added)", "old_value": nil} {
		if item[field] != want {
			t.Errorf("submit item %s: got %v, want %v", field, item[field], want)
		}
	}
	e.Problem(409, "config.invalid_status", "POST", path+":submit", "", `"3"`)
	got, _ = e.Must(200, "POST", path+":activate", "", `"3"`)
	if got["status"] != "active" {
		t.Errorf("activate status: got %v, want active", got["status"])
	}
	if got["activated_version"] != float64(2) {
		t.Errorf("activated version: got %v, want 2", got["activated_version"])
	}
	e.Problem(409, "config.invalid_status", "POST", path+":activate", "", `"4"`)

	versions, _ := e.Must(200, "GET", "/v1/config-versions", "", "")
	vs := versions["items"].([]any)
	if len(vs) != 2 {
		t.Fatalf("versions: got %d %v, want 2", len(vs), vs)
	}
	newest, older := vs[0].(map[string]any), vs[1].(map[string]any)
	for field, want := range map[string]any{"number": float64(2), "is_active": true, "change_request_id": cr["id"]} {
		if newest[field] != want {
			t.Errorf("version 2 %s: got %v, want %v", field, newest[field], want)
		}
	}
	if older["is_active"] != false {
		t.Errorf("version 1 is_active: got %v, want false", older["is_active"])
	}
	payment, _ := e.Must(200, "GET", "/v1/number-ranges?document_type=payment", "", "")
	p := payment["items"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{"first_value": float64(100), "next_value": float64(100), "prefix": "", "is_gapless": false} {
		if p[field] != want {
			t.Errorf("payment range %s: got %v, want %v", field, p[field], want)
		}
	}

	// Raising the limit of a range keeps its issued numbers.
	pgtest.Exec(t, `SELECT config.next_document_number($1, $2, 'supplier_invoice', 2026::smallint)`, e.ten.ID, e.le)
	raised := document(t)
	raised.NumberRanges = append(raised.NumberRanges, doc.NumberRanges[len(doc.NumberRanges)-1]) // the payment range
	raised.NumberRanges[0].Last = 199999
	cr2 := e.propose(t, 2, raised)
	if code, m := e.transition(t, cr2, "submit"); code != 200 {
		t.Fatalf("submit raise: got %d %v, want 200", code, m)
	} else if m["tier"] != float64(1) {
		t.Errorf("submit raise tier: got %v, want 1", m["tier"])
	}
	if code, m := e.transition(t, cr2, "activate"); code != 200 {
		t.Fatalf("activate raise: %d %v", code, m)
	}
	si, _ := e.Must(200, "GET", "/v1/number-ranges?document_type=supplier_invoice&fiscal_year=2026&legal_entity_id="+e.le.String(), "", "")
	s := si["items"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{"last_value": float64(199999), "next_value": float64(2), "version": float64(3)} {
		if s[field] != want {
			t.Errorf("raised range %s: got %v, want %v", field, s[field], want)
		}
	}
	_, tag := e.Must(200, "GET", "/v1/number-ranges/"+s["id"].(string), "", "")
	if tag != `"3"` {
		t.Fatal(tag)
	}

	submitted := events(t, e.ten, "change_request_submitted")
	validated := events(t, e.ten, "change_request_validated")
	approved := events(t, e.ten, "change_request_approved")
	activated := events(t, e.ten, "configuration_activated")
	for name, c := range map[string]struct {
		got  []map[string]any
		want int
	}{"submitted": {submitted, 2}, "validated": {validated, 2}, "approved": {approved, 2}, "activated": {activated, 3}} {
		if len(c.got) != c.want {
			t.Fatalf("%s events: got %d %v, want %d", name, len(c.got), c.got, c.want)
		}
	}
	if submitted[0]["item_count"] != float64(1) {
		t.Errorf("submitted event item_count: got %v, want 1", submitted[0]["item_count"])
	}
	if submitted[0]["title"] != "Payment number range 2026" {
		t.Errorf("submitted event title: got %v, want Payment number range 2026", submitted[0]["title"])
	}
	if validated[0]["outcome"] != "approved" {
		t.Errorf("validated event outcome: got %v, want approved", validated[0]["outcome"])
	}
	if activated[1]["previous_version"] != float64(1) {
		t.Errorf("activated event previous_version: got %v, want 1", activated[1]["previous_version"])
	}
	if activated[1]["change_request_id"] != cr["id"] {
		t.Errorf("activated event change_request_id: got %v, want %v", activated[1]["change_request_id"], cr["id"])
	}
}

func TestSubmitRefusals(t *testing.T) {
	e := setup(t, false)
	refuse := func(name string, doc string, code, field string) {
		t.Helper()
		cr, _ := e.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "T", "base_version": 1, "proposed": %s}`, doc), "")
		code2, m := e.transition(t, cr, "submit")
		if code2 != 422 {
			t.Errorf("%s: got status %d %v, want 422", name, code2, m)
		}
		if m["code"] != code {
			t.Errorf("%s: got code %v, want %s", name, m["code"], code)
		}
		if field != "" && !strings.Contains(fmt.Sprint(m["errors"]), field) {
			t.Errorf("%s: got errors %v, want one about %s", name, m["errors"], field)
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
	// A country pack applied for the next fiscal year creates its ranges
	// afresh, from the same prefix and first number (review:T73g2).
	nextYear := func(prefix string) *config.Config {
		doc := document(t)
		for _, n := range doc.NumberRanges {
			n.FiscalYear = 2027
			if n.DocumentType == "supplier_invoice" {
				n.Prefix = prefix
			}
			doc.NumberRanges = append(doc.NumberRanges, n)
		}
		return doc
	}
	refuse("a range repeating last year's numbers", jsonOf(t, nextYear("SI-")), "config.validation_failed",
		"proposed.number_ranges[1000/supplier_invoice/2027].prefix")
	foreign := document(t)
	foreign.NumberRanges = append(foreign.NumberRanges, config.NumberRange{Company: "3000", DocumentType: "payment", FiscalYear: 2026, First: 1, Last: 9})
	refuse("unknown company", jsonOf(t, foreign), "config.validation_failed", "no legal entity has company code 3000")
	// F14: the configured amounts are read in the company's currency and
	// compared after conversion into the legal entity's; the two must agree.
	dollars := document(t)
	dollars.Companies[1].Currency = "USD"
	refuse("currency differs from the legal entity's", jsonOf(t, dollars), "config.validation_failed", "proposed.companies[2000].currency")

	t.Run("a range with the year in its prefix", func(t *testing.T) {
		if code, m := e.transition(t, e.propose(t, 1, nextYear("SI-2027-")), "submit"); code != 200 {
			t.Errorf("submit: got %d %v, want 200", code, m)
		}
	})

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
	code, m := e.transition(t, pending, "submit")
	if code != 200 {
		t.Fatalf("tier 2: got %d %v, want 200", code, m)
	}
	for field, want := range map[string]any{"status": "pending_approval", "tier": float64(2)} {
		if m[field] != want {
			t.Errorf("tier 2 %s: got %v, want %v", field, m[field], want)
		}
	}
	if m["workflow_request_id"] == nil {
		t.Error("tier 2 workflow_request_id: got nil, want a request")
	}
	if code, m := e.transition(t, pending, "activate"); code != 409 {
		t.Fatalf("activate pending: got %d %v, want 409", code, m)
	} else if m["code"] != "config.invalid_status" {
		t.Errorf("activate pending: got %v, want config.invalid_status", m["code"])
	}
	if code, m := e.transition(t, pending, "simulate"); code != 409 {
		t.Fatalf("simulate pending: %d %v", code, m)
	}

	tax := document(t)
	tax.Companies[0].TaxCodes[0].RatePercent = "9" // tier 3: posting-relevant
	evidence := e.propose(t, 1, tax)
	code, m = e.transition(t, evidence, "submit")
	if code != 200 {
		t.Fatalf("tier 3: got %d %v, want 200", code, m)
	}
	for field, want := range map[string]any{"status": "awaiting_evidence", "tier": float64(3)} {
		if m[field] != want {
			t.Errorf("tier 3 %s: got %v, want %v", field, m[field], want)
		}
	}
	cur, tag := e.Must(200, "GET", "/v1/change-requests/"+evidence["id"].(string), "", "")
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/change-requests/" + cur["id"].(string) + ":simulate", Tenant: e.ten.ID, As: e.ten.Human,
		Key: apitest.NewKey(), IfMatch: tag})
	op := apitest.JSON(t, r)
	if r.Code != 202 {
		t.Fatalf("simulate: got %d %v, want 202", r.Code, op)
	}
	for field, want := range map[string]any{"kind": "config.simulate", "status": "queued"} {
		if op[field] != want {
			t.Errorf("simulate %s: got %v, want %v", field, op[field], want)
		}
	}
	if got, want := r.Header().Get("Location"), "/v1/operations/"+op["id"].(string); got != want {
		t.Errorf("simulate Location: got %s, want %s", got, want)
	}
	e.Problem(412, "request.version_mismatch", "POST", "/v1/change-requests/"+cur["id"].(string)+":simulate", "", `"99"`)
	e.Problem(404, "request.not_found", "POST", "/v1/change-requests/"+uuid.NewString()+":simulate", "", `"1"`)

	// Only the author changes or withdraws a request.
	other := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	other.Subject = "human|" + other.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Other', 'active', $2, $2)`, e.ten.ID, other.ID, pgtest.Issuer(t), other.Subject)
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
	if got["status"] != "draft" {
		t.Errorf("changed invalid request: got status %v, want draft", got["status"])
	}
	if got["proposed"].(map[string]any)["companies"] == nil {
		t.Errorf("changed invalid request: got no proposed companies in %v, want them", got["proposed"])
	}

	withdrawn := events(t, e.ten, "change_request_withdrawn")
	validated := events(t, e.ten, "change_request_validated")
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawn events: got %d %v, want 1", len(withdrawn), withdrawn)
	}
	if withdrawn[0]["previous_status"] != "pending_approval" {
		t.Errorf("withdrawn event previous_status: got %v, want pending_approval", withdrawn[0]["previous_status"])
	}
	if len(validated) != 2 {
		t.Fatalf("validated events: got %d %v, want 2", len(validated), validated)
	}
	for i, want := range []string{"pending_approval", "awaiting_evidence"} {
		if validated[i]["outcome"] != want {
			t.Errorf("validated event %d outcome: got %v, want %s", i, validated[i]["outcome"], want)
		}
	}
	if approved := events(t, e.ten, "change_request_approved"); len(approved) != 0 {
		t.Errorf("approved events: got %v, want none", approved)
	}

	// Lists of change requests: newest first, by status.
	list := func(q string) []any {
		m, _ := e.Must(200, "GET", "/v1/change-requests"+q, "", "")
		return m["items"].([]any)
	}
	if all := list(""); len(all) != 3 {
		t.Fatalf("all: got %d %v, want 3", len(all), all)
	} else if first := all[0].(map[string]any)["id"]; first != draft["id"] {
		t.Errorf("all: got %v first, want the newest %v", first, draft["id"])
	}
	if w := list("?status=withdrawn"); len(w) != 1 {
		t.Fatalf("withdrawn: %v", w)
	}
	page, _ := e.Must(200, "GET", "/v1/change-requests?order_by=created_at%20asc&page_size=2", "", "")
	rest, _ := e.Must(200, "GET", "/v1/change-requests?order_by=created_at%20asc&page_size=2&page_token="+page["next_page_token"].(string), "", "")
	if first := page["items"].([]any)[0].(map[string]any)["id"]; first != pending["id"] {
		t.Errorf("page 1: got %v first, want the oldest %v", first, pending["id"])
	}
	if n := len(rest["items"].([]any)); n != 1 {
		t.Errorf("page 2: got %d items, want 1", n)
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
	if code, m := e.transition(t, b, "activate"); code != 409 {
		t.Fatalf("second: got %d %v, want 409", code, m)
	} else if m["code"] != "config.base_version_stale" {
		t.Errorf("second: got %v, want config.base_version_stale", m["code"])
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
	if len(page) != 3 {
		t.Fatalf("page: got %d %v, want two numbers and a token", len(page), page)
	}
	if got := fmt.Sprint(page[:2]); got != "[1 2]" {
		t.Errorf("page: got %s, want [1 2]", got)
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
	if rest := years("?order_by=fiscal_year%20desc&page_token=" + byYear[1]); len(rest) != 3 { // payment, journal_kg and supplier_invoice of 2026
		t.Fatalf("by year, rest: %v", rest)
	}
	byType := years("?page_size=2")
	if byType[0] != "journal_kg/2026" || !strings.HasPrefix(byType[1], "payment/") {
		t.Fatalf("by type: got %v, want journal_kg/2026 and a payment range", byType)
	}
	if rest := years("?page_size=2&page_token=" + byType[2]); len(rest) != 2 || !strings.HasPrefix(rest[0], "payment/") || rest[1] != "supplier_invoice/2026" {
		t.Fatalf("by type, rest: got %v, want the other payment range and supplier_invoice/2026", rest)
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

// Custom field schemas (ADR-0046): adding an optional field is approved on
// submission (tier 1), requiring one opens an approval (tier 2), and a
// changed schema must carry the next version.
func TestCustomFieldSchemas(t *testing.T) {
	e := setup(t, false)
	field := func(c *config.Config, version int, required ...string) *config.Config {
		no := false
		c.Extensions = map[string]extensions.Schema{"supplier_invoice": {Version: version, Type: "object", AdditionalProperties: &no,
			Required: required, Properties: map[string]extensions.Property{"project": {Type: "string", MaxLength: 20, PII: extensions.PIINone}}}}
		return c
	}
	for name, c := range map[string]struct {
		doc          *config.Config
		status, tier any
	}{
		"optional field": {field(document(t), 1), "approved", float64(1)},
		"required field": {field(document(t), 1, "project"), "pending_approval", float64(2)},
	} {
		t.Run(name, func(t *testing.T) {
			code, m := e.transition(t, e.propose(t, 1, c.doc), "submit")
			if code != 200 {
				t.Fatalf("submit: got %d %v, want 200", code, m)
			}
			if m["status"] != c.status {
				t.Errorf("status: got %v, want %v", m["status"], c.status)
			}
			if m["tier"] != c.tier {
				t.Errorf("tier: got %v, want %v", m["tier"], c.tier)
			}
		})
	}
	code, m := e.transition(t, e.propose(t, 1, field(document(t), 2)), "submit")
	if code != 422 {
		t.Fatalf("first version numbered 2: got %d %v, want 422", code, m)
	}
	if !strings.Contains(fmt.Sprint(m["errors"]), "proposed.extensions.supplier_invoice.version") {
		t.Errorf("first version numbered 2: got errors %v, want one at proposed.extensions.supplier_invoice.version", m["errors"])
	}
}
