package inventory_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestStock: the stock now and as at the end of a day, the sum of the
// documents posted up to it, by material, plant and location; a location
// with none is left out, and another company's stock is not shown.
func TestStock(t *testing.T) {
	e := setup(t)
	first := e.post(t, "561", e.item("10", "EA", nil), e.item("3", "EA", map[string]any{"storage_location_id": e.s2}))
	e.post(t, "201", e.item("10", "EA", map[string]any{"cost_center_id": e.c1}))
	// The first document is dated a week ago, as if posted then: written
	// past its insert-only trigger, as fixture.CostCenter writes its rows.
	admin := pgtest.Admin(t)
	for _, sql := range []string{`SET session_replication_role = replica`, `UPDATE inventory.material_document SET posting_date = posting_date - 7 WHERE id = $1`,
		`RESET session_replication_role`} {
		var args []any
		if strings.Contains(sql, "$1") {
			args = []any{first["id"]}
		}
		if _, err := admin.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	day := func(days int) string { return "&as_of=" + time.Now().AddDate(0, 0, days).Format(time.DateOnly) }
	for _, c := range []struct{ name, query, want string }{
		{"now", "", "S2 3 EA"},
		{"as at four days ago", day(-4), "S1 10 EA, S2 3 EA"},
		{"as at eight days ago", day(-8), ""},
		{"at P1's S1 now", "&storage_location_id=" + e.s1.String(), ""},
		{"at P2", "&plant_id=" + e.p2.String() + day(-4), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.Must(200, "GET", "/v1/stock?material_id="+e.bracket+c.query, "", "")
			var got []string
			for _, s := range rows(t, page, "items") {
				q := s["quantity"].(map[string]any)
				got = append(got, fmt.Sprintf("%v %v %v", s["storage_location_code"], q["value"], q["unit"]))
			}
			if strings.Join(got, ", ") != c.want {
				t.Errorf("got %q, want %q", strings.Join(got, ", "), c.want)
			}
		})
	}
	outsider := e.Reader(t, "Outsider", e.US)
	page, _ := e.As(outsider).Must(200, "GET", "/v1/stock", "", "")
	if n := len(rows(t, page, "items")); n != 0 {
		t.Errorf("another company's reader: got %d rows, want none", n)
	}
	page, _ = e.As(outsider).Must(200, "GET", docs, "", "")
	if n := len(rows(t, page, "items")); n != 0 {
		t.Errorf("another company's reader's documents: got %d, want none", n)
	}
}

