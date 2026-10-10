package tenancy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"tenancy.org.read", "tenancy.org.manage"}

// org is a tenant whose human acts in every legal entity.
func org(t *testing.T) pgtest.Tenant {
	t.Helper()
	ten := pgtest.NewTenant(t, perms...)
	ten.AllCompanies(t, ten.Human)
	return ten
}

type client struct{ apitest.Client }

func newClient(t *testing.T, ten pgtest.Tenant, as pgtest.Actor) client {
	return client{apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: as}}
}

func (c client) legalEntity(code string) map[string]any {
	c.T.Helper()
	m, _ := c.Must(201, "POST", "/v1/legal-entities", fmt.Sprintf(`{"code": %q, "name": "Entity %s", "country_code": "DE",
		"local_currency": "EUR", "fiscal_calendar_id": %q}`, code, code, uuid.NewString()), "")
	return m
}

func TestLegalEntityLifecycle(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	r := c.API.Do(apitest.Req{Method: "POST", Path: "/v1/legal-entities", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey(),
		Body: `{"code": "DE01", "name": "Muster GmbH", "country_code": "DE", "local_currency": "EUR",
		"fiscal_calendar_id": "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5da0", "extensions": {"cost_hint": "MKT", "region": {"a": 1}}}`})
	le := apitest.JSON(t, r)
	id := le["id"].(string)
	if r.Code != 201 || r.Header().Get("Location") != "/v1/legal-entities/"+id || r.Header().Get("ETag") != `"1"` ||
		le["status"] != "inactive" || le["created_by"] != ten.Human.ID.String() || le["extensions"].(map[string]any)["cost_hint"] != "MKT" {
		t.Fatalf("create: %d %v", r.Code, le)
	}
	path := "/v1/legal-entities/" + id
	got, tag := c.Must(200, "GET", path, "", "")
	if got["code"] != "DE01" || tag != `"1"` {
		t.Fatalf("get: %v %s", got, tag)
	}

	// Merge patch: name replaced, a custom field removed, a nested one merged.
	got, tag = c.Must(200, "PATCH", path, `{"name": "Muster Industrie GmbH", "extensions": {"cost_hint": null, "region": {"b": 2}}}`, `"1"`)
	ext := got["extensions"].(map[string]any)
	if got["name"] != "Muster Industrie GmbH" || tag != `"2"` || ext["cost_hint"] != nil || ext["region"].(map[string]any)["b"] != float64(2) ||
		ext["region"].(map[string]any)["a"] != float64(1) {
		t.Fatalf("update: %v", got)
	}
	c.Must(200, "PATCH", path, `{}`, `"2"`) // an empty patch changes nothing but the version

	if code, m, _ := c.Do("POST", path+":block", `{"reason": "x"}`, `"3"`); code != 409 || m["code"] != "tenancy.invalid_status" {
		t.Fatalf("block inactive: %d %v", code, m)
	}
	if code, m, _ := c.Do("POST", path+":activate", "", `"1"`); code != 412 || m["code"] != "request.version_mismatch" {
		t.Fatalf("stale: %d %v", code, m)
	}
	if code, m, _ := c.Do("POST", path+":activate", "", ""); code != 428 {
		t.Fatalf("no If-Match: %d %v", code, m)
	}
	got, _ = c.Must(200, "POST", path+":activate", "", `"3"`)
	if got["status"] != "active" {
		t.Fatal(got)
	}
	got, _ = c.Must(200, "POST", path+":block", `{"reason": "Insolvency filed"}`, `"4"`)
	if got["status"] != "blocked" {
		t.Fatal(got)
	}
	got, _ = c.Must(200, "POST", path+":unblock", "", `"5"`)
	if got["status"] != "active" || got["version"] != float64(6) {
		t.Fatal(got)
	}

	created := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "legal_entity_created"})
	blocked := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "legal_entity_blocked"})
	unblocked := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "legal_entity_unblocked"})
	if len(created) != 1 || created[0]["code"] != "DE01" || created[0]["status"] != "inactive" ||
		len(blocked) != 1 || blocked[0]["previous_status"] != "active" || len(unblocked) != 1 || unblocked[0]["status"] != "active" {
		t.Fatalf("events: %v %v %v", created, blocked, unblocked)
	}
	var actions []string
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT action || coalesce(':' || (detail->>'reason'), '') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_id = $2 ORDER BY seq`, ten.ID, uuid.MustParse(id))
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	want := "tenancy.legal_entity.create tenancy.legal_entity.update tenancy.legal_entity.update tenancy.legal_entity.activate " +
		"tenancy.legal_entity.block:Insolvency filed tenancy.legal_entity.unblock"
	if strings.Join(actions, " ") != want {
		t.Fatalf("audit: %v", actions)
	}
}

func TestLegalEntityValidation(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	c.legalEntity("DE01")
	body := func(code, country, currency, ext string) string {
		return fmt.Sprintf(`{"code": %q, "name": "N", "country_code": %q, "local_currency": %q, "fiscal_calendar_id": %q, "extensions": %s}`,
			code, country, currency, uuid.NewString(), ext)
	}
	for name, tc := range map[string]struct {
		body string
		code int
		pc   string
	}{
		"duplicate code":   {body("DE01", "DE", "EUR", "{}"), 409, "tenancy.legal_entity_code_taken"},
		"unknown country":  {body("XX01", "XX", "EUR", "{}"), 422, "tenancy.unknown_country"},
		"unknown currency": {body("XX02", "DE", "XXQ", "{}"), 422, "tenancy.unknown_currency"},
		"bad extension":    {body("XX03", "DE", "EUR", `{"CostHint": 1}`), 422, "request.invalid_extension"},
		"bad code":         {body("de01", "DE", "EUR", "{}"), 400, "request.invalid_body"},
	} {
		code, m, _ := c.Do("POST", "/v1/legal-entities", tc.body, "")
		if code != tc.code || m["code"] != tc.pc {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
	le := c.legalEntity("DE02")
	if code, m, _ := c.Do("PATCH", "/v1/legal-entities/"+le["id"].(string), `{"extensions": {"Bad": 1}}`, `"1"`); code != 422 || m["code"] != "request.invalid_extension" {
		t.Errorf("bad extension on update: %d %v", code, m)
	}
	if code, _, _ := c.Do("PATCH", "/v1/legal-entities/"+uuid.NewString(), `{}`, `"1"`); code != 404 {
		t.Errorf("update unknown: %d", code)
	}
	if code, _, _ := c.Do("PATCH", "/v1/legal-entities/"+le["id"].(string), `{}`, `"9"`); code != 412 {
		t.Errorf("update stale: %d", code)
	}
}

func TestCompanyAccess(t *testing.T) {
	ten := org(t)
	api := apitest.New(t)
	admin := client{apitest.Client{T: t, API: api, Tenant: ten, As: ten.Human}}
	de := admin.legalEntity("DE01")
	fr := admin.legalEntity("FR01")
	unit, _ := admin.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P1", "name": "Plant"}`, fr["id"]), "")

	// The agent may act in DE01 only.
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date, $4)`, ten.ID, ten.Agent.ID, uuid.MustParse(de["id"].(string)), ten.Human.ID)
	// An ended grant to FR01 gives no access.
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 10, current_date, $4)`, ten.ID, ten.Agent.ID, uuid.MustParse(fr["id"].(string)), ten.Human.ID)
	agent := client{apitest.Client{T: t, API: api, Tenant: ten, As: ten.Agent}}
	list, _ := agent.Must(200, "GET", "/v1/legal-entities", "", "")
	if items := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["code"] != "DE01" {
		t.Fatalf("list: %v", list)
	}
	for _, path := range []string{"/v1/legal-entities/" + fr["id"].(string), "/v1/org-units/" + unit["id"].(string)} {
		if code, m, _ := agent.Do("GET", path, "", ""); code != 404 || m["code"] != "request.not_found" {
			t.Errorf("%s: %d %v", path, code, m)
		}
	}
	if code, m, _ := agent.Do("POST", "/v1/legal-entities/"+fr["id"].(string)+":activate", "", `"1"`); code != 404 {
		t.Errorf("activate outside access: %d %v", code, m)
	}
	units, _ := agent.Must(200, "GET", "/v1/org-units", "", "")
	if len(units["items"].([]any)) != 0 {
		t.Errorf("org units outside access listed: %v", units)
	}
	code, m, _ := agent.Do("POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P2", "name": "Plant"}`, fr["id"]), "")
	if code != 422 || m["code"] != "request.reference_not_found" || m["errors"].([]any)[0].(map[string]any)["field"] != "legal_entity_id" {
		t.Errorf("org unit outside access: %d %v", code, m)
	}
	// Without the permission the operation is refused before any lookup.
	none := pgtest.NewTenant(t)
	if code, m, _ := (client{apitest.Client{T: t, API: api, Tenant: none, As: none.Human}}).Do("GET", "/v1/legal-entities", "", ""); code != 403 || m["code"] != "identity.permission_missing" {
		t.Errorf("no permission: %d %v", code, m)
	}
}

func TestListLegalEntities(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	for _, code := range []string{"C3", "A1", "B2"} {
		c.legalEntity(code)
	}
	codes := func(page map[string]any) (out []string) {
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["code"].(string))
		}
		return out
	}
	first, _ := c.Must(200, "GET", "/v1/legal-entities?page_size=2", "", "")
	if got := codes(first); strings.Join(got, ",") != "A1,B2" {
		t.Fatalf("first page: %v", got)
	}
	second, _ := c.Must(200, "GET", "/v1/legal-entities?page_size=2&page_token="+first["next_page_token"].(string), "", "")
	if got := codes(second); strings.Join(got, ",") != "C3" || second["next_page_token"] != nil {
		t.Fatalf("second page: %v", second)
	}
	byName, _ := c.Must(200, "GET", "/v1/legal-entities?order_by=name%20desc&page_size=1", "", "")
	next, _ := c.Must(200, "GET", "/v1/legal-entities?order_by=name%20desc&page_size=1&page_token="+byName["next_page_token"].(string), "", "")
	if codes(byName)[0] != "C3" || codes(next)[0] != "B2" {
		t.Fatalf("by name: %v %v", byName, next)
	}
	if got, _ := c.Must(200, "GET", "/v1/legal-entities?status=active", "", ""); len(got["items"].([]any)) != 0 {
		t.Fatalf("status filter: %v", got)
	}
	for query, pc := range map[string]string{
		"?order_by=country_code":    "request.invalid_order_by",
		"?order_by=code%20sideways": "request.invalid_parameter",
		"?status=active&page_token=" + first["next_page_token"].(string): "request.invalid_page_token",
	} {
		if code, m, _ := c.Do("GET", "/v1/legal-entities"+query, "", ""); code != 400 || m["code"] != pc {
			t.Errorf("%s: %d %v", query, code, m)
		}
	}
}

func TestOrgUnits(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	de, fr := c.legalEntity("DE01"), c.legalEntity("FR01")
	unit := func(le map[string]any, kind, code, parent string) (int, map[string]any) {
		body := fmt.Sprintf(`{"legal_entity_id": %q, "kind": %q, "code": %q, "name": "Unit %s"`, le["id"], kind, code, code)
		if parent != "" {
			body += fmt.Sprintf(`, "parent_id": %q`, parent)
		}
		code2, m, _ := c.Do("POST", "/v1/org-units", body+"}", "")
		return code2, m
	}
	code, plant := unit(de, "plant", "P100", "")
	if code != 201 || plant["status"] != "inactive" || plant["parent_id"] != nil {
		t.Fatalf("plant: %d %v", code, plant)
	}
	code, store := unit(de, "storage_location", "S1", plant["id"].(string))
	if code != 201 || store["parent_id"] != plant["id"] {
		t.Fatalf("storage location: %d %v", code, store)
	}
	for name, tc := range map[string]struct {
		le                 map[string]any
		kind, code, parent string
		status             int
		pc                 string
	}{
		"duplicate":       {de, "plant", "P100", "", 409, "tenancy.org_unit_code_taken"},
		"unknown kind":    {de, "warehouse", "W1", "", 422, "tenancy.unknown_org_unit_kind"},
		"foreign parent":  {fr, "plant", "P200", plant["id"].(string), 422, "tenancy.invalid_parent"},
		"unknown parent":  {de, "plant", "P300", uuid.NewString(), 422, "tenancy.invalid_parent"},
		"unknown company": {map[string]any{"id": uuid.NewString()}, "plant", "P4", "", 422, "request.reference_not_found"},
	} {
		if code, m := unit(tc.le, tc.kind, tc.code, tc.parent); code != tc.status || m["code"] != tc.pc {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}

	// A unit cannot become its own ancestor.
	if code, m, _ := c.Do("PATCH", "/v1/org-units/"+plant["id"].(string), fmt.Sprintf(`{"parent_id": %q}`, store["id"]), `"1"`); code != 422 ||
		m["errors"].([]any)[0].(map[string]any)["code"] != "cycle" {
		t.Fatalf("cycle: %d %v", code, m)
	}
	got, _ := c.Must(200, "PATCH", "/v1/org-units/"+store["id"].(string), `{"name": "Main store", "parent_id": null}`, `"1"`)
	if got["name"] != "Main store" || got["parent_id"] != nil {
		t.Fatalf("update: %v", got)
	}
	got, _ = c.Must(200, "PATCH", "/v1/org-units/"+store["id"].(string), fmt.Sprintf(`{"parent_id": %q}`, plant["id"]), `"2"`)
	if got["parent_id"] != plant["id"] {
		t.Fatalf("set parent: %v", got)
	}
	if code, _, _ := c.Do("PATCH", "/v1/org-units/"+uuid.NewString(), `{}`, `"1"`); code != 404 {
		t.Fatalf("update unknown: %d", code)
	}
	if code, _, _ := c.Do("PATCH", "/v1/org-units/"+store["id"].(string), `{}`, `"1"`); code != 412 {
		t.Fatalf("update stale: %d", code)
	}

	path := "/v1/org-units/" + plant["id"].(string)
	got, tag := c.Must(200, "GET", path, "", "")
	if got["code"] != "P100" || tag != `"1"` {
		t.Fatal(got, tag)
	}
	if code, _, _ := c.Do("POST", path+":activate", "", `"7"`); code != 412 {
		t.Fatalf("activate stale: %d", code)
	}
	c.Must(200, "POST", path+":activate", "", `"1"`)
	if code, m, _ := c.Do("POST", path+":unblock", "", `"2"`); code != 409 || m["code"] != "tenancy.invalid_status" {
		t.Fatalf("unblock active: %d %v", code, m)
	}
	c.Must(200, "POST", path+":block", `{"reason": "Closed for refit"}`, `"2"`)
	got, _ = c.Must(200, "POST", path+":unblock", "", `"3"`)
	if got["status"] != "active" {
		t.Fatal(got)
	}
	created := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "org_unit_created"})
	blocked := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "org_unit_blocked"})
	unblocked := apitest.Published(t, ten, outbox.Event{Context: "tenancy", Name: "org_unit_unblocked"})
	if len(created) != 2 || created[1]["parent_id"] != plant["id"] || len(blocked) != 1 || len(unblocked) != 1 || unblocked[0]["kind"] != "plant" {
		t.Fatalf("events: %v %v %v", created, blocked, unblocked)
	}

	list := func(q string) []string {
		m, _ := c.Must(200, "GET", "/v1/org-units"+q, "", "")
		var out []string
		for _, it := range m["items"].([]any) {
			out = append(out, it.(map[string]any)["code"].(string))
		}
		if tok, ok := m["next_page_token"].(string); ok {
			out = append(out, "next:"+tok)
		}
		return out
	}
	unit(fr, "sales_org", "SO1", "")
	if got := list("?legal_entity_id=" + de["id"].(string)); strings.Join(got, ",") != "P100,S1" {
		t.Fatalf("by legal entity: %v", got)
	}
	if got := list("?kind=sales_org"); strings.Join(got, ",") != "SO1" {
		t.Fatalf("by kind: %v", got)
	}
	if got := list("?status=active"); strings.Join(got, ",") != "P100" {
		t.Fatalf("by status: %v", got)
	}
	page := list("?order_by=name&page_size=2")
	if len(page) != 3 || page[0] != "S1" {
		t.Fatalf("by name: %v", page)
	}
	if rest := list("?order_by=name&page_size=2&page_token=" + strings.TrimPrefix(page[2], "next:")); strings.Join(rest, ",") != "SO1" {
		t.Fatalf("by name, page 2: %v", rest)
	}
	byCode := list("?page_size=1")
	if len(byCode) != 2 || byCode[0] != "P100" {
		t.Fatalf("by code: %v", byCode)
	}
	if rest := list("?page_size=5&page_token=" + strings.TrimPrefix(byCode[1], "next:")); strings.Join(rest, ",") != "S1,SO1" {
		t.Fatalf("by code, page 2: %v", rest)
	}
	if code, m, _ := c.Do("GET", "/v1/org-units?order_by=kind", "", ""); code != 400 || m["code"] != "request.invalid_order_by" {
		t.Fatalf("bad order: %d %v", code, m)
	}
}

func TestDirectory(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	de := c.legalEntity("DE01")
	err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		id, status, ok, err := tenancy.Directory{}.LegalEntityByCode(ctx, "DE01")
		if err != nil || !ok || id.String() != de["id"] || status != "inactive" {
			t.Errorf("DE01: %v %s %v %v", id, status, ok, err)
		}
		if _, _, ok, err := (tenancy.Directory{}).LegalEntityByCode(ctx, "XX"); ok || err != nil {
			t.Errorf("XX: %v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		status, ok, err := tenancy.Directory{}.LegalEntityStatus(ctx, uuid.MustParse(de["id"].(string)))
		if err != nil || !ok || status != "inactive" {
			t.Errorf("status: %s %v %v", status, ok, err)
		}
		if _, ok, err := (tenancy.Directory{}).LegalEntityStatus(ctx, uuid.New()); ok || err != nil {
			t.Errorf("unknown: %v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := (tenancy.Directory{}).LegalEntityByCode(context.Background(), "DE01"); err == nil {
		t.Error("outside a request")
	}
	if _, _, err := (tenancy.Directory{}).LegalEntityStatus(context.Background(), uuid.New()); err == nil {
		t.Error("status outside a request")
	}
	apitest.EveryFailure(t, ten, ten.Human, "legal entity status", func(ctx context.Context) error {
		_, _, err := tenancy.Directory{}.LegalEntityStatus(ctx, uuid.New())
		return err
	})
	apitest.EveryFailure(t, ten, ten.Human, "directory", func(ctx context.Context) error {
		_, _, _, err := tenancy.Directory{}.LegalEntityByCode(ctx, "DE01")
		return err
	})

	err = apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		d := tenancy.Directory{}
		e, ok, err := d.Entity(ctx, uuid.MustParse(de["id"].(string)))
		if err != nil || !ok || e.Code != "DE01" || e.LocalCurrency != "EUR" || e.Status != "inactive" || e.FiscalCalendarID.String() != de["fiscal_calendar_id"] {
			t.Errorf("entity: %+v %v %v", e, ok, err)
		}
		if byCode, ok, err := d.EntityByCode(ctx, "DE01"); err != nil || !ok || byCode != e {
			t.Errorf("entity by code: %+v %v %v", byCode, ok, err)
		}
		if _, ok, err := d.EntityByCode(ctx, "XX"); ok || err != nil {
			t.Errorf("unknown code: %v %v", ok, err)
		}
		if c, err := d.GroupCurrency(ctx); err != nil || c != "EUR" {
			t.Errorf("group currency: %s %v", c, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := (tenancy.Directory{}).Entity(context.Background(), uuid.New()); err == nil {
		t.Error("entity outside a request")
	}
	if _, err := (tenancy.Directory{}).GroupCurrency(context.Background()); err == nil {
		t.Error("group currency outside a request")
	}
	apitest.EveryFailure(t, ten, ten.Human, "entity", func(ctx context.Context) error {
		_, _, err := tenancy.Directory{}.Entity(ctx, uuid.New())
		return err
	})
	apitest.EveryFailure(t, ten, ten.Human, "group currency", func(ctx context.Context) error {
		_, err := tenancy.Directory{}.GroupCurrency(ctx)
		return err
	})

	plant, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P1", "name": "Plant"}`, de["id"]), "")
	err = apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		u, ok, err := tenancy.Directory{}.OrgUnit(ctx, uuid.MustParse(plant["id"].(string)))
		if err != nil || !ok || u.Kind != "plant" || u.LegalEntityID.String() != de["id"] || u.Status != "inactive" {
			t.Errorf("org unit: %+v %v %v", u, ok, err)
		}
		if _, ok, err := (tenancy.Directory{}).OrgUnit(ctx, uuid.New()); ok || err != nil {
			t.Errorf("unknown org unit: %v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := (tenancy.Directory{}).OrgUnit(context.Background(), uuid.New()); err == nil {
		t.Error("org unit outside a request")
	}
	apitest.EveryFailure(t, ten, ten.Human, "org unit", func(ctx context.Context) error {
		_, _, err := tenancy.Directory{}.OrgUnit(ctx, uuid.New())
		return err
	})
}

