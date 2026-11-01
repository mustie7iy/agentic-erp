package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// receiving is the payables environment with goods receipts and order
// changes posted through the API.
func receiving(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read", "procurement.order.read", "procurement.order.create")...)
	return env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
}

// procurementEndpoints are procurement's endpoints as the API wires them.
func procurementEndpoints() *procurement.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &procurement.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{}, Accounts: ledger.Accounts{},
		Numbers: configuration.Numbers{}, Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}}, Settings: configuration.Settings{},
		Schemas: configuration.Settings{}, Rules: configuration.Settings{}, References: partner.References{}, Approvals: fixture.Approvals(),
		Invoices: payables.Invoices{}, Postings: ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}}
}

// receipt posts a goods receipt of the quantity on item 1 of the order
// through the API and returns its id.
func (e env) receipt(t *testing.T, po uuid.UUID, qty string) string {
	t.Helper()
	gr, _ := e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": %q, "unit": "EA"}}]}`, e.DE, po, qty), "")
	return id(gr)
}

// reverseReceipt reverses a goods receipt in the request's transaction,
// as :reverse does.
func reverseReceipt(gr string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := procurementEndpoints().ReverseGoodsReceipt(ctx, gen.OptGoodsReceiptReverse{}, gen.ReverseGoodsReceiptParams{ID: uuid.MustParse(gr), IfMatch: "*"})
		return err
	}
}

// reverseIn reverses a posted document in the request's transaction, as
// :reverse does.
func reverseIn(v map[string]any) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := endpoints(nil).ReverseSupplierInvoice(ctx, &gen.SupplierInvoiceReverse{Reason: "wrong"},
			gen.ReverseSupplierInvoiceParams{ID: uuid.MustParse(id(v)), IfMatch: "*"})
		return err
	}
}

// onItem1 is a document of the kind for the quantity of item 1 of the
// order at its price of 12.00 with 19% tax, naming no invoice.
func (e env) onItem1(kind, ref string, po uuid.UUID, qty int) string {
	net, tax := int64(qty)*1200, int64(qty)*228
	amount := func(m int64) string { return fmt.Sprintf("%d.%02d", m/100, m%100) }
	return e.document(kind, ref, amount(net+tax), amount(tax), "", poItem(po, 1, fmt.Sprint(qty), "EA", amount(net)))
}

// refusedWith checks a refusal's code and that its message names what it
// says.
func refusedWith(t *testing.T, err error, code string, names ...string) {
	t.Helper()
	pgtest.WantErr(t, err, code)
	for _, n := range names {
		if err != nil && !strings.Contains(err.Error(), n) {
			t.Errorf("%s: got %q, want it to name %q", code, err.Error(), n)
		}
	}
}

