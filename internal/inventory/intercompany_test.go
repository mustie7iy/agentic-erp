package inventory_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Intercompany stock transport orders (S2i1; ADR-0082 As built S2i),
// through the API: company 2000 orders a widget from company 1000's plant
// P3 on a purchase order naming it as its supplying plant, from the
// business partner that is company 1000; 1000 posts the goods issue
// (643) out of P3's stock against cost of goods sold, and 2000 receives
// it into S1 of P1 at the order price against GR/IR.

const orders = "/v1/purchase-orders"

// intercompany is receiving with company 1000's stock, initial entry and
// cost of goods sold accounts of class 3000.
func intercompany(c *config.Config) {
	receiving(c)
	us := c.Company("1000")
	us.ChartOfAccounts = append(us.ChartOfAccounts, config.Account{Number: "155000", Name: "Inventory", Type: config.Asset},
		config.Account{Number: "395000", Name: "Initial entries of stock", Type: config.Equity},
		config.Account{Number: "585000", Name: "Cost of goods sold", Type: config.Expense})
	us.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "155000", InitialEntry: "395000", CostOfGoodsSold: "585000"}}
}

// icEnv is orderEnv with the widget, kept at S1 of P1 (company 2000,
// moving price EUR 2.00) and S4 of P3 (company 1000, moving price USD
// 1.50, 20 EA in stock), and the supplier that is company 1000.
type icEnv struct {
	orderEnv
	widget  string
	sister  uuid.UUID
	account uuid.UUID
}

func setupIntercompany(t *testing.T) icEnv {
	t.Helper()
	e := icEnv{orderEnv: setupOrdersWith(t, intercompany, "procurement.stock_transport.post", "procurement.order.release")}
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": "Widget", "base_unit": "EA",
		"material_type": "trading_good", "material_group": "BRACKETS", "confirm_duplicate": true, "plants": []any{
			map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}, map[string]any{"plant_id": e.p3, "storage_location_ids": []any{e.s4}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	e.widget = m["id"].(string)
	e.value(t, e.widget, e.p1, "moving_average", "2.00")
	e.Must(201, "POST", "/v1/material-valuations", mustJSON(t, map[string]any{"material_id": e.widget, "plant_id": e.p3, "valuation_class": "3000",
		"price_control": "moving_average", "price": map[string]any{"amount": "1.50", "currency": "USD"}}), "")
	// Company 1000's postings are converted into the group currency, EUR.
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	e.post(t, "561", e.item("20", "EA", map[string]any{"material_id": e.widget, "plant_id": e.p3, "storage_location_id": e.s4}))
	e.sister = e.NewSupplier(t, "IC-1000")
	pgtest.Exec(t, `UPDATE partner.business_partner SET group_company_id = $2, version = version + 1 WHERE id = $1`, e.sister, e.US)
	e.account = e.expense
	return e
}

// icItem is an order item of 10 widgets at EUR 3.00 into S1 of P1, with
// fields replaced, added or (nil) taken out by over.
func (e icEnv) icItem(over map[string]any) map[string]any {
	it := map[string]any{"material_id": e.widget, "description": "Widget", "quantity": map[string]any{"value": "10", "unit": "EA"},
		"unit_price": amount("3.00"), "account_id": e.account, "plant_id": e.p1, "storage_location_id": e.s1}
	for k, v := range over {
		if v == nil {
			delete(it, k)
			continue
		}
		it[k] = v
	}
	return it
}

// icOrder is a request creating an order from the supplier supplied by
// the plant of the items.
func (e icEnv) icOrder(t *testing.T, supplier, plant any, items ...map[string]any) string {
	t.Helper()
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	return mustJSON(t, map[string]any{"legal_entity_id": e.DE, "supplier_id": supplier, "currency": "EUR", "order_date": e.day, "supplying_plant_id": plant,
		"items": list})
}

// released creates the intercompany order of one item and releases it.
func (e icEnv) released(t *testing.T) map[string]any {
	t.Helper()
	o, tag := e.Must(201, "POST", orders, e.icOrder(t, e.sister, e.p3, e.icItem(nil)), "")
	got, _ := e.Must(200, "POST", orders+"/"+o["id"].(string)+":release", "", tag)
	if got["status"] != "released" {
		t.Fatalf("the order: got %v, want released", got["status"])
	}
	return got
}

// issue posts the order's goods issue from S4 at its version.
func (e icEnv) issue(t *testing.T, o map[string]any, status int, items ...any) map[string]any {
	t.Helper()
	if items == nil {
		items = []any{map[string]any{"po_item_no": 1, "storage_location_id": e.s4}}
	}
	_, tag := e.Must(200, "GET", orders+"/"+o["id"].(string), "", "")
	got, _ := e.Must(status, "POST", orders+"/"+o["id"].(string)+":postIntercompanyGoodsIssue", mustJSON(t, map[string]any{"items": items}), tag)
	return got
}

// listed are the ids of the orders the query lists, joined by spaces.
func (e icEnv) listed(t *testing.T, query string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", orders+"?"+query, "", "")
	var ids []string
	for _, o := range rows(t, page, "items") {
		ids = append(ids, o["id"].(string))
	}
	return strings.Join(ids, " ")
}

// widgetStock is the widget's stock at the storage location, "" for none.
func (e icEnv) widgetStock(t *testing.T, location uuid.UUID) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/stock?material_id="+e.widget+"&storage_location_id="+location.String(), "", "")
	items := rows(t, page, "items")
	if len(items) == 0 {
		return ""
	}
	q := items[0]["quantity"].(map[string]any)
	return fmt.Sprintf("%v %v", q["value"], q["unit"])
}

