package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// orderItem is a stock order item of the request body: 10 EA of the
// bracket at 2.50 into S1 of P1, with fields replaced, added or (nil)
// taken out by over.
func (e orderEnv) orderItem(over map[string]any) map[string]any {
	it := map[string]any{"material_id": e.bracket, "description": "Bracket", "quantity": map[string]any{"value": "10", "unit": "EA"},
		"unit_price": amount("2.50"), "account_id": e.expense, "plant_id": e.p1, "storage_location_id": e.s1}
	for k, v := range over {
		if v == nil {
			delete(it, k)
			continue
		}
		it[k] = v
	}
	return it
}

// order is a purchase order request in currency with the item.
func (e orderEnv) order(t *testing.T, currency string, item map[string]any) string {
	t.Helper()
	return mustJSON(t, map[string]any{"legal_entity_id": e.DE, "supplier_id": e.supplier, "currency": currency, "order_date": "2026-11-01",
		"items": []any{item}})
}

// TestStockItemOrders (S1e): an order item names its plant and storage
// location together with its material, on no cost center, in the
// company's currency, of an active material kept there in a unit that
// converts its quantity exactly; a released order's item keeps its stock
// location, which a change that sends none keeps.
func TestStockItemOrders(t *testing.T) {
	e := setupOrders(t)
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", e.order(t, "EUR", e.orderItem(nil)), "")
	it := rows(t, o, "items")[0]
	if got, want := fmt.Sprintf("%v %v %v/%v %v/%v", it["plant_id"], it["storage_location_id"], it["plant_code"], it["plant_name"], it["storage_location_code"],
		it["storage_location_name"]), fmt.Sprintf("%v %v P1/Unit P1 S1/Unit S1", e.p1, e.s1); got != want {
		t.Errorf("stock item: got %s, want %s", got, want)
	}
	pending := e.material(t, "Washer", "trading_good", false)
	for _, c := range []struct {
		name, currency string
		over           map[string]any
		code, field    string
	}{
		{"a plant without a storage location", "EUR", map[string]any{"storage_location_id": nil}, "procurement.invalid_stock_item", "items[0].storage_location_id"},
		{"no material", "EUR", map[string]any{"material_id": nil}, "procurement.invalid_stock_item", "items[0].material_id"},
		{"on a cost center", "EUR", map[string]any{"cost_center_id": e.c1}, "procurement.invalid_stock_item", "items[0].account_assignment"},
		{"in another currency", "USD", map[string]any{"unit_price": map[string]any{"amount": "2.50", "currency": "USD"}}, "procurement.invalid_stock_item",
			"items[0].storage_location_id"},
		{"a plant of another company", "EUR", map[string]any{"plant_id": e.p3, "storage_location_id": e.s4}, "procurement.invalid_org_unit", "items[0].plant_id"},
		{"an unknown material", "EUR", map[string]any{"material_id": uuid.New()}, "procurement.invalid_stock_item", "items[0].material_id"},
		{"a pending material", "EUR", map[string]any{"material_id": pending}, "procurement.invalid_stock_item", "items[0].material_id"},
		{"a storage location it is not kept at", "EUR", map[string]any{"storage_location_id": e.s3}, "procurement.invalid_stock_item",
			"items[0].storage_location_id"},
		{"a unit it does not have", "EUR", map[string]any{"quantity": map[string]any{"value": "1", "unit": "L"}}, "procurement.invalid_stock_item",
			"items[0].quantity.unit"},
		{"a quantity not exact in the base unit", "EUR", map[string]any{"quantity": map[string]any{"value": "1", "unit": "KG"}}, "procurement.invalid_stock_item",
			"items[0].quantity.unit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.Problem(422, c.code, "POST", "/v1/purchase-orders", e.order(t, c.currency, e.orderItem(c.over)), "")
			if field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
	t.Run("an order change", func(t *testing.T) {
		po := e.stockOrder(t, "PO-9", "EUR")
		items := func(location any) string {
			first := e.orderItem(map[string]any{"quantity": map[string]any{"value": "100", "unit": "EA"}, "plant_id": nil, "storage_location_id": location})
			if location != nil {
				first["plant_id"] = e.p1
			}
			bolt := e.orderItem(map[string]any{"material_id": e.bolt, "description": "Bolt", "unit_price": amount("1.20")})
			paper := e.orderItem(map[string]any{"material_id": nil, "description": "Paper", "quantity": map[string]any{"value": "5", "unit": "EA"},
				"unit_price": amount("2.00"), "plant_id": nil, "storage_location_id": nil})
			nut := e.orderItem(map[string]any{"material_id": e.nut, "description": "Nut", "unit_price": amount("0.10")})
			return mustJSON(t, map[string]any{"reason": "More brackets", "items": []any{first, bolt, paper, nut}})
		}
		e.Problem(422, "procurement.invalid_change", "POST", "/v1/purchase-orders/"+po.String()+"/changes", items(e.s2), "")
		ch, _ := e.Must(201, "POST", "/v1/purchase-orders/"+po.String()+"/changes", items(nil), "")
		if got := rows(t, ch, "items")[0]["storage_location_code"]; got != "S1" {
			t.Errorf("the change's stock item: got storage location %v, want S1", got)
		}
		page, _ := e.Must(200, "GET", "/v1/purchase-orders/"+po.String()+"/changes", "", "")
		if got := rows(t, rows(t, page, "items")[0], "items")[0]["plant_code"]; got != "P1" {
			t.Errorf("the listed change's stock item: got plant %v, want P1", got)
		}
		_, got, _ := e.Do("GET", "/v1/purchase-orders/"+po.String()+"/changes/"+ch["id"].(string), "", "")
		if code := rows(t, got, "items")[0]["plant_code"]; code != "P1" {
			t.Errorf("the change's stock item read: got plant %v, want P1", code)
		}
	})
	t.Run("a change after the material is blocked", func(t *testing.T) {
		// An item ordered while its material was active is changed after it
		// is blocked: its open quantity can still be brought down.
		po := e.stockOrder(t, "PO-10", "EUR")
		_, mtag := e.Must(200, "GET", "/v1/materials/"+e.nut, "", "")
		e.Must(200, "PATCH", "/v1/materials/"+e.nut, `{"status": "blocked"}`, mtag)
		change := func(nuts map[string]any) string {
			return mustJSON(t, map[string]any{"reason": "Fewer nuts", "items": []any{
				e.orderItem(map[string]any{"quantity": map[string]any{"value": "100", "unit": "EA"}}),
				e.orderItem(map[string]any{"material_id": e.bolt, "description": "Bolt", "unit_price": amount("1.20")}),
				e.orderItem(map[string]any{"material_id": nil, "description": "Paper", "quantity": map[string]any{"value": "5", "unit": "EA"},
					"unit_price": amount("2.00"), "plant_id": nil, "storage_location_id": nil}),
				e.orderItem(map[string]any{"material_id": e.nut, "description": "Nut", "unit_price": amount("0.10"), "quantity": nuts})}})
		}
		e.Must(201, "POST", "/v1/purchase-orders/"+po.String()+"/changes", change(map[string]any{"value": "5", "unit": "EA"}), "")
		// A kept item's quantity still converts exactly.
		e.Problem(422, "procurement.invalid_stock_item", "POST", "/v1/purchase-orders/"+po.String()+"/changes", change(map[string]any{"value": "1", "unit": "KG"}), "")
	})
}

// TestStockReceiptEdges (S1e): a stock item received free of charge, and
// at a standard price equal to the order price, posts its value to the
// stock alone; a receipt's reversal is refused once the stock is gone, a
// receipt of stock items mixing currencies with another order's items is
// refused, and a stock item's receipt without its document cannot be
// reversed.
func TestStockReceiptEdges(t *testing.T) {
	e := setupOrders(t)
	po := e.stockOrderAt(t, "PO-1", "EUR", 0, 100)
	gr := e.receive(t, po, 1, "10", 2, "10")
	if got, want := strings.Join(values(t, e.document(t, gr)), "; "), e.materialNumber(t, e.bracket)+" 10 0.00 0.00; "+e.materialNumber(t, e.bolt)+" 10 10.00 0.00"; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
	if got := e.balance(t, "517000"); got != 0 {
		t.Errorf("price differences: got %d, want 0", got)
	}
	e.post(t, "201", e.item("10", "EA", map[string]any{"cost_center_id": e.c1}))
	e.Problem(409, "inventory.insufficient_stock", "POST", "/v1/goods-receipts/"+gr["id"].(string)+":reverse", "", e.receiptTag(t, gr))
	usd := e.stockOrder(t, "PO-2", "USD")
	e.Problem(422, "procurement.mixed_currencies", "POST", "/v1/goods-receipts", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "document_date": e.day,
		"posting_date": e.day, "items": []any{
			map[string]any{"purchase_order_id": po, "po_item_no": 1, "quantity": map[string]any{"value": "1", "unit": "EA"}},
			map[string]any{"purchase_order_id": usd, "po_item_no": 3, "quantity": map[string]any{"value": "1", "unit": "EA"}}}}), "")
	// A receipt written without its document, as no goods receipt leaves
	// one, is refused rather than reversed half.
	e.Receive(t, po, 2, "1", "GR-X")
	var bare string
	pgtest.Scan(t, `SELECT id::text FROM procurement.goods_movement WHERE tenant_id = $1 AND document_number = 'GR-X'`, []any{e.Tenant.ID}, &bare)
	if code, got, _ := e.Do("POST", "/v1/goods-receipts/"+bare+":reverse", "", e.receiptTag(t, map[string]any{"id": bare})); code != 500 {
		t.Errorf("reversal without its document: got %d %v, want 500", code, got)
	}
}