// TestReversalsKeepQuantities: a reversal may not take an order item past
// what the match holds a posting to (03 §3.9). A goods receipt's reversal
// is refused below what is invoiced, a credit memo's above what is
// received, an invoice's under credit memos crediting more than the other
// documents invoice or under a subsequent debit naming no invoice once
// nothing remains invoiced; each is allowed once the documents agree. A
// return is not held to what is invoiced: the supplier's credit memo
// follows it.
func TestReversalsKeepQuantities(t *testing.T) {
	e := receiving(t)
	commit := func(e env, fn func(ctx context.Context) error) error {
		return apitest.Committed(e.T, e.Tenant, e.Tenant.Human, fn)
	}
	t.Run("goods receipt reversal below the invoiced quantity", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-G1")
		e.receipt(t, po, "6")
		second := e.receipt(t, po, "4")
		e.posted(t, e.matched(t, e.onItem1("invoice", "G1-I", po, 7)))
		var number string
		pgtest.Scan(t, `SELECT document_number FROM procurement.goods_movement WHERE id = $1`, []any{second}, &number)
		refusedWith(t, commit(e, reverseReceipt(second)), "procurement.reversal_below_invoiced", number, "item 1 of order PO-G1", "6 received of 7 invoiced")
		e.posted(t, e.matched(t, e.onItem1("credit_memo", "G1-C", po, 1)))
		if err := commit(e, reverseReceipt(second)); err != nil {
			t.Errorf("receipt reversal once 6 are invoiced: got %v, want it posted", err)
		}
		// A return takes the item below what is invoiced: the supplier's
		// credit memo for it follows.
		e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "movement_type": "122", "reason": "damaged", "document_date": "2026-03-22",
			"posting_date": "2026-03-22", "items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "2", "unit": "EA"}}]}`, e.DE, po), "")
	})
	t.Run("credit memo reversal past the received quantity", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-G2")
		e.receipt(t, po, "6")
		second := e.receipt(t, po, "4")
		e.posted(t, e.matched(t, e.onItem1("invoice", "G2-I", po, 10)))
		memo, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(e.posted(t, e.matched(t, e.onItem1("credit_memo", "G2-C", po, 4)))), "", "")
		if err := commit(e, reverseReceipt(second)); err != nil {
			t.Fatalf("receipt reversal once 6 are invoiced: got %v, want it posted", err)
		}
		refusedWith(t, commit(e, reverseIn(memo)), "payables.reversal_exceeds_received", memo["document_number"].(string), "item 1 of purchase order PO-G2",
			"invoiced 10 of 6 received")
		e.receipt(t, po, "4")
		if err := commit(e, reverseIn(memo)); err != nil {
			t.Errorf("credit memo reversal once 10 are received: got %v, want it reversed", err)
		}
	})
	t.Run("invoice reversal under a credit memo naming none", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-G3")
		e.receipt(t, po, "10")
		a := e.posted(t, e.matched(t, e.onItem1("invoice", "G3-A", po, 6)))
		b := e.posted(t, e.matched(t, e.onItem1("invoice", "G3-B", po, 4)))
		e.posted(t, e.matched(t, e.onItem1("credit_memo", "G3-C", po, 5)))
		refusedWith(t, commit(e, reverseIn(a)), "payables.reversal_exceeds_invoiced", a["document_number"].(string), "item 1 of purchase order PO-G3", "invoiced -1")
		if err := commit(e, reverseIn(b)); err != nil {
			t.Errorf("reversal of B, leaving 1 invoiced: got %v, want it reversed", err)
		}
	})
	t.Run("invoice reversal under a subsequent debit naming none", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-G4")
		e.receipt(t, po, "10")
		invoice := e.posted(t, e.matched(t, e.onItem1("invoice", "G4-I", po, 10)))
		debit := e.posted(t, e.matched(t, e.document("subsequent_debit", "G4-D", "5.95", "0.95", fmt.Sprintf(`, "purchase_order_id": %q`, po),
			poItem(po, 1, "0", "EA", "5.00"))))
		refusedWith(t, commit(e, reverseIn(invoice)), "payables.reversal_leaves_revaluation", invoice["document_number"].(string), debit["document_number"].(string))
		if err := commit(e, reverseIn(debit)); err != nil {
			t.Fatalf("reversal of the subsequent debit: got %v, want it reversed", err)
		}
		if err := commit(e, reverseIn(invoice)); err != nil {
			t.Errorf("reversal of the invoice once nothing revalues it: got %v, want it reversed", err)
		}
	})
	t.Run("credit memo on an item no longer on the order", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-G5")
		e.receive(t, po, 1, "10")
		e.posted(t, e.matched(t, e.onItem1("invoice", "G5-I", po, 10)))
		memo := e.posted(t, e.matched(t, e.onItem1("credit_memo", "G5-C", po, 10)))
		pgtest.Unguarded(t, `UPDATE procurement.purchase_order_item SET is_removed = true WHERE purchase_order_id = $1 AND item_no = 1`, po)
		refusedWith(t, commit(e, reverseIn(memo)), "payables.reversal_exceeds_received", "invoiced 10 of 0 received")
	})
	t.Run("database failures", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-G6")
		e.receive(t, po, 1, "10")
		e.posted(t, e.matched(t, e.onItem1("invoice", "G6-I", po, 9)))
		e.posted(t, e.matched(t, e.document("subsequent_debit", "G6-D", "5.95", "0.95", fmt.Sprintf(`, "purchase_order_id": %q`, po),
			poItem(po, 1, "0", "EA", "5.00"))))
		memo := e.posted(t, e.matched(t, e.onItem1("credit_memo", "G6-C", po, 2)))
		invoice := e.posted(t, e.matched(t, e.onItem1("invoice", "G6-J", po, 1)))
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "reverse a credit memo", reverseIn(memo))
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "reverse an invoice", reverseIn(invoice))
	})
}

