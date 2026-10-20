package configuration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// packEnv is a tenant bootstrapped with doc whose API serves the test
// packs: US 2026.1 (signed off) and 2026.2 (not yet).
func packEnv(t *testing.T, doc *config.Config) env {
	t.Helper()
	e := setup(t, true)
	e.API = apitest.NewWith(t, func(c *app.Config) { c.Packs = apitest.Packs(t, "testdata/packs") })
	if err := e.bootstrap(t, jsonOf(t, doc)); err != nil {
		t.Fatalf("bootstrap the configuration: %v", err)
	}
	return e
}

// adopted is the document with company 1000 having adopted the US pack
// 2026.1 and renamed its Christmas holiday since.
func adopted(t *testing.T, version string) *config.Config {
	t.Helper()
	doc := document(t)
	doc.Packs = []config.PackAdoption{{Company: "1000", Country: "US", Version: version, AppliedOn: "2026-02-01"}}
	doc.Calendars = []config.Calendar{{Company: "1000", Holidays: []config.Holiday{
		{Date: "2026-07-04", Name: "Independence Day"}, {Date: "2026-12-25", Name: "Xmas"}}}}
	return doc
}

func apply(version, company string) string {
	return fmt.Sprintf(`{"country": "US", "version": %q, "company": %q, "applied_on": "2026-11-01", "fiscal_year": 2026}`, version, company)
}

func TestListCountryPacks(t *testing.T) {
	e := packEnv(t, document(t))
	page, _ := e.Must(200, "GET", "/v1/country-packs", "", "")
	items := apitest.List(t, page, "items")
	if len(items) != 2 {
		t.Fatalf("packs: got %d %v, want 2", len(items), items)
	}
	first := items[0].(map[string]any)
	for field, want := range map[string]any{"country": "US", "version": "2026.1", "name": "United States (test)", "valid_from": "2026-01-01", "signed_off": true} {
		if first[field] != want {
			t.Errorf("pack %s: got %v, want %v", field, first[field], want)
		}
	}
	if hash := apitest.String(t, first, "hash"); len(hash) != 64 {
		t.Errorf("pack hash: got %q, want 64 hex characters", hash)
	}
	if got := apitest.Object(t, first, "sign_off")["tax_specialist"]; got != "A. Specialist, Example Tax LLP" {
		t.Errorf("pack sign-off: got %v, want the tax specialist", got)
	}
	if got := apitest.List(t, first, "sources")[0].(map[string]any)["url"]; got != "https://example.org/5-usc-6103" {
		t.Errorf("pack source url: got %v, want https://example.org/5-usc-6103", got)
	}
	if got := apitest.Object(t, first, "contents")["holidays"]; got != float64(2) {
		t.Errorf("pack holidays: got %v, want 2", got)
	}
	second := items[1].(map[string]any)
	if second["signed_off"] != false {
		t.Errorf("unsigned pack signed_off: got %v, want false", second["signed_off"])
	}
	if second["sign_off"] != nil {
		t.Errorf("unsigned pack sign_off: got %v, want none", second["sign_off"])
	}

	t.Run("pages", func(t *testing.T) {
		c := e.In(t)
		one, _ := c.Must(200, "GET", "/v1/country-packs?page_size=1", "", "")
		tok := apitest.String(t, one, "next_page_token")
		next, _ := c.Must(200, "GET", "/v1/country-packs?page_size=1&page_token="+tok, "", "")
		if got := apitest.List(t, next, "items")[0].(map[string]any)["version"]; got != "2026.2" {
			t.Errorf("second page: got %v, want 2026.2", got)
		}
		if next["next_page_token"] != nil {
			t.Errorf("second page token: got %v, want none on the last page", next["next_page_token"])
		}
		c.Problem(400, "request.invalid_page_token", "GET", "/v1/country-packs?country=US&page_token="+tok, "", "")
		c.Problem(400, "request.invalid_parameter", "GET", "/v1/country-packs?page_size=0", "", "")
	})
	t.Run("by country", func(t *testing.T) {
		c := e.In(t)
		page, _ := c.Must(200, "GET", "/v1/country-packs?country=DE", "", "")
		if items := apitest.List(t, page, "items"); len(items) != 0 {
			t.Errorf("DE packs: got %v, want none", items)
		}
		page, _ = c.Must(200, "GET", "/v1/country-packs?country=US", "", "")
		if items := apitest.List(t, page, "items"); len(items) != 2 {
			t.Errorf("US packs: got %d, want 2", len(items))
		}
	})
}