// stub is a PostEntry answering the bookings' amounts, or what given says.
type stub struct {
	bookings []ledger.GRIRStock
	posted   []ledger.StockPosted
	err      error
}

func (s *stub) post(b []ledger.GRIRStock) (ledger.Entry, []ledger.StockPosted, error) {
	s.bookings = b
	if s.err != nil {
		return ledger.Entry{}, nil, s.err
	}
	return ledger.Entry{ID: uuid.New(), FiscalYear: 2026, DocumentNumber: "WE1"}, s.posted, nil
}

// TestOrderPorts (S1e): the published API refuses a receipt of another
// company's goods, a reversal of what is no goods receipt and an
// invoice's difference without a valuation or account, an unknown unit
// or a value below zero, and passes the entry's refusal on; a moving
// receipt valued beyond an amount's range is refused; outside a request
// each is refused.
func TestOrderPorts(t *testing.T) {
	e := setupOrders(t)
	h := endpoints(t)
	ctx := context.Background()
	in := inventory.OrderReceipt{Company: e.DE, Day: time.Now(), Reference: "GR1", Items: []inventory.OrderItem{{MaterialID: uuid.MustParse(e.bracket),
		PlantID: e.p1, StorageLocationID: e.s1, Quantity: "1", Unit: "EA"}}}
	items := []inventory.InvoiceItem{{MaterialID: uuid.MustParse(e.bracket), PlantID: e.p1, Quantity: "1", Unit: "EA"}}
	src := inventory.InvoiceSource{InvoiceID: uuid.New(), PurchaseOrderID: uuid.New(), POItemNos: []int32{1}}
	refused := errors.New("refused")
	s := &stub{posted: []ledger.StockPosted{{}}}
	if _, err := h.ReceiveForOrder(ctx, in, s.post); err == nil {
		t.Error("receive outside a request: got nil, want an error")
	}
	if _, err := h.ReverseForOrder(ctx, uuid.New(), time.Now(), "GR2", s.post); err == nil {
		t.Error("reverse outside a request: got nil, want an error")
	}
	if err := h.InvoiceDifferences(ctx, e.DE, items, src, s.post); err == nil {
		t.Error("invoice outside a request: got nil, want an error")
	}
	if _, err := h.RevaluedStock(ctx, uuid.New()); err == nil {
		t.Error("revalued outside a request: got nil, want an error")
	}
	for name, c := range map[string]struct {
		run  func(ctx context.Context) error
		want string
	}{
		"another company's receipt": {func(ctx context.Context) error {
			other := in
			other.Company = e.US
			_, err := h.ReceiveForOrder(ctx, other, s.post)
			return err
		}, "inventory.mixed_companies"},
		"an unknown material": {func(ctx context.Context) error {
			other := in
			other.Items = []inventory.OrderItem{{MaterialID: uuid.New(), PlantID: e.p1, StorageLocationID: e.s1, Quantity: "1", Unit: "EA"}}
			_, err := h.ReceiveForOrder(ctx, other, s.post)
			return err
		}, "inventory.material_invalid"},
		"the entry refused": {func(ctx context.Context) error {
			_, err := h.ReceiveForOrder(ctx, in, (&stub{err: refused}).post)
			return err
		}, "refused"},
		"a value beyond an amount": {func(ctx context.Context) error {
			// The first fills the stock's value to the range's end, the second goes past it.
			_, err := h.ReceiveForOrder(ctx, in, (&stub{posted: []ledger.StockPosted{{Stock: money.Minor(math.MaxInt64)}}}).post)
			if err == nil {
				_, err = h.ReceiveForOrder(ctx, in, (&stub{posted: []ledger.StockPosted{{Stock: 1}}}).post)
			}
			return err
		}, "inventory.amount_too_large"},
		"a standard value beyond an amount": {func(ctx context.Context) error {
			pin := e.material(t, "Dowel pin", "trading_good", true)
			e.value(t, pin, e.p1, "standard", "1.00")
			// A price no request gives: the stock of one is out of range at it.
			pgtest.Exec(t, `UPDATE inventory.valuation SET price = 9000000000000000000, version = version + 1 WHERE material_id = $1`, pin)
			other := in
			other.Items = []inventory.OrderItem{{MaterialID: uuid.MustParse(pin), PlantID: e.p1, StorageLocationID: e.s1, Quantity: "2", Unit: "EA"}}
			_, err := h.ReceiveForOrder(ctx, other, s.post)
			return err
		}, "inventory.amount_too_large"},
		"no goods receipt": {func(ctx context.Context) error {
			d := e.post(t, "561", e.item("1", "EA", nil))
			_, err := h.ReverseForOrder(ctx, uuid.MustParse(d["id"].(string)), time.Now(), "GR2", s.post)
			return err
		}, "no goods receipt to reverse"},
		"an unknown document": {func(ctx context.Context) error {
			_, err := h.ReverseForOrder(ctx, uuid.New(), time.Now(), "GR2", s.post)
			return err
		}, "not_found"},
		"an invoice of no valuation": {func(ctx context.Context) error {
			return h.InvoiceDifferences(ctx, e.DE, []inventory.InvoiceItem{{MaterialID: uuid.MustParse(e.nut), PlantID: e.p1, Quantity: "1", Unit: "EA"}}, src,
				s.post)
		}, "inventory.no_valuation"},
		"an invoice in an unknown unit": {func(ctx context.Context) error {
			e.post(t, "501", e.item("1", "EA", map[string]any{"amount": amount("2.00")}))
			return h.InvoiceDifferences(ctx, e.DE, []inventory.InvoiceItem{{MaterialID: uuid.MustParse(e.bracket), PlantID: e.p1, Quantity: "1", Unit: "L"}},
				src, s.post)
		}, "not a unit of the material"},
		"an invoice's entry refused": {func(ctx context.Context) error {
			return h.InvoiceDifferences(ctx, e.DE, items, src, (&stub{err: refused}).post)
		}, "refused"},
		"a value below zero": {func(ctx context.Context) error {
			e.post(t, "501", e.item("1", "EA", map[string]any{"amount": amount("2.00")}))
			return h.InvoiceDifferences(ctx, e.DE, items, src, (&stub{posted: []ledger.StockPosted{{Stock: -1_000_000}}}).post)
		}, "inventory.stock_value_below_zero"},
		"a class without a price difference account": {func(ctx context.Context) error {
			washer := e.material(t, "Spring washer", "trading_good", true)
			e.Must(201, "POST", "/v1/material-valuations", mustJSON(t, map[string]any{"material_id": washer, "plant_id": e.p1, "valuation_class": "3100",
				"price_control": "moving_average", "price": amount("1.00")}), "")
			return h.InvoiceDifferences(ctx, e.DE, []inventory.InvoiceItem{{MaterialID: uuid.MustParse(washer), PlantID: e.p1, Quantity: "1", Unit: "EA"}},
				src, s.post)
		}, "inventory.no_inventory_account"},
	} {
		t.Run(name, func(t *testing.T) {
			err := e.inRequest(t, c.run)
			if err == nil || !strings.Contains(problem.From(err).Code+" "+err.Error(), c.want) {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
	t.Run("no quantity is covered by none", func(t *testing.T) {
		e.post(t, "501", e.item("1", "EA", map[string]any{"amount": amount("2.00")}))
		zero := &stub{posted: []ledger.StockPosted{{}}}
		if err := e.inRequest(t, func(ctx context.Context) error {
			return h.InvoiceDifferences(ctx, e.DE, []inventory.InvoiceItem{{MaterialID: uuid.MustParse(e.bracket), PlantID: e.p1, Quantity: "0", Unit: "EA"}},
				src, zero.post)
		}); err != nil {
			t.Fatal(err)
		}
		if got := zero.bookings[0].Share.Sign(); got != 0 {
			t.Errorf("share: got sign %d, want 0", got)
		}
	})
}

// TestStockItemsForeignCurrency (S1e): the ledger posts a stock item's
// counter line in the company's currency alone.
func TestStockItemsForeignCurrency(t *testing.T) {
	e := setupOrders(t)
	stock := e.Account(t, "2000", "150000")
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, Settings: configuration.Settings{}}
	err := e.inRequest(t, func(ctx context.Context) error {
		day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		_, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "WE", DocumentDate: day, PostingDate: day, Currency: "USD",
			SourceContext: "procurement", SourceDocumentID: uuid.New(), GRIR: []ledger.GRIRItem{{PurchaseOrderID: uuid.New(), POItemNo: 1, Quantity: "-1",
				Unit: "EA", UnitPrice: 100, PriceUnit: 1, OffsetAccountID: &stock, Stock: &ledger.GRIRStock{AccountID: stock, DifferenceAccountID: stock}}}})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "stock items post in") {
		t.Errorf("got %v, want the company's currency refused", err)
	}
}

// TestStockReceiptDatabaseFailures (S1e): every statement of a stock
// item's order, receipt, reversal, invoice and invoice reversal fails
// with the request.
func TestStockReceiptDatabaseFailures(t *testing.T) {
	e := setupOrders(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{Pages: pages, Now: time.Now}
	proc, pay := app.Procurement(cfg), app.Payables(cfg)
	po := e.stockOrder(t, "PO-1", "EUR")
	gr := e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "10")
	code, inv := e.invoice(t, "", nil, po, "12.00", invoiced{2, "10", "12.00"})
	if code != 200 {
		t.Fatalf("invoice of the bolts: got %d %v", code, inv)
	}
	parked := e.parked(t, po, "25.50", invoiced{1, "10", "25.50"})
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for name, call := range map[string]func(ctx context.Context) error{
		"order a stock item": func(ctx context.Context) error {
			_, err := proc.CreatePurchaseOrder(ctx, &gen.PurchaseOrderCreate{LegalEntityID: e.DE, SupplierID: e.supplier, Currency: "EUR", OrderDate: day,
				Items: []gen.PurchaseOrderItem{{MaterialID: gen.NewOptUUID(uuid.MustParse(e.bracket)), Description: "Bracket", Quantity: gen.Quantity{Value: "1", Unit: "EA"},
					UnitPrice: gen.Money{Amount: "2.50", Currency: "EUR"}, AccountID: e.expense, PlantID: gen.NewOptUUID(e.p1),
					StorageLocationID: gen.NewOptUUID(e.s1)}}}, gen.CreatePurchaseOrderParams{})
			return err
		},
		"receive into stock": func(ctx context.Context) error {
			_, err := proc.PostGoodsReceipt(ctx, &gen.GoodsReceiptCreate{LegalEntityID: e.DE, DocumentDate: day, PostingDate: day,
				Items: []gen.GoodsReceiptItemInput{{PurchaseOrderID: po, PoItemNo: 1, Quantity: gen.Quantity{Value: "1", Unit: "EA"}}}}, gen.PostGoodsReceiptParams{})
			return err
		},
		"reverse a receipt into stock": func(ctx context.Context) error {
			_, err := proc.ReverseGoodsReceipt(ctx, gen.OptGoodsReceiptReverse{}, gen.ReverseGoodsReceiptParams{ID: uuid.MustParse(gr["id"].(string)), IfMatch: "*"})
			return err
		},
		"post an invoice of stock": func(ctx context.Context) error {
			_, err := pay.PostSupplierInvoice(ctx, gen.OptSupplierInvoicePost{}, gen.PostSupplierInvoiceParams{ID: uuid.MustParse(parked), IfMatch: "*"})
			return err
		},
		"reverse an invoice of stock": func(ctx context.Context) error {
			_, err := pay.ReverseSupplierInvoice(ctx, &gen.SupplierInvoiceReverse{Reason: "wrong"},
				gen.ReverseSupplierInvoiceParams{ID: uuid.MustParse(inv["id"].(string)), IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call) })
	}
}

// parked enters and matches an invoice clean, leaving it parked, and
// returns its id.
func (e orderEnv) parked(t *testing.T, po uuid.UUID, gross string, items ...invoiced) string {
	t.Helper()
	*e.receiptSeq++
	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "supplier_id": e.supplier,
		"supplier_reference": fmt.Sprintf("PARKED-%d", *e.receiptSeq), "invoice_date": e.day, "posting_date": e.day, "gross_amount": amount(gross), "tax_amount": amount("0.00"), "items": invoiceItems(po, items)}), "")
	if m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+inv["id"].(string)+":match", "", tag); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	return inv["id"].(string)
}

