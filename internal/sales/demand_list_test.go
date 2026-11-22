package sales_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestListByPlantAndMaterial (S2m): listSalesOrders finds the orders
// delivering from a plant and those with a standing item of a material,
// both together, each order answered whole; an order whose item of the
// material a change removed has none, and an order naming no plant is no
// plant's.
func TestListByPlantAndMaterial(t *testing.T) {
	e := setup(t)
	p1, p2 := e.plant(t, "P1", ""), e.plant(t, "P2", "")
	hinge, other := e.stocked(t, "100000031"), e.stocked(t, "100000032")
	of := func(m uuid.UUID) string {
		return e.ofMaterial(m, "2 EA", `, "unit_price": {"amount": "3.00", "currency": "EUR"}`)
	}
	order := func(plant uuid.UUID, items ...string) string {
		extra := ""
		if plant != uuid.Nil {
			extra = fmt.Sprintf(`, "plant_id": %q`, plant)
		}
		o, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, extra, items...), "")
		return o["id"].(string)
	}
	both := order(p1, e.item(line{}), of(hinge))
	elsewhere := order(p2, of(hinge))
	others := order(p1, of(other))
	none := order(uuid.Nil, of(hinge))
	removed := order(p1, of(other), of(hinge))
	e.Must(200, "POST", changing(removed), `{"removed_items": [2]}`, e.version(t, removed))
	ids := func(path string) []string {
		page, _ := e.Must(200, "GET", path, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		slices.Sort(out)
		return out
	}
	sorted := func(s ...string) []string { slices.Sort(s); return s }
	for name, c := range map[string]struct {
		query string
		want  []string
	}{
		"the plant's":                {"plant_id=" + p1.String(), sorted(both, others, removed)},
		"the material's":             {"material_id=" + hinge.String(), sorted(both, elsewhere, none)},
		"the material's at a plant":  {"plant_id=" + p1.String() + "&material_id=" + hinge.String(), []string{both}},
		"a page of them":             {"page_size=1&plant_id=" + p1.String() + "&material_id=" + hinge.String(), []string{both}},
		"of no such material":        {"material_id=" + uuid.NewString(), nil},
		"of no such plant":           {"plant_id=" + uuid.NewString(), nil},
		"the other material's at P1": {"plant_id=" + p1.String() + "&material_id=" + other.String(), sorted(others, removed)},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ids("/v1/sales-orders?" + c.query); !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	page, _ := e.Must(200, "GET", "/v1/sales-orders?material_id="+hinge.String()+"&plant_id="+p1.String(), "", "")
	if n := len(page["items"].([]any)[0].(map[string]any)["items"].([]any)); n != 2 {
		t.Errorf("the order's items: got %d, want 2: an order is answered whole", n)
	}
}