// Applying a pack creates a draft change request with the merge's notes;
// it changes nothing until the request goes through the usual flow.
func TestApplyCountryPack(t *testing.T) {
	e := packEnv(t, document(t))
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/country-pack-applications", Tenant: e.ten.ID, As: e.ten.Human, Key: apitest.NewKey(),
		Body: apply("2026.1", "1000")})
	cr := apitest.JSON(t, r)
	if r.Code != 201 {
		t.Fatalf("apply: got %d %v, want 201", r.Code, cr)
	}
	if got, want := r.Header().Get("Location"), "/v1/change-requests/"+cr["id"].(string); got != want {
		t.Errorf("apply Location: got %s, want %s", got, want)
	}
	for field, want := range map[string]any{"title": "Apply the US country pack 2026.1 to company 1000", "status": "draft", "base_version": float64(1), "tier": float64(1)} {
		if cr[field] != want {
			t.Errorf("apply %s: got %v, want %v", field, cr[field], want)
		}
	}
	notes := apitest.List(t, cr, "notes")
	if len(notes) != 1 {
		t.Fatalf("apply notes: got %v, want the application's", notes)
	}
	if got := notes[0].(map[string]any)["path"]; got != "packs[1000]" {
		t.Errorf("apply note path: got %v, want packs[1000]", got)
	}
	raw, err := json.Marshal(apitest.Object(t, cr, "proposed"))
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := config.Parse(raw)
	if err != nil {
		t.Fatalf("proposed: %v", err)
	}
	want := config.PackAdoption{Company: "1000", Country: "US", Version: "2026.1", AppliedOn: "2026-11-01"}
	if got := proposed.Pack("1000"); got == nil || *got != want {
		t.Errorf("proposed adoption: got %+v, want %+v", got, want)
	}
	if got := proposed.Calendar("1000"); got == nil || len(got.Holidays) != 2 {
		t.Errorf("proposed calendar: got %+v, want the pack's two holidays", got)
	}
	got, _ := e.Must(200, "GET", "/v1/change-requests/"+cr["id"].(string), "", "")
	if len(apitest.List(t, got, "notes")) != 1 {
		t.Errorf("read notes: got %v, want the application's", got["notes"])
	}
	var detail string
	pgtest.Scan(t, `SELECT detail->'country_pack'->>'version' || ' ' || (detail->>'notes') FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'config.change_request.create'`, []any{e.ten.ID}, &detail)
	if detail != "2026.1 1" {
		t.Errorf("audit detail: got %q, want the pack version and one note", detail)
	}
	active, _ := e.Must(200, "GET", "/v1/config-versions?is_active=true", "", "")
	if got := apitest.List(t, active, "items")[0].(map[string]any)["number"]; got != float64(1) {
		t.Errorf("active version: got %v, want 1 (nothing activated)", got)
	}
	// The request goes on like any other: the calendar is operational, so
	// it is approved by people (tier 2).
	submitted, _ := e.Must(200, "POST", "/v1/change-requests/"+cr["id"].(string)+":submit", "", `"1"`)
	if submitted["tier"] != float64(2) {
		t.Errorf("submitted tier: got %v, want 2", submitted["tier"])
	}
	if submitted["status"] != "pending_approval" {
		t.Errorf("submitted status: got %v, want pending_approval", submitted["status"])
	}
	// A request without notes reads without them.
	plain := e.propose(t, 1, document(t))
	if _, present := plain["notes"]; present {
		t.Errorf("plain request notes: got %v, want none", plain["notes"])
	}
	// The previous version inserts no notes; the column's default is an
	// empty list, which reads as none.
	old := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO config.change_request (tenant_id, id, number, title, author_id, base_version, proposed, tier, status, created_by, updated_by)
		VALUES ($1, $2, 'CR-OLD', 'Old', $3, 1, '{}', 1, 'draft', $3, $3)`, e.ten.ID, old, e.ten.Human.ID)
	if got, _ := e.Must(200, "GET", "/v1/change-requests/"+old.String(), "", ""); got["notes"] != nil {
		t.Errorf("request of the previous version: got notes %v, want none", got["notes"])
	}
	t.Run("replayed", func(t *testing.T) {
		req := apitest.Req{Method: "POST", Path: "/v1/country-pack-applications", Tenant: e.ten.ID, As: e.ten.Human, Key: apitest.NewKey(),
			Body: apply("2026.1", "1000")}
		first, again := apitest.JSON(t, e.API.Do(req)), e.API.Do(req)
		if got := apitest.JSON(t, again)["id"]; got != first["id"] {
			t.Errorf("replayed id: got %v, want %v", got, first["id"])
		}
		if got := again.Header().Get("Idempotent-Replayed"); got != "true" {
			t.Errorf("Idempotent-Replayed: got %q, want true", got)
		}
	})
}

// An upgrade merges from the adopted version and notes the conflicts and
// the missing sign-off; titles and rationales are the caller's when given.
func TestApplyCountryPackUpgrade(t *testing.T) {
	e := packEnv(t, adopted(t, "2026.1"))
	cr, _ := e.Must(201, "POST", "/v1/country-pack-applications",
		`{"country": "US", "version": "2026.2", "company": "1000", "applied_on": "2026-11-01", "fiscal_year": 2026, "title": "US holidays 2026.2", "rationale": "Thanksgiving"}`, "")
	for field, want := range map[string]any{"title": "US holidays 2026.2", "rationale": "Thanksgiving"} {
		if cr[field] != want {
			t.Errorf("upgrade %s: got %v, want %v", field, cr[field], want)
		}
	}
	paths := map[string]string{}
	for _, n := range apitest.List(t, cr, "notes") {
		paths[n.(map[string]any)["path"].(string)] += n.(map[string]any)["message"].(string)
	}
	if got := paths["calendars[1000].holidays[2026-12-25]"]; !strings.Contains(got, "changed both here and in the pack") {
		t.Errorf("conflict note: got %q, want the renamed Christmas kept as the tenant has it", got)
	}
	for _, want := range []string{"upgrading from 2026.1", "no tax specialist"} {
		if got := paths["packs[1000]"]; !strings.Contains(got, want) {
			t.Errorf("pack notes: got %q, want one containing %q", got, want)
		}
	}
}

func TestApplyCountryPackRefusals(t *testing.T) {
	e := packEnv(t, document(t))
	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{"unknown version", apply("2025.9", "1000"), 404, "request.not_found"},
		{"company of no legal entity", apply("2026.1", "9999"), 422, "config.unknown_company"},
		{"company in another country", apply("2026.1", "2000"), 422, "config.pack_not_applicable"},
		{"no fiscal year", `{"country": "US", "version": "2026.1", "company": "1000", "applied_on": "2026-11-01"}`, 400, "request.invalid_body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.In(t).Problem(tt.status, tt.code, "POST", "/v1/country-pack-applications", tt.body, "")
		})
	}
	t.Run("why not applicable", func(t *testing.T) {
		m := e.In(t).Problem(422, "config.pack_not_applicable", "POST", "/v1/country-pack-applications", apply("2026.1", "2000"), "")
		if got := apitest.List(t, m, "errors")[0].(map[string]any)["message"]; got != `company 2000 is in "DE", the pack is for US` {
			t.Errorf("field message: got %v, want the reason without the package's prefix", got)
		}
	})
	t.Run("company outside access", func(t *testing.T) {
		// The agent may propose (level 2) but has no company access.
		agent := e.In(t)
		agent.As = e.ten.Agent
		agent.Problem(422, "config.unknown_company", "POST", "/v1/country-pack-applications", apply("2026.1", "1000"), "")
		e.ten.AllCompanies(t, e.ten.Agent)
		agent.Must(201, "POST", "/v1/country-pack-applications", apply("2026.1", "1000"), "")
	})
	t.Run("downgrade", func(t *testing.T) {
		newer := packEnv(t, adopted(t, "2026.2"))
		newer.Problem(409, "config.pack_downgrade", "POST", "/v1/country-pack-applications", apply("2026.1", "1000"), "")
	})
	t.Run("adopted version not in the image", func(t *testing.T) {
		gone := packEnv(t, adopted(t, "2025.1"))
		gone.Problem(500, "platform.internal", "POST", "/v1/country-pack-applications", apply("2026.1", "1000"), "")
	})
	t.Run("not bootstrapped", func(t *testing.T) {
		bare := setup(t, true)
		bare.API = e.API
		bare.Problem(409, "config.not_bootstrapped", "POST", "/v1/country-pack-applications", apply("2026.1", "1000"), "")
	})
}

func TestApplyCountryPackDatabaseFailures(t *testing.T) {
	e := packEnv(t, document(t))
	h := packEndpoints(t)
	on := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	apitest.EveryFailure(t, e.ten, e.ten.Human, "apply", func(ctx context.Context) error {
		_, err := h.ApplyCountryPack(ctx, &gen.CountryPackApplication{Country: "US", Version: "2026.1", Company: "1000", AppliedOn: on, FiscalYear: 2026},
			gen.ApplyCountryPackParams{})
		return err
	})
	if _, err := configuration.ApplyCountryPack(context.Background(), deps(), h.Packs, configuration.PackApplication{}); err == nil {
		t.Error("apply outside a request: got nil, want an error")
	}
	// The contract bounds the page size before the handler sees it.
	if _, err := h.ListCountryPacks(context.Background(), gen.ListCountryPacksParams{PageSize: gen.NewOptInt32(0)}); err == nil {
		t.Error("list with page size 0: got nil, want an error")
	}
}

func packEndpoints(t *testing.T) *configuration.Endpoints {
	t.Helper()
	lib, err := countrypack.LoadAll(apitest.Packs(t, "testdata/packs"))
	if err != nil {
		t.Fatal(err)
	}
	h := endpoints()
	h.Packs = lib
	return h
}
