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

var batchPerms = append([]string{"inventory.batch.write", "inventory.count.write"}, perms...)

const batches = "/v1/batches"

// batchEnv is env with the sealant, a batch-managed material valued in P1
// and P2 at a moving price of EUR 1.00, and its batches L1 and L2.
type batchEnv struct {
	env
	sealant, l1, l2 string
}

func batchSetup(t *testing.T) batchEnv {
	t.Helper()
	e := batchEnv{env: setupWith(t, batchPerms...)}
	e.sealant = e.batchMaterial(t, "Thread sealant")
	e.l1 = e.batch(t, e.sealant, "L1", "2026-11-01", "2027-05-01")["id"].(string)
	e.l2 = e.batch(t, e.sealant, "L2", "2026-11-10", "")["id"].(string)
	return e
}

// batchMaterial creates an active batch-managed raw material kept where
// the bracket is, valued in P1 and P2.
func (e env) batchMaterial(t *testing.T, description string) string {
	t.Helper()
	id := e.batchManaged(t, description)
	e.value(t, id, e.p1, "moving_average", "1.00")
	e.value(t, id, e.p2, "moving_average", "1.00")
	return id
}

// batchManaged creates an active batch-managed raw material kept where the
// bracket is, valued nowhere.
func (e env) batchManaged(t *testing.T, description string) string {
	t.Helper()
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": description, "base_unit": "EA",
		"material_type": "raw_material", "material_group": "SEALANTS", "confirm_duplicate": true, "batch_managed": true, "plants": []any{
			map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1, e.s2}},
			map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	return m["id"].(string)
}

// batch creates a batch of the material; no expiry when expiry is "".
func (e env) batch(t *testing.T, material, number, made, expiry string) map[string]any {
	t.Helper()
	body := map[string]any{"material_id": material, "number": number, "manufacture_date": made}
	if expiry != "" {
		body["expiry_date"] = expiry
	}
	got, _ := e.Must(201, "POST", batches, mustJSON(t, body), "")
	return got
}

// of is an item of the sealant's batch at S1 of P1, with over applied.
func (e batchEnv) of(batch, qty string, over map[string]any) map[string]any {
	it := e.item(qty, "EA", map[string]any{"material_id": e.sealant, "batch_id": batch})
	for k, v := range over {
		if v == nil {
			delete(it, k)
			continue
		}
		it[k] = v
	}
	return it
}

// byBatch is the sealant's stock by batch as "location batch quantity",
// sorted, and its stock by location as "location quantity", sorted.
func (e batchEnv) byBatch(t *testing.T) (string, string) {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/batch-stock?material_id="+e.sealant, "", "")
	var bs []string
	for _, r := range rows(t, page, "items") {
		bs = append(bs, fmt.Sprintf("%v %v %v", r["storage_location_code"], r["batch_number"], r["quantity"].(map[string]any)["value"]))
	}
	page, _ = e.Must(200, "GET", "/v1/stock?material_id="+e.sealant, "", "")
	var ls []string
	for _, r := range rows(t, page, "items") {
		ls = append(ls, fmt.Sprintf("%v %v", r["storage_location_code"], r["quantity"].(map[string]any)["value"]))
	}
	slices.Sort(bs)
	slices.Sort(ls)
	return strings.Join(bs, ", "), strings.Join(ls, ", ")
}

