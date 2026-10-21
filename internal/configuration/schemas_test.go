package configuration_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// TestGetSchema: the active schema of an entity, tagged with the
// configuration version, 304 while that version is active, and 404 for an
// entity without one; reading it takes the entity's read permission.
func TestGetSchema(t *testing.T) {
	e := setup(t, false, "payables.invoice.read")
	e.Problem(404, "request.not_found", "GET", "/v1/schemas/supplier_invoice", "", "")
	no := false
	doc := document(t)
	doc.Extensions = map[string]extensions.Schema{"supplier_invoice": {Version: 1, Type: "object", AdditionalProperties: &no,
		Properties: map[string]extensions.Property{"project": {Type: "string", MaxLength: 20, PII: extensions.PIIPersonal}}}}
	cr := e.propose(t, 1, doc)
	for _, step := range []string{"submit", "activate"} {
		if code, m := e.transition(t, cr, step); code != 200 {
			t.Fatalf("%s: got %d %v, want 200", step, code, m)
		}
	}

	m, tag := e.Must(200, "GET", "/v1/schemas/supplier_invoice", "", "")
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want the configuration version \"2\"", tag)
	}
	for k, want := range map[string]any{"entity": "supplier_invoice", "version": float64(1), "config_version": float64(2)} {
		if m[k] != want {
			t.Errorf("%s: got %v, want %v", k, m[k], want)
		}
	}
	schema, _ := m["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Errorf("schema additionalProperties: got %v, want false", schema["additionalProperties"])
	}
	field, _ := schema["properties"].(map[string]any)["project"].(map[string]any)
	if field["x-pii"] != "personal" {
		t.Errorf("project x-pii: got %v, want personal", field["x-pii"])
	}

	for header, want := range map[string]int{`"2"`: 304, `W/"2"`: 304, `"1"`: 200} {
		t.Run("If-None-Match "+header, func(t *testing.T) {
			r := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/schemas/supplier_invoice", Tenant: e.ten.ID, As: e.ten.Human,
				Header: http.Header{"If-None-Match": {header}}})
			if r.Code != want {
				t.Errorf("status: got %d, want %d", r.Code, want)
			}
			if got := r.Header().Get("ETag"); got != `"2"` {
				t.Errorf("ETag: got %s, want \"2\"", got)
			}
		})
	}
	e.Problem(403, "identity.permission_missing", "GET", "/v1/schemas/purchase_order", "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/schemas/ledger_account", "", "")
	setup(t, true, "payables.invoice.read").Problem(404, "request.not_found", "GET", "/v1/schemas/supplier_invoice", "", "")

	get := func(ctx context.Context) error {
		_, err := (&configuration.Endpoints{}).GetSchema(ctx, gen.GetSchemaParams{Entity: "supplier_invoice"})
		return err
	}
	if err := get(context.Background()); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "get schema", get)
}

// TestSchemaPermissionsCoverEntities: the contract names a read permission
// for exactly the entities that take custom fields.
func TestSchemaPermissionsCoverEntities(t *testing.T) {
	got := slices.Sorted(maps.Keys(api.MustOperations()["getSchema"].EntityPermissions))
	if !slices.Equal(got, extensions.Entities) {
		t.Errorf("getSchema x-entity-permissions: got %v, want %v", got, extensions.Entities)
	}
}