// TestLists: documents the latest first, filtered by material, plant and
// movement type, and stock, each in pages.
func TestLists(t *testing.T) {
	e := setup(t)
	other := e.material(t, "Machined bracket M12", "trading_good", true)
	e.value(t, other, e.p2, "standard", "1.00")
	e.post(t, "561", e.item("10", "EA", nil))
	e.post(t, "561", e.item("10", "EA", map[string]any{"material_id": other, "plant_id": e.p2, "storage_location_id": e.s3}))
	e.post(t, "311", e.item("1", "EA", map[string]any{"to_storage_location_id": e.s2}))
	types := func(page map[string]any) string {
		var out []string
		for _, d := range rows(t, page, "items") {
			out = append(out, fmt.Sprint(d["movement_type"]))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, query, want string }{
		{"all", "", "311 561 561"},
		{"of the other material", "?material_id=" + other, "561"},
		{"at P2", "?plant_id=" + e.p2.String(), "561"},
		{"of transfers", "?movement_type=311", "311"},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.Must(200, "GET", docs+c.query, "", "")
			if got := types(page); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	page, _ := e.Must(200, "GET", docs+"?page_size=2", "", "")
	next, _ := e.Must(200, "GET", docs+"?page_size=2&page_token="+page["next_page_token"].(string), "", "")
	if got := types(page) + " | " + types(next); got != "311 561 | 561" {
		t.Errorf("pages of documents: got %q, want 311 561 | 561", got)
	}
	stock, _ := e.Must(200, "GET", "/v1/stock?page_size=2", "", "")
	rest, _ := e.Must(200, "GET", "/v1/stock?page_size=2&page_token="+stock["next_page_token"].(string), "", "")
	if got := len(rows(t, stock, "items")) + len(rows(t, rest, "items")); got != 3 {
		t.Errorf("pages of stock: got %d rows, want 3", got)
	}
	for _, path := range []string{docs + "?order_by=number", "/v1/stock?order_by=number"} {
		if status, got, _ := e.Do("GET", path, "", ""); status != 400 || got["code"] != "request.invalid_order_by" {
			t.Errorf("%s: got %d %v, want 400 request.invalid_order_by", path, status, got)
		}
	}
}

// TestDatabaseFailures calls every handler outside a unit of work and
// with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	received := e.post(t, "561", e.item("10", "EA", nil))
	id := uuid.MustParse(received["id"].(string))
	_, tag := e.Must(200, "GET", docs+"/"+id.String(), "", "")
	center := gen.NewOptUUID(e.c1)
	material := uuid.MustParse(e.bracket)
	other := uuid.MustParse(e.material(t, "Unvalued bracket", "trading_good", true))
	page, _ := e.Must(200, "GET", "/v1/material-valuations?plant_id="+e.p1.String(), "", "")
	valuation := uuid.MustParse(rows(t, page, "items")[0]["id"].(string))
	_, valuationTag := e.Must(200, "GET", "/v1/material-valuations/"+valuation.String(), "", "")
	page, _ = e.Must(200, "GET", "/v1/material-valuations?plant_id="+e.p2.String(), "", "")
	empty := uuid.MustParse(rows(t, page, "items")[0]["id"].(string))
	_, emptyTag := e.Must(200, "GET", "/v1/material-valuations/"+empty.String(), "", "")
	calls := map[string]func(ctx context.Context) error{
		"post an issue": func(ctx context.Context) error {
			_, err := h.CreateMaterialDocument(ctx, &gen.MaterialDocumentCreate{MovementType: "201", Items: []gen.MaterialDocumentItemInput{{MaterialID: material,
				PlantID: e.p1, StorageLocationID: e.s1, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, CostCenterID: center}}}, gen.CreateMaterialDocumentParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetMaterialDocument(ctx, gen.GetMaterialDocumentParams{ID: id})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := h.ReverseMaterialDocument(ctx, gen.ReverseMaterialDocumentParams{ID: id, IfMatch: tag})
			return err
		},
		"list documents": func(ctx context.Context) error {
			_, err := h.ListMaterialDocuments(ctx, gen.ListMaterialDocumentsParams{})
			return err
		},
		"list stock": func(ctx context.Context) error {
			_, err := h.ListStock(ctx, gen.ListStockParams{})
			return err
		},
		"create a valuation": func(ctx context.Context) error {
			_, err := h.CreateMaterialValuation(ctx, &gen.MaterialValuationCreate{MaterialID: other, PlantID: e.p1, ValuationClass: "3000",
				PriceControl: "standard", Price: gen.Money{Amount: "1.00", Currency: "EUR"}}, gen.CreateMaterialValuationParams{})
			return err
		},
		"get a valuation": func(ctx context.Context) error {
			_, err := h.GetMaterialValuation(ctx, gen.GetMaterialValuationParams{ID: valuation})
			return err
		},
		"list valuations": func(ctx context.Context) error {
			_, err := h.ListMaterialValuations(ctx, gen.ListMaterialValuationsParams{})
			return err
		},
		"change a class": func(ctx context.Context) error {
			_, err := h.UpdateMaterialValuation(ctx, &gen.MaterialValuationUpdate{ValuationClass: "3100"}, gen.UpdateMaterialValuationParams{ID: empty,
				IfMatch: emptyTag})
			return err
		},
		"change a price": func(ctx context.Context) error {
			_, err := h.ChangeMaterialPrice(ctx, &gen.MaterialPriceChange{Price: gen.Money{Amount: "2.50", Currency: "EUR"}},
				gen.ChangeMaterialPriceParams{ID: valuation, IfMatch: valuationTag})
			return err
		},
		"list stock as at a day": func(ctx context.Context) error {
			_, err := h.ListStock(ctx, gen.ListStockParams{AsOf: gen.NewOptDate(time.Now())})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"documents": func(ctx context.Context) error {
			_, err := h.ListMaterialDocuments(ctx, gen.ListMaterialDocumentsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"stock": func(ctx context.Context) error {
			_, err := h.ListStock(ctx, gen.ListStockParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"valuations": func(ctx context.Context) error {
			_, err := h.ListMaterialValuations(ctx, gen.ListMaterialValuationsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		t.Run("page size of "+name, func(t *testing.T) {
			if p := problem.From(e.inRequest(t, call)); p.Code != "request.invalid_page_size" {
				t.Errorf("got %v, want request.invalid_page_size", p)
			}
		})
	}
}
