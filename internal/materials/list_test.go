package materials_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// numbers are the numbers of a page's materials.
func numbers(t *testing.T, page map[string]any) string {
	t.Helper()
	var out []string
	for _, m := range rows(t, page, "items") {
		out = append(out, m["number"].(string))
	}
	return strings.Join(out, " ")
}

// TestList: materials by number, filtered by number, plant, status, type
// and group, paged; a plant a material lost lists it no more.
func TestList(t *testing.T) {
	e := setup(t)
	a := e.active(t, nil)
	b := e.active(t, map[string]any{"description": "Copper wire", "base_unit": "M", "material_type": "raw_material", "material_group": "WIRE",
		"plants": []any{map[string]any{"plant_id": e.p2, "storage_location_ids": []any{}}}, "alternative_units": nil})
	c := e.active(t, map[string]any{"description": "Installation", "base_unit": "H", "material_type": "service", "plants": nil, "alternative_units": nil})
	e.Must(200, "PATCH", path+"/"+c["id"].(string), `{"status": "blocked"}`, `"2"`)
	e.create(t, map[string]any{"description": "Pending bracket"})
	for name, tc := range map[string]struct {
		query string
		want  string
	}{
		"all":                  {"", "MA2000-100000 MA2000-100001 MA2000-100002 MA2000-100003"},
		"by number":            {"number=MA2000-100001", "MA2000-100001"},
		"by plant":             {"plant_id=" + e.p2.String(), "MA2000-100001"},
		"by status":            {"status=blocked", "MA2000-100002"},
		"pending":              {"status=pending", "MA2000-100003"},
		"by an unknown status": {"status=retired", ""},
		"by type":              {"material_type=raw_material", "MA2000-100001"},
		"by group":             {"material_group=BRACKETS", "MA2000-100000 MA2000-100002 MA2000-100003"},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := e.Must(200, "GET", path+"?"+tc.query, "", "")
			if n := numbers(t, got); n != tc.want {
				t.Errorf("got %q, want %q", n, tc.want)
			}
		})
	}
	first, _ := e.Must(200, "GET", path+"?page_size=2", "", "")
	if n := numbers(t, first); n != "MA2000-100000 MA2000-100001" {
		t.Errorf("first page: got %q, want the first two", n)
	}
	next, _ := e.Must(200, "GET", path+"?page_size=2&page_token="+url.QueryEscape(first["next_page_token"].(string)), "", "")
	if n := numbers(t, next); n != "MA2000-100002 MA2000-100003" || next["next_page_token"] != nil {
		t.Errorf("second page: got %q (next %v), want the last two alone", n, next["next_page_token"])
	}
	e.changed(t, b["id"].(string), `{"plants": []}`)
	if got, _ := e.Must(200, "GET", path+"?plant_id="+e.p2.String(), "", ""); numbers(t, got) != "" {
		t.Errorf("by a plant removed: got %q, want none", numbers(t, got))
	}
	listed, _ := e.Must(200, "GET", path+"?number=MA2000-100000", "", "")
	if got := plants(t, rows(t, listed, "items")[0]); got != "P1:S1" || a["number"] != "MA2000-100000" {
		t.Errorf("a listed material's plants: got %q, want P1:S1", got)
	}
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	id := uuid.MustParse(e.active(t, nil)["id"].(string))
	other := uuid.MustParse(e.active(t, map[string]any{"description": "Copper wire"})["id"].(string))
	pending := uuid.MustParse(e.change(t, other.String(), mustJSON(t, map[string]any{"material_group": "WIRE",
		"plants":            []any{map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}},
		"alternative_units": []any{map[string]any{"unit": "KG", "numerator": 3, "denominator": 1}},
		"descriptions":      []any{map[string]any{"language": "ar", "description": arabic}}}))["id"].(string))
	box := []gen.MaterialUnitInput{{Unit: "BOX", Numerator: 10, Denominator: 1}}
	where := []gen.MaterialPlantInput{{PlantID: e.p1, StorageLocationIds: []uuid.UUID{e.s2}}}
	arabicToo := []gen.MaterialDescription{{Language: "ar", Description: arabic}}
	decide := materials.Approvals{Endpoints: h}
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateMaterial(ctx, &gen.MaterialCreate{LegalEntityID: e.DE, Description: "Machined bracket M10", BaseUnit: "EA",
				MaterialType: "trading_good", MaterialGroup: "B", Plants: where, AlternativeUnits: box, ConfirmDuplicate: gen.NewOptBool(true),
				Descriptions: arabicToo},
				gen.CreateMaterialParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetMaterial(ctx, gen.GetMaterialParams{ID: id})
			return err
		},
		"the published API": func(ctx context.Context) error {
			_, err := materials.Materials{}.Materials(ctx, []uuid.UUID{id})
			return err
		},
		"the published API by number": func(ctx context.Context) error {
			_, err := materials.Materials{}.ByNumber(ctx, []string{"MA1"})
			return err
		},
		"change": func(ctx context.Context) error {
			_, err := h.UpdateMaterial(ctx, &gen.MaterialUpdate{Description: gen.NewOptString("Bolt"), Plants: where, AlternativeUnits: box,
				Descriptions: arabicToo},
				gen.UpdateMaterialParams{ID: id, IfMatch: `"2"`})
			return err
		},
		"block": func(ctx context.Context) error {
			_, err := h.UpdateMaterial(ctx, &gen.MaterialUpdate{Status: gen.NewOptString("blocked")}, gen.UpdateMaterialParams{ID: id, IfMatch: `"2"`})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListMaterials(ctx, gen.ListMaterialsParams{})
			return err
		},
		"get change": func(ctx context.Context) error {
			_, err := h.GetMaterialChange(ctx, gen.GetMaterialChangeParams{ID: pending})
			return err
		},
		"list changes": func(ctx context.Context) error {
			_, err := h.ListMaterialChanges(ctx, gen.ListMaterialChangesParams{})
			return err
		},
		"approved":  func(ctx context.Context) error { return decide.Approved(ctx, pending, uuid.Nil) },
		"rejected":  func(ctx context.Context) error { return decide.Rejected(ctx, pending, uuid.Nil, "No") },
		"cancelled": func(ctx context.Context) error { return decide.Cancelled(ctx, pending, uuid.Nil, "No") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("change list page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListMaterialChanges(ctx, gen.ListMaterialChangesParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
	t.Run("list page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListMaterials(ctx, gen.ListMaterialsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