// TestFailures calls every handler outside a unit of work and with each of
// its database statements failing in turn.
func TestFailures(t *testing.T) {
	ten := org(t)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	e := &tenancy.Endpoints{Pages: pages}
	c := newClient(t, ten, ten.Human)
	le := c.legalEntity("DE01")
	leID := uuid.MustParse(le["id"].(string))
	parent, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P1", "name": "P"}`, leID), "")
	parentID := uuid.MustParse(parent["id"].(string))
	ou, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P2", "name": "P"}`, leID), "")
	ouID := uuid.MustParse(ou["id"].(string))
	blocked := c.legalEntity("BL01")
	blockedID := uuid.MustParse(blocked["id"].(string))
	c.Must(200, "POST", "/v1/legal-entities/"+blocked["id"].(string)+":activate", "", `"1"`)
	c.Must(200, "POST", "/v1/legal-entities/"+blocked["id"].(string)+":block", `{"reason": "r"}`, `"2"`)
	name := gen.NewOptString("New")
	calls := map[string]func(ctx context.Context) error{
		"create legal entity": func(ctx context.Context) error {
			_, err := e.CreateLegalEntity(ctx, &gen.LegalEntityCreate{Code: "NEW1", Name: "N", CountryCode: "DE", LocalCurrency: "EUR", FiscalCalendarID: uuid.New()}, gen.CreateLegalEntityParams{})
			return err
		},
		"get legal entity": func(ctx context.Context) error {
			_, err := e.GetLegalEntity(ctx, gen.GetLegalEntityParams{ID: leID})
			return err
		},
		"list legal entities": func(ctx context.Context) error {
			_, err := e.ListLegalEntities(ctx, gen.ListLegalEntitiesParams{})
			return err
		},
		"update legal entity": func(ctx context.Context) error {
			_, err := e.UpdateLegalEntity(ctx, &gen.LegalEntityUpdate{Name: name, Extensions: gen.NewOptExtensions(gen.Extensions{"a": []byte("1")})},
				gen.UpdateLegalEntityParams{ID: leID, IfMatch: "*"})
			return err
		},
		"activate legal entity": func(ctx context.Context) error {
			_, err := e.ActivateLegalEntity(ctx, gen.ActivateLegalEntityParams{ID: leID, IfMatch: "*"})
			return err
		},
		"unblock legal entity": func(ctx context.Context) error {
			_, err := e.UnblockLegalEntity(ctx, gen.UnblockLegalEntityParams{ID: blockedID, IfMatch: "*"})
			return err
		},
		"create org unit": func(ctx context.Context) error {
			_, err := e.CreateOrgUnit(ctx, &gen.OrgUnitCreate{LegalEntityID: leID, Kind: "plant", Code: "P9", Name: "N", ParentID: gen.NewOptUUID(parentID)}, gen.CreateOrgUnitParams{})
			return err
		},
		"get org unit": func(ctx context.Context) error {
			_, err := e.GetOrgUnit(ctx, gen.GetOrgUnitParams{ID: ouID})
			return err
		},
		"list org units": func(ctx context.Context) error {
			_, err := e.ListOrgUnits(ctx, gen.ListOrgUnitsParams{})
			return err
		},
		"update org unit": func(ctx context.Context) error {
			_, err := e.UpdateOrgUnit(ctx, &gen.OrgUnitUpdate{Name: name, ParentID: gen.NewOptNilUUID(parentID)}, gen.UpdateOrgUnitParams{ID: ouID, IfMatch: "*"})
			return err
		},
		"activate org unit": func(ctx context.Context) error {
			_, err := e.ActivateOrgUnit(ctx, gen.ActivateOrgUnitParams{ID: ouID, IfMatch: "*"})
			return err
		},
	}
	for name, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, ten, ten.Human, name, call)
	}
	// Handlers that only differ from the above in their transition.
	for name, call := range map[string]func(ctx context.Context) error{
		"block legal entity": func(ctx context.Context) error {
			_, err := e.BlockLegalEntity(ctx, &gen.ReasonInput{Reason: "r"}, gen.BlockLegalEntityParams{ID: leID, IfMatch: "*"})
			return err
		},
		"block org unit": func(ctx context.Context) error {
			_, err := e.BlockOrgUnit(ctx, &gen.ReasonInput{Reason: "r"}, gen.BlockOrgUnitParams{ID: ouID, IfMatch: "*"})
			return err
		},
		"unblock org unit": func(ctx context.Context) error {
			_, err := e.UnblockOrgUnit(ctx, gen.UnblockOrgUnitParams{ID: ouID, IfMatch: "*"})
			return err
		},
	} {
		if err := call(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work", name)
		}
	}
	// Page sizes are checked by the handler too, for callers other than the generated router.
	for name, call := range map[string]func(ctx context.Context) error{
		"legal entities": func(ctx context.Context) error {
			_, err := e.ListLegalEntities(ctx, gen.ListLegalEntitiesParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"org units": func(ctx context.Context) error {
			_, err := e.ListOrgUnits(ctx, gen.ListOrgUnitsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		if err := apitest.InRequest(t, ten, ten.Human, call); err == nil {
			t.Errorf("%s: page_size 0 accepted", name)
		}
	}
}
