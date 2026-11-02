package fixture

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// InvoicePermissions are the permissions of the people who work supplier
// invoices: their payables permissions, the agents' recommendations, the
// orders and receipts invoices are matched against, and the accounts and
// centers an invoice's lines name (T73e3).
var InvoicePermissions = []string{"payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "payables.invoice.hold",
	"payables.invoice.review", "agents.session.read", "agents.recommendation.decide", "procurement.order.read", "procurement.receipt.read",
	"ledger.entry.read", "ledger.account.read", "ledger.cost_center.read"}

// Order inserts a released purchase order of the supplier in company 2000
// (EUR): item 1 is 10 EA of paper at 12.00 with tax code V1, item 2 is
// 5 KG of toner at 3.00 without one, both on expense account 520000.
func (e Env) Order(tb testing.TB, supplier uuid.UUID, number string) uuid.UUID {
	tb.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-03-01', 'NT30', 'draft', $6, $6)`,
		e.Tenant.ID, id, number, e.DE, supplier, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit)
		VALUES ($1, $2, 1, 'Paper', 10, 'EA', 1200, 1, $3, 'V1', 1200, 1), ($1, $2, 2, 'Toner', 5, 'KG', 300, 1, $3, NULL, 300, 1)`,
		e.Tenant.ID, id, e.Account(tb, "2000", "520000"))
	pgtest.Exec(tb, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, id)
	return id
}

// OrderChange drafts and submits, as the tenant's human, a change of a
// new released order of the supplier numbered number (Order) that raises
// its paper to 100 EA, EUR 1,215.00 in all, above the first order tier,
// and returns its pending order_change approval request. The tenant needs
// procurement.order.create and .read.
func (e Env) OrderChange(tb testing.TB, supplier uuid.UUID, number string) uuid.UUID {
	tb.Helper()
	path := "/v1/purchase-orders/" + e.Order(tb, supplier, number).String()
	account := e.Account(tb, "2000", "520000")
	ch, tag := e.Must(201, "POST", path+"/changes", fmt.Sprintf(`{"reason": "Paper for the whole year", "items": [
		{"description": "Paper", "quantity": {"value": "100", "unit": "EA"}, "unit_price": {"amount": "12.00", "currency": "EUR"}, "account_id": %q, "tax_code": "V1"},
		{"description": "Toner", "quantity": {"value": "5", "unit": "KG"}, "unit_price": {"amount": "3.00", "currency": "EUR"}, "account_id": %q}]}`,
		account, account), "")
	e.Must(200, "POST", path+"/changes/"+ch["id"].(string)+":submit", "", tag)
	return uuid.MustParse(e.Pending(tb, ch["id"].(string))["id"].(string))
}

// Receive posts a goods receipt of the quantity on an order item, numbered
// number.
func (e Env) Receive(tb testing.TB, po uuid.UUID, item int, qty, number string) {
	tb.Helper()
	gm := uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date, posted_by)
		VALUES ($1, $2, $3, $4, '101', '2026-03-05', '2026-03-05', $5)`, e.Tenant.ID, gm, number, e.DE, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code)
		SELECT $1, $2, 1, $3, $4, $5::numeric, uom_code FROM procurement.purchase_order_item WHERE purchase_order_id = $3 AND item_no = $4`,
		e.Tenant.ID, gm, po, item, qty)
}

// POItem is an invoice item on an order item, in EUR.
func POItem(po uuid.UUID, item int, qty, unit, amount string) string {
	return fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": %d, "quantity": {"value": %q, "unit": %q}, "amount": {"amount": %q, "currency": "EUR"}}`,
		po, item, qty, unit, amount)
}

// AccountItem is an invoice item on expense account 520000 with tax code
// V1, in EUR.
func (e Env) AccountItem(tb testing.TB, description, amount string) string {
	tb.Helper()
	return fmt.Sprintf(`{"account_id": %q, "description": %q, "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}, "tax_code": "V1"}`,
		e.Account(tb, "2000", "520000"), description, amount)
}

// SupplierInvoice enters, as the tenant's human, a parked invoice of the
// supplier in company 2000 (EUR) dated 2026-03-10, with the items (on an
// order: POItem; on an account: AccountItem), and returns its id.
func (e Env) SupplierInvoice(tb testing.TB, supplier uuid.UUID, ref, gross, tax string, items ...string) uuid.UUID {
	tb.Helper()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": "2026-03-10", "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": %q, "currency": "EUR"}, "items": [%s]}`,
		e.DE, supplier, ref, gross, tax, strings.Join(items, ", ")), "")
	return uuid.MustParse(inv["id"].(string))
}

// Recommend records, as the tenant's agent in a session of its own, a
// recommendation of kind on the supplier invoice, as the AP exceptions
// agent leaves one (internal/agent), and returns its id.
func (e Env) Recommend(tb testing.TB, invoice uuid.UUID, kind, content string) uuid.UUID {
	tb.Helper()
	session, id := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	pgtest.Exec(tb, `INSERT INTO agents.session (tenant_id, id, agent_id, purpose, model, status, cost_currency) VALUES ($1, $2, $3, 'ap_exceptions', 'test', 'completed', 'USD')`,
		e.Tenant.ID, session, e.Tenant.Agent.ID)
	pgtest.Exec(tb, `INSERT INTO agents.recommendation (tenant_id, id, session_id, object_type, object_id, kind, content) VALUES ($1, $2, $3, 'supplier_invoice', $4, $5, $6)`,
		e.Tenant.ID, id, session, invoice, kind, content)
	return id
}

// Captured makes the invoice one an agent captured from document: its
// source capture, entered by the tenant's agent, with the document's hash
// and object key, as :capture leaves it. The test stores the bytes under
// the key in the API's object store.
func (e Env) Captured(tb testing.TB, invoice uuid.UUID, sha256 []byte, key string) {
	tb.Helper()
	pgtest.Exec(tb, `UPDATE payables.supplier_invoice SET source = 'capture', capture_sha256 = $3, capture_object_ref = $4, created_by = $5, version = version + 1
		WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, invoice, sha256, key, e.Tenant.Agent.ID)
}