// TestStockReceiptsAtOnce (S1e; ADR-0082 As built S1e, locks): goods
// receipts of the bracket, issues of it to a cost center and receipts of
// it without an order post at once, none waiting on another for good:
// each takes the valuation, then the stock, then the ranges and the
// ledger's locks; invoices of another order's received brackets post
// beside them, taking the order items, then the valuation.
func TestStockReceiptsAtOnce(t *testing.T) {
	e := setupOrders(t)
	po, billed := e.stockOrder(t, "PO-1", "EUR"), e.stockOrder(t, "PO-2", "EUR")
	e.receive(t, billed, 1, "40")
	e.post(t, "501", e.item("40", "EA", map[string]any{"amount": amount("100.00")}))
	invoices := make([]string, 4)
	for i := range invoices {
		invoices[i] = e.parked(t, billed, "25.00", invoiced{1, "10", "25.00"})
	}
	var wg sync.WaitGroup
	statuses := make([]int, 16)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				statuses[i], _, _ = e.Do("POST", "/v1/goods-receipts", e.receipt(t, po, "", 1, "1"), "")
			case 1:
				statuses[i], _, _ = e.Do("POST", docs, body(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c1})), "")
			case 2:
				statuses[i], _, _ = e.Do("POST", docs, body(t, "501", e.item("1", "EA", map[string]any{"amount": amount("2.50")})), "")
			default:
				statuses[i], _, _ = e.Do("POST", "/v1/supplier-invoices/"+invoices[i/4]+":post", mustJSON(t, map[string]any{"posting_date": e.day}), "*")
			}
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if want := map[bool]int{true: 200, false: 201}[i%4 == 3]; s != want {
			t.Errorf("posting %d: got %d, want %d", i, s, want)
		}
	}
	if got, want := e.stock(t, e.s1, ""), "84 EA"; got != want {
		t.Errorf("stock: got %s, want %s", got, want)
	}
	if got, want := e.balance(t, "150000"), e.valuationValue(t); got != want {
		t.Errorf("stock account: got %d, want the valuation's %d", got, want)
	}
}

// valuationValue is the bracket's valuation's value in P1, in minor units.
func (e orderEnv) valuationValue(t *testing.T) int64 {
	t.Helper()
	var v int64
	pgtest.Scan(t, `SELECT value FROM inventory.valuation WHERE tenant_id = $1 AND material_id = $2 AND plant_id = $3`, []any{e.Tenant.ID, e.bracket, e.p1}, &v)
	return v
}