// TestBatchMaster: a batch is created of a batch-managed material, read
// and listed; refused for another material, twice, or expiring before it
// was made.
func TestBatchMaster(t *testing.T) {
	e := batchSetup(t)
	got, tag := e.Must(200, "GET", batches+"/"+e.l1, "", "")
	if s := fmt.Sprintf("%v %v %v %v", got["number"], got["material_id"] == e.sealant, got["manufacture_date"], got["expiry_date"]); s != "L1 true 2026-11-01 2027-05-01" {
		t.Errorf("the batch: got %s, want L1 of the sealant made 2026-11-01, expiring 2027-05-01", s)
	}
	if tag == "" {
		t.Error("the batch's ETag: got none, want one")
	}
	if got["material_number"] == "" {
		t.Error("the batch's material number: got none, want the sealant's")
	}
	if got["material_description"] != "Thread sealant" {
		t.Errorf("the batch's material description: got %v, want Thread sealant", got["material_description"])
	}
	if l2, _ := e.Must(200, "GET", batches+"/"+e.l2, "", ""); l2["expiry_date"] != nil {
		t.Errorf("L2's expiry: got %v, want none", l2["expiry_date"])
	}
	other := e.batchMaterial(t, "Other sealant")
	e.batch(t, other, "L1", "2026-11-01", "")
	page, _ := e.Must(200, "GET", batches+"?number=L1&material_id="+e.sealant, "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != e.l1 {
		t.Errorf("the sealant's L1: got %v, want only it", items)
	}
	page, _ = e.Must(200, "GET", batches+"?material_id="+e.sealant, "", "")
	if items := rows(t, page, "items"); len(items) != 2 || items[0]["id"] != e.l2 {
		t.Errorf("the sealant's batches: got %v, want L2 then L1, the latest first", items)
	}
	first, _ := e.Must(200, "GET", batches+"?page_size=1&material_id="+e.sealant, "", "")
	second, _ := e.Must(200, "GET", batches+"?page_size=1&material_id="+e.sealant+"&page_token="+first["next_page_token"].(string), "", "")
	if items := rows(t, second, "items"); len(items) != 1 || items[0]["id"] != e.l1 {
		t.Errorf("the second page of one: got %v, want L1", items)
	}
	pending := e.material(t, "Pending bracket", "trading_good", false)
	for name, c := range map[string]struct {
		body         map[string]any
		status       int
		code, field_ string
	}{
		"twice":          {map[string]any{"material_id": e.sealant, "number": "L1", "manufacture_date": "2026-11-01"}, 409, "inventory.batch_exists", ""},
		"expiring early": {map[string]any{"material_id": e.sealant, "number": "L3", "manufacture_date": "2026-11-01", "expiry_date": "2026-10-31"}, 422, "inventory.batch_dates", "expiry_date"},
		"not batches":    {map[string]any{"material_id": e.bracket, "number": "L3", "manufacture_date": "2026-11-01"}, 422, "inventory.material_not_batch_managed", "material_id"},
		"no material":    {map[string]any{"material_id": uuid.NewString(), "number": "L3", "manufacture_date": "2026-11-01"}, 422, "inventory.material_invalid", "material_id"},
		"pending":        {map[string]any{"material_id": pending, "number": "L3", "manufacture_date": "2026-11-01"}, 422, "inventory.material_not_usable", "material_id"},
		"bad number":     {map[string]any{"material_id": e.sealant, "number": "l 3", "manufacture_date": "2026-11-01"}, 400, "request.invalid_body", ""},
	} {
		t.Run(name, func(t *testing.T) {
			status, p, _ := e.Do("POST", batches, mustJSON(t, c.body), "")
			if status != c.status || p["code"] != c.code {
				t.Errorf("got %d %v, want %d %s", status, p["code"], c.status, c.code)
			}
			if c.field_ != "" && field(p) != c.field_ {
				t.Errorf("field: got %v, want %s", field(p), c.field_)
			}
		})
	}
	if a := e.audits(t, e.l1, "inventory.batch.create"); len(a) != 1 || a[0]["number"] != "L1" || a[0]["expiry_date"] != "2027-05-01" {
		t.Errorf("the audit: got %v, want L1's creation with its expiry", a)
	}
	if status, _, _ := e.Do("GET", batches+"/"+uuid.NewString(), "", ""); status != 404 {
		t.Errorf("an unknown batch: got %d, want 404", status)
	}
}

