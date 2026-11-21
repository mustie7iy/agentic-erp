package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const serials = "/v1/serial-numbers"

// serialEnv is env with the gauge, a serial-managed material kept where
// the bracket is, valued in P1 and P2 at a moving price of EUR 1.00.
type serialEnv struct {
	env
	gauge string
}

func serialSetup(t *testing.T) serialEnv {
	t.Helper()
	e := serialEnv{env: setupWith(t, append([]string{"inventory.count.write"}, perms...)...)}
	e.gauge = e.serialMaterial(t, "Pressure gauge")
	return e
}

// serialMaterial creates an active serial-managed trading good kept where
// the bracket is, valued in P1 and P2.
func (e env) serialMaterial(t *testing.T, description string) string {
	t.Helper()
	id := e.serialManaged(t, description)
	e.value(t, id, e.p1, "moving_average", "1.00")
	e.value(t, id, e.p2, "moving_average", "1.00")
	return id
}

// serialManaged creates an active serial-managed trading good kept where
// the bracket is, valued nowhere.
func (e env) serialManaged(t *testing.T, description string) string {
	t.Helper()
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": description, "base_unit": "EA",
		"material_type": "trading_good", "material_group": "GAUGES", "confirm_duplicate": true, "serial_managed": true, "plants": []any{
			map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1, e.s2}},
			map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	return m["id"].(string)
}

// of is an item of the gauge at S1 of P1 moving the serial numbers, one
// unit each, with over applied.
func (e serialEnv) of(over map[string]any, numbers ...string) map[string]any {
	it := e.item(fmt.Sprint(len(numbers)), "EA", map[string]any{"material_id": e.gauge, "serial_numbers": numbers})
	for k, v := range over {
		if v == nil {
			delete(it, k)
			continue
		}
		it[k] = v
	}
	return it
}