// TestIntercompanyStockTransport: the order names its supplying plant and
// company; nothing is received before its goods issue, which posts a 643
// of company 1000 out of S4 against cost of goods sold, once; the goods
// are then in neither company's stock, and the order is neither
// cancelled nor closed; the receipt takes them into S1 at the order price
// against GR/IR, at most what was issued; the 643 is not reversed, nor
// posted by a person.
func TestIntercompanyStockTransport(t *testing.T) {
	e := setupIntercompany(t)
	o := e.released(t)
	po := uuid.MustParse(o["id"].(string))
	if got, want := fmt.Sprint(o["supplying_plant_id"] == e.p3.String(), " ", o["supplying_plant_code"], " ", o["supplying_plant_name"], " ",
		o["supplying_company_code"], " ", o["valuation"]), "true P3 Unit P3 1000 gr_ir"; got != want {
		t.Errorf("the order: got %s, want %s", got, want)
	}
	e.Problem(409, "procurement.intercompany_not_issued", "POST", "/v1/goods-receipts", e.receipt(t, po, "", 1, "10"), "")
	if got, want := e.listed(t, "supplying_plant_id="+e.p3.String()+"&issued=false"), po.String(); got != want {
		t.Errorf("orders of P3 to issue: got %s, want %s", got, want)
	}
	issued := e.issue(t, o, 200)
	for q, want := range map[string]string{"issued=false": "", "issued=true": po.String(), "supplying_plant_id=" + e.p1.String(): ""} {
		if got := e.listed(t, q); got != want {
			t.Errorf("orders %s after the issue: got %q, want %q", q, got, want)
		}
	}
	d, _ := e.Must(200, "GET", docs+"/"+issued["issue_document_id"].(string), "", "")
	if got, want := fmt.Sprint(d["movement_type"], " ", d["reference"] == o["document_number"], " ", d["legal_entity_id"] == e.US.String(), " ",
		d["number"] == issued["issue_document_number"], " ", e.lines(t, d)), "643 true true true P3:S4 -10 EA (10 EA)"; got != want {
		t.Errorf("the goods issue: got %s, want %s", got, want)
	}
	if got, want := e.entry(t, d), "WA 155000:-1500 585000:1500"; got != want {
		t.Errorf("its entry: got %s, want %s", got, want)
	}
	if got, want := e.widgetStock(t, e.s4)+"|"+e.widgetStock(t, e.s1), "10 EA|"; got != want {
		t.Errorf("stock at S4 and S1 in transit: got %s, want %s", got, want)
	}
	_, tag := e.Must(200, "GET", orders+"/"+po.String(), "", "")
	for _, action := range []string{"cancel", "close"} {
		e.Problem(409, "procurement.order_in_transit", "POST", orders+"/"+po.String()+":"+action, "", tag)
	}
	e.issueRefused(t, issued, 409, "procurement.intercompany_issued")
	_, docTag := e.Must(200, "GET", docs+"/"+d["id"].(string), "", "")
	e.Problem(409, "inventory.reverse_stock_transport", "POST", docs+"/"+d["id"].(string)+":reverse", "", docTag)
	e.Problem(422, "inventory.unknown_movement_type", "POST", docs, body(t, "643", e.item("1", "EA", nil)), "")
	r := e.receive(t, po, 1, "10")
	md, _ := e.Must(200, "GET", docs+"/"+r["material_document_id"].(string), "", "")
	if got, want := e.lines(t, md)+" "+e.valuation(t, e.widget, e.p1), "P1:S1 10 EA (10 EA) 10 EA 30.00 3.00/1"; got != want {
		t.Errorf("the receipt and the valuation: got %s, want %s", got, want)
	}
	e.Problem(409, "procurement.over_delivery", "POST", "/v1/goods-receipts", e.receipt(t, po, "", 1, "1"), "")
	// A return would take the goods out of both companies' stock: stock
	// items are not returned (S1e), so neither is an intercompany order's.
	e.Problem(422, "procurement.stock_item_return", "POST", "/v1/goods-receipts", e.receipt(t, po, "122", 1, "1"), "")
	_, tag = e.Must(200, "GET", orders+"/"+po.String(), "", "")
	closed, _ := e.Must(200, "POST", orders+"/"+po.String()+":close", "", tag)
	if closed["status"] != "closed" {
		t.Errorf("the received order closed: got %v", closed["status"])
	}
}