// TestBatchMovements: every movement of a batch-managed material names a
// batch of it, a transfer keeps it and a reversal takes it back; a batch
// never goes below zero though its location holds more, and the batches
// at a location add up to its stock.
func TestBatchMovements(t *testing.T) {
	e := batchSetup(t)
	receipt := e.post(t, "561", e.of(e.l1, "10", nil), e.of(e.l2, "5", nil), e.item("2", "EA", nil))
	if got := batchLines(receipt); got != "1:L1:10 2:L2:5 3:<nil>:2" {
		t.Errorf("the receipt's lines: got %s, want L1 10, L2 5 and the bracket's 2 of no batch", got)
	}
	ev := e.events(t, receipt["id"].(string))
	if len(ev) != 1 || rows(t, ev[0], "lines")[0]["batch_number"] != "L1" || rows(t, ev[0], "lines")[0]["batch_id"] != e.l1 {
		t.Errorf("the event: got %v, want its lines naming their batches", ev)
	}
	transfer := e.post(t, "311", e.of(e.l1, "4", map[string]any{"to_storage_location_id": e.s2}))
	if got := batchLines(transfer); got != "1:L1:-4 2:L1:4" {
		t.Errorf("the transfer's lines: got %s, want L1 out of S1 and into S2", got)
	}
	e.post(t, "301", e.of(e.l2, "1", map[string]any{"to_plant_id": e.p2, "to_storage_location_id": e.s3}))
	status, p, _ := e.Do("POST", docs, body(t, "201", e.of(e.l2, "5", map[string]any{"cost_center_id": e.c1})), "")
	if status != 409 || p["code"] != "inventory.insufficient_stock" || !strings.Contains(p["detail"].(string), "Batch L2") {
		t.Errorf("an issue of more than the batch holds, though S1 holds 10: got %d %v, want 409 naming batch L2", status, p)
	}
	e.post(t, "201", e.of(e.l2, "4", map[string]any{"cost_center_id": e.c1}))
	batched, located := e.byBatch(t)
	if batched != "S1 L1 6, S2 L1 4, S3 L2 1" || located != "S1 6, S2 4, S3 1" {
		t.Errorf("stock by batch and by location: got %q and %q, want the batches adding up to each location", batched, located)
	}
	_, tag := e.Must(200, "GET", docs+"/"+transfer["id"].(string), "", "")
	reversal, _ := e.Must(200, "POST", docs+"/"+transfer["id"].(string)+":reverse", "", tag)
	if got := batchLines(reversal); got != "1:L1:-4 2:L1:4" {
		t.Errorf("the reversal's lines: got %s, want L1 back from S2 to S1", got)
	}
	if batched, _ = e.byBatch(t); batched != "S1 L1 10, S3 L2 1" {
		t.Errorf("stock by batch after the reversal: got %q, want L1's 10 back at S1", batched)
	}
	first, _ := e.Must(200, "GET", "/v1/batch-stock?page_size=1&material_id="+e.sealant, "", "")
	second, _ := e.Must(200, "GET", "/v1/batch-stock?page_size=1&material_id="+e.sealant+"&page_token="+first["next_page_token"].(string), "", "")
	if got := len(rows(t, first, "items")) + len(rows(t, second, "items")); got != 2 {
		t.Errorf("two pages of one: got %d rows, want 2", got)
	}
	page, _ := e.Must(200, "GET", "/v1/batch-stock?batch_id="+e.l2+"&plant_id="+e.p2.String()+"&storage_location_id="+e.s3.String(), "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["manufacture_date"] != "2026-11-10" || items[0]["company_code"] != "2000" {
		t.Errorf("L2 at S3: got %v, want its one row with its dates and company", items)
	}
}

// batchLines are a document's lines as "line:batch:quantity".
func batchLines(d map[string]any) string {
	var out []string
	for _, l := range d["lines"].([]any) {
		l := l.(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v:%v", l["line_no"], l["batch_number"], l["quantity"].(map[string]any)["value"]))
	}
	return strings.Join(out, " ")
}

// TestBatchRefusals: a batch-managed material's item names a batch of it,
// another material's none, and the flows that carry no batch yet refuse a
// batch-managed material.
func TestBatchRefusals(t *testing.T) {
	e := batchSetup(t)
	other := e.batchMaterial(t, "Other sealant")
	foreign := e.batch(t, other, "L9", "2026-11-01", "")["id"].(string)
	for name, c := range map[string]struct {
		item        map[string]any
		code, field string
	}{
		"no batch":            {e.of("", "1", map[string]any{"batch_id": nil}), "inventory.batch_required", "items[0].batch_id"},
		"another's batch":     {e.of(foreign, "1", nil), "inventory.batch_invalid", "items[0].batch_id"},
		"no such batch":       {e.of(uuid.NewString(), "1", nil), "inventory.batch_invalid", "items[0].batch_id"},
		"a batch of no batch": {e.item("1", "EA", map[string]any{"batch_id": e.l1}), "inventory.batch_not_allowed", "items[0].batch_id"},
	} {
		t.Run(name, func(t *testing.T) {
			status, p, _ := e.Do("POST", docs, body(t, "561", c.item), "")
			if status != 422 || p["code"] != c.code || field(p) != c.field {
				t.Errorf("got %d %v %v, want 422 %s naming %s", status, p["code"], field(p), c.code, c.field)
			}
		})
	}
	status, p, _ := e.Do("POST", "/v1/physical-inventory-documents", e.countBody(t, e.p1, e.s1, e.sealant), "")
	if status != 422 || p["code"] != "inventory.batch_not_supported" {
		t.Errorf("a count of the sealant: got %d %v, want 422 inventory.batch_not_supported", status, p["code"])
	}
	h, m := endpoints(t), uuid.MustParse(e.sealant)
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
			if err := e.inRequest(t, call); !errors.As(err, &p) || p.Code != "inventory.batch_not_supported" {
				t.Errorf("got %v, want inventory.batch_not_supported", err)
			}
		})
	}
}