// places are the gauge's serial numbers as "number:location", sorted, the
// location "none" for one in no stock.
func (e serialEnv) places(t *testing.T, query string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", serials+"?material_id="+e.gauge+query, "", "")
	var out []string
	for _, r := range rows(t, page, "items") {
		where := "none"
		if r["in_stock"] == true {
			where = fmt.Sprint(r["storage_location_code"])
		}
		out = append(out, fmt.Sprintf("%v:%v", r["number"], where))
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// serialLines are a document's lines as "line:quantity:serial numbers".
func serialLines(d map[string]any) string {
	var out []string
	for _, l := range d["lines"].([]any) {
		l := l.(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v:%v", l["line_no"], l["quantity"].(map[string]any)["value"], l["serial_numbers"]))
	}
	return strings.Join(out, " ")
}

// serialID is the id of the gauge's serial number.
func (e serialEnv) serialID(t *testing.T, number string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", serials+"?material_id="+e.gauge+"&number="+number, "", "")
	items := rows(t, page, "items")
	if len(items) != 1 {
		t.Fatalf("serial number %s: got %d, want 1", number, len(items))
	}
	return items[0]["id"].(string)
}

// TestSerialMovements: a receipt names the serial numbers of the units it
// receives and creates them, a transfer moves them, an issue takes them
// out of stock and a reversal moves its original's back; a serial number
// is in stock at most once, so receiving one in stock is refused, as is
// issuing one not in stock at the item's location; each has its history.
func TestSerialMovements(t *testing.T) {
	e := serialSetup(t)
	receipt := e.post(t, "501", e.of(nil, "G2", "G1"), e.item("2", "EA", nil))
	if got := serialLines(receipt); got != "1:2:[G1 G2] 2:2:<nil>" {
		t.Errorf("the receipt's lines: got %s, want G1 and G2, and the bracket's none", got)
	}
	ev := e.events(t, receipt["id"].(string))
	if len(ev) != 1 || fmt.Sprint(rows(t, ev[0], "lines")[0]["serial_numbers"]) != "[G1 G2]" {
		t.Errorf("the event: got %v, want its line naming G1 and G2", ev)
	}
	transfer := e.post(t, "311", e.of(map[string]any{"to_storage_location_id": e.s2}, "G1"))
	if got := serialLines(transfer); got != "1:-1:[G1] 2:1:[G1]" {
		t.Errorf("the transfer's lines: got %s, want G1 out of S1 and into S2", got)
	}
	issue := e.post(t, "201", e.of(map[string]any{"cost_center_id": e.c1}, "G2"))
	if got := e.places(t, ""); got != "G1:S2 G2:none" {
		t.Errorf("the serial numbers: got %s, want G1 at S2, G2 in no stock", got)
	}
	for name, c := range map[string]struct {
		movementType string
		item         map[string]any
		code         string
	}{
		"a receipt of one in stock":   {"501", e.of(nil, "G1"), "inventory.serial_in_stock"},
		"an issue of one issued":      {"201", e.of(map[string]any{"cost_center_id": e.c1}, "G2"), "inventory.serial_not_in_stock"},
		"an issue of one never known": {"551", e.of(map[string]any{"cost_center_id": e.c1, "storage_location_id": e.s2}, "G9"), "inventory.serial_not_in_stock"},
		"an issue at another place":   {"311", e.of(map[string]any{"storage_location_id": e.s2, "to_storage_location_id": e.s1}, "G3"), "inventory.serial_not_in_stock"},
	} {
		t.Run(name, func(t *testing.T) {
			status, p, _ := e.Do("POST", docs, body(t, c.movementType, c.item), "")
			if status != 409 || p["code"] != c.code {
				t.Errorf("got %d %v, want 409 %s", status, p["code"], c.code)
			}
		})
	}
	_, tag := e.Must(200, "GET", docs+"/"+issue["id"].(string), "", "")
	e.Must(200, "POST", docs+"/"+issue["id"].(string)+":reverse", "", tag)
	_, tag = e.Must(200, "GET", docs+"/"+transfer["id"].(string), "", "")
	reversal, _ := e.Must(200, "POST", docs+"/"+transfer["id"].(string)+":reverse", "", tag)
	if got := serialLines(reversal); got != "1:-1:[G1] 2:1:[G1]" {
		t.Errorf("the transfer's reversal: got %s, want G1 back from S2 to S1", got)
	}
	if got := e.places(t, ""); got != "G1:S1 G2:S1" {
		t.Errorf("after the reversals: got %s, want both at S1", got)
	}
	e.post(t, "551", e.of(map[string]any{"cost_center_id": e.c1}, "G1"))
	g2, tag := e.Must(200, "GET", serials+"/"+e.serialID(t, "G2"), "", "")
	if tag == "" {
		t.Error("the serial number's ETag: got none, want one")
	}
	var history []string
	for _, h := range rows(t, g2, "history") {
		history = append(history, fmt.Sprintf("%v %v %v", h["movement_type"], h["direction"], h["storage_location_code"]))
	}
	if got := strings.Join(history, ", "); got != "501 received S1, 201 issued S1, 202 received S1" {
		t.Errorf("G2's history: got %s, want its receipt, issue and the issue's reversal", got)
	}
	if got := fmt.Sprintf("%v %v %v %v", g2["material_description"], g2["plant_code"], g2["company_code"], g2["in_stock"]); got != "Pressure gauge P1 2000 true" {
		t.Errorf("G2: got %s, want the gauge in stock at P1 of company 2000", got)
	}
	for query, want := range map[string]string{
		"&in_stock=true":                                       "G2:S1",
		"&in_stock=false":                                      "G1:none",
		"&number=G1":                                           "G1:none",
		"&plant_id=" + e.p1.String():                           "G2:S1",
		"&storage_location_id=" + e.s2.String():                "",
		"&plant_id=" + e.p2.String() + "&in_stock=true":        "",
		"&storage_location_id=" + e.s1.String() + "&number=G2": "G2:S1",
	} {
		t.Run(query, func(t *testing.T) {
			if got := e.places(t, query); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	first, _ := e.Must(200, "GET", serials+"?page_size=1&material_id="+e.gauge, "", "")
	second, _ := e.Must(200, "GET", serials+"?page_size=1&material_id="+e.gauge+"&page_token="+first["next_page_token"].(string), "", "")
	if got := len(rows(t, first, "items")) + len(rows(t, second, "items")); got != 2 {
		t.Errorf("two pages of one: got %d rows, want 2", got)
	}
	if status, _, _ := e.Do("GET", serials+"/"+uuid.NewString(), "", ""); status != 404 {
		t.Errorf("an unknown serial number: got %d, want 404", status)
	}
}

// TestSerialRefusals: a serial-managed material's item names one serial
// number for each whole unit, each once in the document, another
// material's none, and the flows that carry no serial numbers yet refuse
// a serial-managed material.
func TestSerialRefusals(t *testing.T) {
	e := serialSetup(t)
	for name, c := range map[string]struct {
		items       []map[string]any
		code, field string
	}{
		"too few":            {[]map[string]any{e.of(map[string]any{"quantity": map[string]any{"value": "2", "unit": "EA"}}, "G1")}, "inventory.serial_required", "items[0].serial_numbers"},
		"none":               {[]map[string]any{e.of(map[string]any{"serial_numbers": nil, "quantity": map[string]any{"value": "1", "unit": "EA"}})}, "inventory.serial_required", "items[0].serial_numbers"},
		"part of a unit":     {[]map[string]any{e.of(map[string]any{"quantity": map[string]any{"value": "1.5", "unit": "EA"}}, "G1")}, "inventory.serial_quantity", "items[0].quantity"},
		"twice in an item":   {[]map[string]any{e.of(nil, "G1", "G1")}, "inventory.serial_repeated", "items[0].serial_numbers[1]"},
		"twice in the items": {[]map[string]any{e.of(nil, "G1"), e.of(map[string]any{"storage_location_id": e.s2}, "G2", "G1")}, "inventory.serial_repeated", "items[1].serial_numbers[1]"},
		"on the bracket":     {[]map[string]any{e.item("1", "EA", map[string]any{"serial_numbers": []string{"G1"}})}, "inventory.serial_not_allowed", "items[0].serial_numbers"},
	} {
		t.Run(name, func(t *testing.T) {
			status, p, _ := e.Do("POST", docs, body(t, "501", c.items...), "")
			if status != 422 || p["code"] != c.code || field(p) != c.field {
				t.Errorf("got %d %v %v, want 422 %s naming %s", status, p["code"], field(p), c.code, c.field)
			}
		})
	}
	for name, number := range map[string]string{"a number in lower case": "g1", "a number of 19": strings.Repeat("G", 19)} {
		t.Run(name, func(t *testing.T) {
			status, p, _ := e.Do("POST", docs, body(t, "501", e.of(nil, number)), "")
			if status != 400 || p["code"] != "request.invalid_body" {
				t.Errorf("got %d %v, want 400 request.invalid_body", status, p["code"])
			}
		})
	}
	if got := e.places(t, ""); got != "" {
		t.Errorf("after the refusals: got serial numbers %s, want none", got)
	}
	status, p, _ := e.Do("POST", "/v1/physical-inventory-documents", e.countBody(t, e.p1, e.s1, e.gauge), "")
	if status != 422 || p["code"] != "inventory.serial_not_supported" {
		t.Errorf("a count of the gauge: got %d %v, want 422 inventory.serial_not_supported", status, p["code"])
	}
	h, m := endpoints(t), uuid.MustParse(e.gauge)
	calls := map[string]func(ctx context.Context) error{
		"purchase order receipt": func(ctx context.Context) error {
			_, err := h.ReceiveForOrder(ctx, inventory.OrderReceipt{Company: e.DE, Day: time.Now(), Reference: "GR1", Items: []inventory.OrderItem{{MaterialID: m,
				PlantID: e.p1, StorageLocationID: e.s1, Quantity: "1", Unit: "EA"}}},
				func([]ledger.GRIRStock) (ledger.Entry, []ledger.StockPosted, error) {
					return ledger.Entry{}, nil, errors.New("not posted")
				})
			return err
		},
		"delivery": func(ctx context.Context) error {
			_, err := h.IssueForDelivery(ctx, "OD1", []inventory.DeliveryItem{{MaterialID: m, PlantID: e.p1, StorageLocationID: e.s1, Quantity: "1", Unit: "EA"}})
			return err
		},
		"stock transport": func(ctx context.Context) error {
			_, _, err := h.TransportQuantities(ctx, []inventory.TransportItem{{MaterialID: m, SupplyingPlantID: e.p1, SupplyingStorageLocation: e.s1,
				ReceivingPlantID: e.p2, ReceivingStorageLocation: e.s3, Quantity: "1", Unit: "EA"}})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			var p *problem.Problem
			if err := e.inRequest(t, call); !errors.As(err, &p) || p.Code != "inventory.serial_not_supported" {
				t.Errorf("got %v, want inventory.serial_not_supported", err)
			}
		})
	}
}

// TestSerialMovementsAtOnce: two receipts of one new serial number at
// once at two locations post one, as do two issues of one; receipts at
// two locations, so under no common stock row, of the same serial
// numbers named in opposite order, known or new, post one each time,
// none waiting on another for good; transfers either way round between
// two locations at once all post.
func TestSerialMovementsAtOnce(t *testing.T) {
	e := serialSetup(t)
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, at := range []uuid.UUID{e.s1, e.s2} {
		wg.Go(func() {
			statuses[i], _, _ = e.Do("POST", docs, body(t, "501", e.of(map[string]any{"storage_location_id": at}, "N1")), "")
		})
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[201 409]" && fmt.Sprint(statuses) != "[409 201]" {
		t.Errorf("two receipts of N1: got %v, want one 201 and one 409", statuses)
	}
	e.post(t, "501", e.of(nil, "X1", "X2", "X3", "X4", "X5"), e.of(map[string]any{"storage_location_id": e.s2}, "Y1", "Y2", "Y3", "Y4", "Y5"))
	where := e.places(t, "&number=N1")
	from := e.s1
	if strings.HasSuffix(where, "S2") {
		from = e.s2
	}
	for i := range statuses {
		wg.Go(func() {
			statuses[i], _, _ = e.Do("POST", docs, body(t, "551", e.of(map[string]any{"cost_center_id": e.c1, "storage_location_id": from}, "N1")), "")
		})
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[201 409]" && fmt.Sprint(statuses) != "[409 201]" {
		t.Errorf("two issues of N1: got %v, want one 201 and one 409", statuses)
	}
	e.post(t, "501", e.of(nil, "K1", "K2"))
	e.post(t, "551", e.of(map[string]any{"cost_center_id": e.c1}, "K1", "K2"))
	for round, pair := range [][2]string{{"K1", "K2"}, {"M1", "M2"}} {
		for i, at := range []uuid.UUID{e.s1, e.s2} {
			numbers := []string{pair[0], pair[1]}
			if i == 1 {
				numbers = []string{pair[1], pair[0]}
			}
			wg.Go(func() {
				statuses[i], _, _ = e.Do("POST", docs, body(t, "501", e.of(map[string]any{"storage_location_id": at}, numbers...)), "")
			})
		}
		wg.Wait()
		if fmt.Sprint(statuses) != "[201 409]" && fmt.Sprint(statuses) != "[409 201]" {
			t.Errorf("round %d, receipts of %v in opposite order: got %v, want one 201 and one 409", round, pair, statuses)
		}
		first, second := e.places(t, "&number="+pair[0]), e.places(t, "&number="+pair[1])
		if first[len(pair[0]):] != second[len(pair[1]):] || strings.HasSuffix(first, ":none") {
			t.Errorf("round %d: got %s and %s, want the pair in stock together where its one receipt put it", round, first, second)
		}
	}
	transfers := make([]int, 5)
	for i := range transfers {
		x := e.of(map[string]any{"to_storage_location_id": e.s2}, fmt.Sprintf("X%d", i+1))
		y := e.of(map[string]any{"storage_location_id": e.s2, "to_storage_location_id": e.s1}, fmt.Sprintf("Y%d", i+1))
		items := []map[string]any{x, y}
		if i%2 == 1 {
			items = []map[string]any{y, x}
		}
		wg.Go(func() {
			transfers[i], _, _ = e.Do("POST", docs, body(t, "311", items...), "")
		})
	}
	wg.Wait()
	for i, s := range transfers {
		if s != 201 {
			t.Errorf("transfer %d: got %d, want 201", i, s)
		}
	}
	ys := slices.DeleteFunc(strings.Fields(e.places(t, "&storage_location_id="+e.s1.String())), func(p string) bool { return !strings.HasPrefix(p, "Y") })
	if got := strings.Join(ys, " "); got != "Y1:S1 Y2:S1 Y3:S1 Y4:S1 Y5:S1" {
		t.Errorf("at S1: got %s, want the Ys", got)
	}
}

// TestSerialDatabaseFailures: every database error of the serial number
// operations is answered as an error, none outside a unit of work.
func TestSerialDatabaseFailures(t *testing.T) {
	e := serialSetup(t)
	e.post(t, "501", e.of(nil, "G1", "G0"))
	h := endpoints(t)
	id, m := uuid.MustParse(e.serialID(t, "G1")), uuid.MustParse(e.gauge)
	calls := map[string]func(ctx context.Context) error{
		"get": func(ctx context.Context) error {
			_, err := h.GetSerialNumber(ctx, gen.GetSerialNumberParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListSerialNumbers(ctx, gen.ListSerialNumbersParams{})
			return err
		},
	}
	calls["post"] = func(ctx context.Context) error {
		_, err := h.CreateMaterialDocument(ctx, &gen.MaterialDocumentCreate{MovementType: "311", Items: []gen.MaterialDocumentItemInput{{MaterialID: m,
			PlantID: e.p1, StorageLocationID: e.s1, Quantity: gen.Quantity{Value: "2", Unit: "EA"}, ToStorageLocationID: gen.NewOptUUID(e.s2),
			SerialNumbers: []string{"G1", "G0"}}}}, gen.CreateMaterialDocumentParams{})
		return err
	}
	calls["receive"] = func(ctx context.Context) error {
		_, err := h.CreateMaterialDocument(ctx, &gen.MaterialDocumentCreate{MovementType: "501", Items: []gen.MaterialDocumentItemInput{{MaterialID: m,
			PlantID: e.p1, StorageLocationID: e.s2, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, SerialNumbers: []string{"R" + strings.ToUpper(uuid.NewString()[:8])}}}},
			gen.CreateMaterialDocumentParams{})
		return err
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("list page size", func(t *testing.T) {
		if err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListSerialNumbers(ctx, gen.ListSerialNumbersParams{PageSize: gen.NewOptInt32(0)})
			return err
		}); err == nil {
			t.Error("a page of no rows: got nil error, want an error")
		}
	})
}

// TestSerialManyUnits: an item of the contract's 1000 serial numbers is
// received and transferred, each line counted once at commit.
func TestSerialManyUnits(t *testing.T) {
	e := serialSetup(t)
	numbers := make([]string, 1000)
	for i := range numbers {
		numbers[i] = fmt.Sprintf("U%04d", i)
	}
	start := time.Now()
	e.post(t, "501", e.of(nil, numbers...))
	e.post(t, "311", e.of(map[string]any{"to_storage_location_id": e.s2}, numbers...))
	t.Logf("a receipt and a transfer of 1000 serial numbers: %v", time.Since(start))
	page, _ := e.Must(200, "GET", serials+"?page_size=1&in_stock=true&storage_location_id="+e.s2.String()+"&material_id="+e.gauge, "", "")
	if items := rows(t, page, "items"); len(items) != 1 || page["next_page_token"] == nil {
		t.Errorf("at S2: got %v, want a first page of the 1000", page)
	}
}