// issueRefused posts the goods issue of the order as it is now from S4,
// refused with the status and code.
func (e icEnv) issueRefused(t *testing.T, o map[string]any, status int, code string) {
	t.Helper()
	_, tag := e.Must(200, "GET", orders+"/"+o["id"].(string), "", "")
	e.Problem(status, code, "POST", orders+"/"+o["id"].(string)+":postIntercompanyGoodsIssue",
		mustJSON(t, map[string]any{"items": []any{map[string]any{"po_item_no": 1, "storage_location_id": e.s4}}}), tag)
}

// TestIntercompanyOrderRefused: an intercompany order needs an active
// plant of another company whose company is its supplier, and stock items
// without an over-delivery tolerance; a draft's new supplier is checked
// again; a released one is not changed; only a released intercompany
// order is issued, each item once.
func TestIntercompanyOrderRefused(t *testing.T) {
	e := setupIntercompany(t)
	for name, c := range map[string]struct {
		supplier, plant any
		item            map[string]any
		want            string
	}{
		"no such plant":         {e.sister, uuid.New(), nil, "procurement.supplying_plant_invalid"},
		"a storage location":    {e.sister, e.s4, nil, "procurement.supplying_plant_invalid"},
		"the company's own":     {e.sister, e.p2, nil, "procurement.supplying_plant_own_company"},
		"a third party":         {e.supplier, e.p3, nil, "procurement.intercompany_supplier"},
		"no stock item":         {e.sister, e.p3, map[string]any{"plant_id": nil, "storage_location_id": nil, "material_id": nil}, "procurement.intercompany_item_not_stock"},
		"over-delivery allowed": {e.sister, e.p3, map[string]any{"over_delivery_pct": "5"}, "procurement.intercompany_over_delivery"},
	} {
		t.Run(name, func(t *testing.T) {
			e.Problem(422, c.want, "POST", orders, e.icOrder(t, c.supplier, c.plant, e.icItem(c.item)), "")
		})
	}
	draft, tag := e.Must(201, "POST", orders, e.icOrder(t, e.sister, e.p3, e.icItem(nil)), "")
	id := draft["id"].(string)
	e.Problem(422, "procurement.intercompany_supplier", "PATCH", orders+"/"+id, mustJSON(t, map[string]any{"supplier_id": e.supplier}), tag)
	e.issueRefused(t, draft, 409, "procurement.invalid_status")
	o := e.released(t)
	e.Problem(409, "procurement.intercompany_order_change", "POST", orders+"/"+o["id"].(string)+"/changes", `{"reason": "More widgets"}`, "")
	for name, items := range map[string][]any{
		"no item":       {map[string]any{"po_item_no": 2, "storage_location_id": e.s4}},
		"an item twice": {map[string]any{"po_item_no": 1, "storage_location_id": e.s4}, map[string]any{"po_item_no": 1, "storage_location_id": e.s4}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := e.issue(t, o, 422, items...); got["code"] != "procurement.issue_items" {
				t.Errorf("got %v, want procurement.issue_items", got["code"])
			}
		})
	}
	t.Run("the supplying plant's orders to issue, page by page", func(t *testing.T) {
		q := orders + "?supplying_plant_id=" + e.p3.String() + "&issued=false&page_size=1"
		first, _ := e.Must(200, "GET", q, "", "")
		tok, _ := first["next_page_token"].(string)
		if n := len(rows(t, first, "items")); n != 1 || tok == "" {
			t.Fatalf("page 1: got %d orders and token %q, want 1 and a token", n, tok)
		}
		second, _ := e.Must(200, "GET", q+"&page_token="+tok, "", "")
		if n := len(rows(t, second, "items")); n != 1 {
			t.Errorf("page 2: got %d orders, want 1", n)
		}
	})
	plain := e.stockOrder(t, "PO-PLAIN", "EUR")
	e.issueRefused(t, map[string]any{"id": plain.String()}, 409, "procurement.not_intercompany")
}

