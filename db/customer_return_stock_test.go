package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A customer's return received into stock (S2b4, migrations 000504 and
// 000505; ADR-0083 As built S2b4).

const (
	returnStockDocument = `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
		reversal_of_id, reference, created_by) VALUES ($1, $2, $3, 2026, $4, $5, '2026-11-27', $6, $7, $1)`
	returnStockReceipt = `INSERT INTO sales.return_receipt (tenant_id, id, returns_order_id, receipt_no, receipt_date, created_by)
		VALUES ($1, $2, $3, $4, '2026-03-22', $1)`
	returnStockReceiptItem = `INSERT INTO sales.return_receipt_item (tenant_id, receipt_id, returns_order_id, item_no, quantity) VALUES ($1, $2, $3, $4, 1)`
	returnStockPosting     = `INSERT INTO sales.return_receipt_posting (tenant_id, receipt_id, returns_order_id, material_document_id, material_document_number,
		currency, created_by) VALUES ($1, $2, $3, $4, $5, $6, $1)`
	returnStockCost = `INSERT INTO sales.return_receipt_item_cost (tenant_id, receipt_id, item_no, cost_amount) VALUES ($1, $2, $3, $4)`
)

// TestCustomerReturnDocuments: a customer's return into stock (653)
// names the returns order as its reference and reverses nothing; no
// other movement type is added.
func TestCustomerReturnDocuments(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name string
		run  func(s inventoryScene) []stmt
		want string // empty: allowed
	}{
		{"a return", func(s inventoryScene) []stmt {
			return []stmt{step(returnStockDocument, s.tenant, s.doc, s.company, "MD1", "653", nil, "RO1")}
		}, ""},
		{"a return without its reference", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "653", nil)} },
			"material_document_reference"},
		// The reversal trigger fires first: a 653 is no type's reversal.
		{"a return reversing a document", func(s inventoryScene) []stmt {
			return []stmt{step(returnStockDocument, s.tenant, s.doc, s.company, "MD1", "601", nil, "OD1"),
				step(returnStockDocument, s.tenant, id(), s.company, "MD2", "653", s.doc, "RO1")}
		}, "a reversal is of its original"},
		{"a 654", func(s inventoryScene) []stmt {
			return []stmt{step(returnStockDocument, s.tenant, s.doc, s.company, "MD1", "654", nil, "RO1")}
		}, "material_document_movement_type_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := inventoryScene{tenant: id(), company: id(), doc: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// returnStockScene is the returns scene's returns order of 4 of billing
// item 1 and 2 of billing item 2, confirmed, with a receipt of both
// items and a second receipt of item 1 alone.
type returnStockScene struct {
	returnsScene
	receipt, second, document uuid.UUID
}

func newReturnStockScene() returnStockScene {
	return returnStockScene{returnsScene: newReturnsScene(), receipt: id(), second: id(), document: id()}
}

// received are the scene's returns order, confirmed, and its receipts,
// followed by more.
func (s returnStockScene) received(more ...stmt) []stmt {
	return s.billed(append([]stmt{s.made(), s.line(s.returns, 1, 1, "4"), s.line(s.returns, 2, 2, "2"),
		step(`UPDATE sales.returns_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`, s.returns),
		step(returnStockReceipt, s.tenant, s.receipt, s.returns, 1), step(returnStockReceiptItem, s.tenant, s.receipt, s.returns, 1),
		step(returnStockReceiptItem, s.tenant, s.receipt, s.returns, 2),
		step(returnStockReceipt, s.tenant, s.second, s.returns, 2), step(returnStockReceiptItem, s.tenant, s.second, s.returns, 1)}, more...)...)
}

// posting is the receipt's posting by the scene's document.
func (s returnStockScene) posting() stmt {
	return step(returnStockPosting, s.tenant, s.receipt, s.returns, s.document, "MD1", "EUR")
}

// TestCustomerReturnPostingRules: a receipt's stock posting names its
// 653 by id and a number of 1 to 40 characters, and the currency, once
// per receipt and once per document; each of its items' cost is at least
// zero, once, of an item of the receipt that is posted; both are never
// changed or deleted, not even by the tables' owner.
func TestCustomerReturnPostingRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s returnStockScene) []stmt
		want string // empty: allowed
	}{
		{"posted with its items' costs", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, 8000), step(returnStockCost, s.tenant, s.receipt, 2, 0))
		}, ""},
		{"a number of 41 characters", func(s returnStockScene) []stmt {
			return s.received(step(returnStockPosting, s.tenant, s.receipt, s.returns, s.document, strings.Repeat("M", 41), "EUR"))
		}, "return_receipt_posting_material_document_number_check"},
		{"an empty number", func(s returnStockScene) []stmt {
			return s.received(step(returnStockPosting, s.tenant, s.receipt, s.returns, s.document, "", "EUR"))
		}, "return_receipt_posting_material_document_number_check"},
		{"a currency that is none", func(s returnStockScene) []stmt {
			return s.received(step(returnStockPosting, s.tenant, s.receipt, s.returns, s.document, "MD1", "XXY"))
		}, "return_receipt_posting_currency_fkey"},
		{"a receipt posted twice", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockPosting, s.tenant, s.receipt, s.returns, id(), "MD2", "EUR"))
		}, "return_receipt_posting_pkey"},
		{"one document for two receipts", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockPosting, s.tenant, s.second, s.returns, s.document, "MD1", "EUR"))
		}, "return_receipt_posting_document_once"},
		{"a posting of no receipt", func(s returnStockScene) []stmt {
			return s.received(step(returnStockPosting, s.tenant, id(), s.returns, s.document, "MD1", "EUR"))
		}, "return_receipt_posting_receipt_fkey"},
		{"a posting naming another returns order than its receipt's", func(s returnStockScene) []stmt {
			return s.received(step(returnStockPosting, s.tenant, s.receipt, s.otherDoc, s.document, "MD1", "EUR"))
		}, "return_receipt_posting_receipt_fkey"},
		{"a cost below zero", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, -1))
		}, "return_receipt_item_cost_check"},
		{"a cost of an item the receipt does not have", func(s returnStockScene) []stmt {
			return s.received(step(returnStockPosting, s.tenant, s.second, s.returns, s.document, "MD1", "EUR"), step(returnStockCost, s.tenant, s.second, 2, 1))
		}, "return_receipt_item_cost_item_fkey"},
		{"a cost of a receipt not posted", func(s returnStockScene) []stmt {
			return s.received(step(returnStockCost, s.tenant, s.receipt, 1, 1))
		}, "return_receipt_item_cost_posting_fkey"},
		{"a cost twice", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, 1), step(returnStockCost, s.tenant, s.receipt, 1, 2))
		}, "return_receipt_item_cost_pkey"},
		{"a posting changed", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(`UPDATE sales.return_receipt_posting SET material_document_number = 'MD2'`))
		}, "permission denied"},
		{"a posting deleted", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(`DELETE FROM sales.return_receipt_posting`))
		}, "permission denied"},
		{"a cost changed", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, 1), step(`UPDATE sales.return_receipt_item_cost SET cost_amount = 2`))
		}, "permission denied"},
		{"a cost deleted", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, 1), step(`DELETE FROM sales.return_receipt_item_cost`))
		}, "permission denied"},
		{"a posting changed by the tables' owner", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(`SET LOCAL ROLE erp_owner`), step(`UPDATE sales.return_receipt_posting SET material_document_number = 'MD2'`))
		}, "is insert-only"},
		{"a posting deleted by the tables' owner", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(`SET LOCAL ROLE erp_owner`), step(`DELETE FROM sales.return_receipt_posting`))
		}, "is insert-only"},
		{"a cost changed by the tables' owner", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, 1), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.return_receipt_item_cost SET cost_amount = 2`))
		}, "is insert-only"},
		{"a cost deleted by the tables' owner", func(s returnStockScene) []stmt {
			return s.received(s.posting(), step(returnStockCost, s.tenant, s.receipt, 1, 1), step(`SET LOCAL ROLE erp_owner`),
				step(`DELETE FROM sales.return_receipt_item_cost`))
		}, "is insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newReturnStockScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestCustomerReturnStockDownRefused: 000504's down migration refuses to
// drop a receipt's stock posting or a 653 (06 §6.4: a down migration
// loses no data), reading every tenant's rows.
func TestCustomerReturnStockDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000504_customer_return_stock").Down
	t.Run("a receipt's posting", func(t *testing.T) {
		s := newReturnStockScene()
		downRefused(t, down, s.tenant, s.received(s.posting()), "customer returns received into stock exist")
	})
	t.Run("a 653", func(t *testing.T) {
		s := inventoryScene{tenant: id(), company: id(), doc: id()}
		downRefused(t, down, s.tenant, []stmt{step(returnStockDocument, s.tenant, s.doc, s.company, "MD1", "653", nil, "RO1")},
			"customer returns received into stock exist")
	})
}