// TestConcurrentReversalsSerialize runs a reversal and a posting on one
// order item in two transactions at once (apitest.Serialized): the second
// waits for the first's lock on the order item, then reads what the first
// committed and is refused, so the two cannot together take the item past
// what was received or invoiced. An order change applied through
// procurement is waited for the same way.
func TestConcurrentReversalsSerialize(t *testing.T) {
	e := receiving(t)
	serialized := func(e env, first, second func(ctx context.Context) error) error {
		firstErr, secondErr := apitest.Serialized(e.T, e.Tenant, e.Tenant.Human, first, second)
		if firstErr != nil {
			e.T.Fatalf("first transaction: got %v, want it committed", firstErr)
		}
		return secondErr
	}
	t.Run("posting after a goods receipt reversal", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-S1")
		gr := e.receipt(t, po, "10")
		invoice := e.matched(t, e.onItem1("invoice", "S1-I", po, 6))
		pgtest.WantErr(t, serialized(e, reverseReceipt(gr), postIn(invoice)), "payables.match_outdated")
		e.wantParked(t, invoice)
	})
	t.Run("goods receipt reversal after a posting", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-S2")
		gr := e.receipt(t, po, "10")
		invoice := e.matched(t, e.onItem1("invoice", "S2-I", po, 6))
		refusedWith(t, serialized(e, postIn(invoice), reverseReceipt(gr)), "procurement.reversal_below_invoiced", "0 received of 6 invoiced")
	})
	t.Run("goods receipt reversal after a credit memo's reversal", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-S3")
		e.receipt(t, po, "6")
		gr := e.receipt(t, po, "4")
		e.posted(t, e.matched(t, e.onItem1("invoice", "S3-I", po, 10)))
		memo := e.posted(t, e.matched(t, e.onItem1("credit_memo", "S3-C", po, 4)))
		refusedWith(t, serialized(e, reverseIn(memo), reverseReceipt(gr)), "procurement.reversal_below_invoiced", "6 received of 10 invoiced")
	})
	t.Run("credit memo reversal after a goods receipt reversal", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-S4")
		e.receipt(t, po, "6")
		gr := e.receipt(t, po, "4")
		e.posted(t, e.matched(t, e.onItem1("invoice", "S4-I", po, 10)))
		memo := e.posted(t, e.matched(t, e.onItem1("credit_memo", "S4-C", po, 4)))
		refusedWith(t, serialized(e, reverseReceipt(gr), reverseIn(memo)), "payables.reversal_exceeds_received", "invoiced 10 of 6 received")
	})
	t.Run("posting after an order change", func(t *testing.T) {
		e := e.in(t)
		po := e.order(t, e.Supplier, "PO-S5")
		e.receipt(t, po, "10")
		invoice := e.matched(t, e.onItem1("invoice", "S5-I", po, 10))
		ch, _ := e.Must(201, "POST", fmt.Sprintf("/v1/purchase-orders/%s/changes", po), fmt.Sprintf(`{"reason": "Price agreed down", "items": [
			{"description": "Paper", "quantity": {"value": "10", "unit": "EA"}, "unit_price": {"amount": "9.00", "currency": "EUR"}, "account_id": %q, "tax_code": "V1"},
			{"description": "Toner", "quantity": {"value": "5", "unit": "KG"}, "unit_price": {"amount": "3.00", "currency": "EUR"}, "account_id": %q}]}`,
			e.expense, e.expense), "")
		submit := func(ctx context.Context) error {
			_, err := procurementEndpoints().SubmitPurchaseOrderChange(ctx, gen.SubmitPurchaseOrderChangeParams{ID: po, ChangeID: uuid.MustParse(id(ch)), IfMatch: "*"})
			return err
		}
		pgtest.WantErr(t, serialized(e, submit, postIn(invoice)), "payables.match_outdated")
		e.wantParked(t, invoice)
		if got := issueCodes(e.act(t, 200, id(invoice), "match", "")); got != "price_variance" {
			t.Errorf("match after the change: got %v, want price_variance", got)
		}
	})
}