// TestIntercompanyDatabaseFailures: every statement of creating, reading
// and issuing an intercompany order fails cleanly.
func TestIntercompanyDatabaseFailures(t *testing.T) {
	e := setupIntercompany(t)
	pages, err := pagination.NewCodec([]byte("pppppppppppppppppppppppppppppppp"))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Procurement(app.Config{Pages: pages})
	o := e.released(t)
	po := uuid.MustParse(o["id"].(string))
	widget := uuid.MustParse(e.widget)
	day, err := time.Parse(time.DateOnly, e.day)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreatePurchaseOrder(ctx, &gen.PurchaseOrderCreate{LegalEntityID: e.DE, SupplierID: e.sister, Currency: "EUR", OrderDate: day,
				SupplyingPlantID: gen.NewOptUUID(e.p3), Items: []gen.PurchaseOrderItem{{MaterialID: gen.NewOptUUID(widget), Description: "Widget",
					Quantity: gen.Quantity{Value: "1", Unit: "EA"}, UnitPrice: gen.Money{Amount: "3.00", Currency: "EUR"}, AccountID: e.account,
					PlantID: gen.NewOptUUID(e.p1), StorageLocationID: gen.NewOptUUID(e.s1)}}}, gen.CreatePurchaseOrderParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetPurchaseOrder(ctx, gen.GetPurchaseOrderParams{ID: po})
			return err
		},
		"goods issue": func(ctx context.Context) error {
			_, err := h.PostIntercompanyGoodsIssue(ctx, &gen.IntercompanyGoodsIssue{Items: []gen.IntercompanyGoodsIssueItemsItem{{PoItemNo: 1,
				StorageLocationID: e.s4}}}, gen.PostIntercompanyGoodsIssueParams{ID: po, IfMatch: `"2"`})
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
}