// TestBatchIssuesAtOnce: two issues of a batch's last quantity at once
// post one, and transfers of two batches either way round at once all
// post, none waiting on another for good.
func TestBatchIssuesAtOnce(t *testing.T) {
	e := batchSetup(t)
	e.post(t, "561", e.of(e.l1, "3", nil), e.of(e.l2, "10", nil), e.of(e.l1, "10", map[string]any{"storage_location_id": e.s2}))
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Go(func() {
			statuses[i], _, _ = e.Do("POST", docs, body(t, "201", e.of(e.l1, "2", map[string]any{"cost_center_id": e.c1})), "")
		})
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[201 409]" && fmt.Sprint(statuses) != "[409 201]" {
		t.Errorf("statuses: got %v, want one 201 and one 409", statuses)
	}
	transfers := make([]int, 10)
	for i := range transfers {
		from, to, batch := e.s1, e.s2, e.l2
		if i%2 == 1 {
			from, to, batch = e.s2, e.s1, e.l1
		}
		wg.Go(func() {
			transfers[i], _, _ = e.Do("POST", docs, body(t, "311", e.of(batch, "1", map[string]any{"storage_location_id": from, "to_storage_location_id": to})), "")
		})
	}
	wg.Wait()
	for i, s := range transfers {
		if s != 201 {
			t.Errorf("transfer %d: got %d, want 201", i, s)
		}
	}
	if batched, located := e.byBatch(t); batched != "S1 L1 6, S1 L2 5, S2 L1 5, S2 L2 5" || located != "S1 11, S2 10" {
		t.Errorf("stock by batch and by location: got %q and %q, want the batches adding up to each location", batched, located)
	}
}

// TestBatchDatabaseFailures: every database error of the batch operations
// is answered as an error, none outside a unit of work.
func TestBatchDatabaseFailures(t *testing.T) {
	e := batchSetup(t)
	h := endpoints(t)
	id, m := uuid.MustParse(e.l1), uuid.MustParse(e.sealant)
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateBatch(ctx, &gen.BatchCreate{MaterialID: m, Number: "L" + strings.ToUpper(uuid.NewString()[:8]), ManufactureDate: time.Now()},
				gen.CreateBatchParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetBatch(ctx, gen.GetBatchParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListBatches(ctx, gen.ListBatchesParams{})
			return err
		},
		"stock": func(ctx context.Context) error {
			_, err := h.ListBatchStock(ctx, gen.ListBatchStockParams{})
			return err
		},
		"post": func(ctx context.Context) error {
			_, err := h.CreateMaterialDocument(ctx, &gen.MaterialDocumentCreate{MovementType: "561", Items: []gen.MaterialDocumentItemInput{{MaterialID: m,
				PlantID: e.p1, StorageLocationID: e.s1, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, BatchID: gen.NewOptUUID(id)}}},
				gen.CreateMaterialDocumentParams{})
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
		"list": func(ctx context.Context) error {
			_, err := h.ListBatches(ctx, gen.ListBatchesParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"stock": func(ctx context.Context) error {
			_, err := h.ListBatchStock(ctx, gen.ListBatchStockParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		t.Run(name+" page size", func(t *testing.T) {
			if err := e.inRequest(t, call); err == nil {
				t.Error("a page of no rows: got nil error, want an error")
			}
		})
	}
}
