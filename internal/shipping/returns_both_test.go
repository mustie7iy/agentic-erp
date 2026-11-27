package shipping_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Returns both ways (batch 98S integration): a return to the supplier
// (122, S1e2; ADR-0082 As built S1e2) and a customer's return into stock
// (653, S2b4; ADR-0083 As built S2b4) of one moving-priced material, each
// through the API, with the valuation (ADR-0082 §2), the availability
// check (S2j), the counts due (S2c2) and the ABC analysis (S2c3) reading
// what they leave.

// returnsEnv is env with GR/IR, a price difference account for the
// bracket's valuation class and a supplier, whose released order valued
// through GR/IR buys 100 EA of the bracket at 3.00 EUR into S1 of P1.
type returnsEnv struct {
	env
	po  uuid.UUID
	day string
}

func setupReturns(t *testing.T) returnsEnv {
	t.Helper()
	e := returnsEnv{env: setupWith(t, func(c *config.Config) {
		valued(c)
		de := c.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "191000", Name: "GR/IR clearing", Type: config.Liability},
			config.Account{Number: "590000", Name: "Price differences", Type: config.Expense})
		de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "191000", "590000"
		de.AccountDetermination.Inventory[0].PriceDifference = "231000" // class 3000, the bracket's
	}, "procurement.receipt.post", "procurement.order.create", "partner.supplier.read"), day: time.Now().UTC().Format(time.DateOnly)}
	supplier := e.NewSupplier(t, "S-1")
	e.po = uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-1', $3, $4, 'EUR', '2026-11-01', 'NT30', 'draft', $5, $5)`,
		e.Tenant.ID, e.po, e.DE, supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, material_id, description, quantity, uom_code,
		unit_price, price_unit, account_id, valuation_price, valuation_price_unit, plant_id, storage_location_id)
		VALUES ($1, $2, 1, $3, 'Bracket', 100, 'EA', 300, 1, $4, 300, 1, $5, $6)`,
		e.Tenant.ID, e.po, e.bracket, e.Account(t, "2000", "150000"), e.p1, e.s1)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, e.po)
	return e
}

// goodsMovement posts the order's goods movement of the movement type
// ("" a receipt) of qty EA of item 1: the movement.
func (e returnsEnv) goodsMovement(t *testing.T, movementType, qty string) map[string]any {
	t.Helper()
	b := map[string]any{"legal_entity_id": e.DE, "document_date": e.day, "posting_date": e.day,
		"items": []any{map[string]any{"purchase_order_id": e.po, "po_item_no": 1, "quantity": map[string]any{"value": qty, "unit": "EA"}}}}
	if movementType != "" {
		b["movement_type"], b["reason"] = movementType, "Scratched"
	}
	got, _ := e.Must(201, "POST", "/v1/goods-receipts", mustJSON(t, b), "")
	return got
}

// stockAccount is company 2000's stock account 150000 in the leading
// ledger, as "amount EUR".
func (e returnsEnv) stockAccount(t *testing.T) string {
	t.Helper()
	var cents int64
	pgtest.Scan(t, `SELECT coalesce(sum(l.amount_lc), 0)::bigint FROM ledger.journal_line l JOIN ledger.gl_account a ON a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.legal_entity_id = $2 AND a.number = '150000'
			AND l.ledger_id = (SELECT g.id FROM ledger.ledger g WHERE g.tenant_id = l.tenant_id AND g.is_leading)`, []any{e.Tenant.ID, e.DE}, &cents)
	return fmt.Sprintf("%d.%02d EUR", cents/100, cents%100)
}

// reconciles checks that the bracket's valuation holds the stock on hand
// at S1 and that its value is the stock account's balance, the only
// material the company keeps in stock.
func (e returnsEnv) reconciles(t *testing.T, stage, want string) {
	t.Helper()
	if got := e.valuationOf(t, e.bracket); got != want {
		t.Errorf("the valuation %s: got %s, want %s", stage, got, want)
	}
	page, _ := e.Must(200, "GET", "/v1/stock?material_id="+e.bracket, "", "")
	if got := fmt.Sprintf("%v %s", rows(t, page, "items")[0]["quantity"].(map[string]any)["value"], e.stockAccount(t)); got != want {
		t.Errorf("the stock and the stock account %s: got %s, want %s", stage, got, want)
	}
}

// TestReturnsBothWays: the bracket (200 EA worth 400.00 at a moving price)
// receives 10 EA at 3.00 (430.00); 4 go back to the supplier at the
// moving price, 8.19 of the 430.00, the 3.81 rest of GR/IR's 12.00 on
// price differences; 50 EA are delivered (102.38 of 421.81) and 20 of
// them come back at what their goods issue took, 40.95. After each the
// valuation's quantity is the stock and its value the stock account's;
// an order confirms the stock on hand after both; the location stays due
// for its count, which the stock it holds and its last count decide, not
// the movements; and the ABC analysis's consumption is the goods issue
// alone: neither return is consumption (ADR-0082 As built S2c3).
func TestReturnsBothWays(t *testing.T) {
	e := setupReturns(t)
	for _, code := range []string{"A", "B", "C"} {
		indicator, _ := e.Must(201, "POST", "/v1/cycle-count-indicators", mustJSON(t, map[string]any{"plant_id": e.p1, "code": code, "interval_days": 90}), "")
		if code == "A" {
			e.Must(201, "POST", "/v1/cycle-count-materials", mustJSON(t, map[string]any{"material_id": e.bracket, "indicator_id": indicator["id"]}), "")
		}
	}
	e.reconciles(t, "at the start", "200 400.00 EUR")
	e.goodsMovement(t, "", "10")
	e.reconciles(t, "after the receipt", "210 430.00 EUR")
	ret := e.goodsMovement(t, "122", "4")
	doc, _ := e.Must(200, "GET", "/v1/material-documents/"+ret["material_document_id"].(string), "", "")
	if l := rows(t, doc, "lines")[0]; money(l["amount"]) != "-8.19 EUR" || money(l["price_difference"]) != "-3.81 EUR" {
		t.Errorf("the 122's line: got %s, price difference %s; want -8.19 EUR, -3.81 EUR", money(l["amount"]), money(l["price_difference"]))
	}
	e.reconciles(t, "after the return to the supplier", "206 421.81 EUR")
	order := e.order(t, false, e.ofMaterial(e.bracket, "50 EA"))
	d, _ := e.shipped(t, order, "1 50 EA")
	if c := money(d["cost_of_goods_sold"]); c != "102.38 EUR" {
		t.Errorf("the goods issue's cost: got %s, want 102.38 EUR (50 of 206 EA worth 421.81)", c)
	}
	e.reconciles(t, "after the goods issue", "156 319.43 EUR")
	back, _ := e.returning(t, e.posted(t, order), "1 20 EA")
	got, _ := e.Must(201, "POST", returnsOrders+"/"+back+"/receipts", receipt(t, d["goods_issue_date"].(string), received("1 20 EA", at(e.s1))), "")
	if c := money(rows(t, got, "items")[0]["cost"]); c != "40.95 EUR" {
		t.Errorf("the 653's cost: got %s, want 40.95 EUR, 20 of the 50 EA the goods issue took 102.38 for", c)
	}
	e.reconciles(t, "after the customer's return", "176 360.38 EUR")
	if _, o := e.confirmedAt(t, e.p1, e.ofMaterial(e.bracket, "1000 EA")); confirmedOf(t, o) != "176 EA" {
		t.Errorf("an order of 1000 EA: got %s confirmed, want the 176 EA on hand after both returns", confirmedOf(t, o))
	}
	if got, want := e.countsDue(t), []string{"S1 true A never"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("the counts due after both returns: got %v, want %v", got, want)
	}
	from := time.Now().UTC().AddDate(0, 0, -29).Format(time.DateOnly)
	a, _ := e.Must(200, "GET", fmt.Sprintf("/v1/cycle-count-abc-analysis?plant_id=%s&from=%s&to=%s", e.p1, from, e.day), "", "")
	if l := rows(t, a, "lines"); len(l) != 1 || l[0]["consumption"].(map[string]any)["amount"] != "102.38" {
		t.Errorf("the ABC analysis: got %v, want the bracket's consumption 102.38, its goods issue's alone", l)
	}
}

// TestReturnedSerialIssuedAgain (S2b4 with S2g2): a serial number a
// customer returned into stock is in stock again and is issued again by
// the next delivery, which takes it out of stock.
func TestReturnedSerialIssuedAgain(t *testing.T) {
	e := setupTracked(t)
	first := e.order(t, false, e.ofMaterial(e.gauge, "1 EA"))
	id, tag := e.picked(t, first, "1 1 EA")
	d, _ := e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 1 EA", "", "G1")), tag)
	ret, _ := e.returning(t, e.posted(t, first), "1 1 EA")
	e.Must(201, "POST", returnsOrders+"/"+ret+"/receipts", receipt(t, d["goods_issue_date"].(string),
		received("1 1 EA", map[string]any{"storage_location_id": e.s1, "serial_numbers": []string{"G1"}})), "")
	if got := e.inStock(t, e.gauge); got != "G1 G2 G3" {
		t.Fatalf("the gauges in stock after the return: got %s, want G1 G2 G3", got)
	}
	second := e.order(t, false, e.ofMaterial(e.gauge, "1 EA"))
	id, tag = e.picked(t, second, "1 1 EA")
	e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 1 EA", "", "G1")), tag)
	if got := e.inStock(t, e.gauge); got != "G2 G3" {
		t.Errorf("the gauges in stock after G1 is issued again: got %s, want G2 G3", got)
	}
}
