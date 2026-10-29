package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// takeOver records a migrated document's quantities, committed, as a
// migration load does.
func (e env) takeOver(t *testing.T, ref, kind string, items ...payables.MigratedItem) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		id, err = endpoints(nil).TakeOverInvoiced(ctx, payables.MigratedInvoice{Reference: ref, Kind: kind, Items: items})
		return err
	}); err != nil {
		t.Fatalf("take over %s: %v", ref, err)
	}
	return id
}

func migratedItem(no int32, po uuid.UUID, poItem int32, qty string) payables.MigratedItem {
	return payables.MigratedItem{ItemNo: no, PurchaseOrderID: po, POItemNo: poItem, Quantity: qty}
}

// TestMigratedQuantitiesMatch: what a source system invoiced and credited
// before migration counts in the three-way match beside the documents
// posted here (T72b2): a credit memo is held while nothing is invoiced and
// matches clean once SAP's invoiced quantity is taken over, a subsequent
// debit likewise, and SAP's credit memos reduce what is invoiced.
func TestMigratedQuantitiesMatch(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-M")
	e.receive(t, po, 1, "10")
	onOrder := fmt.Sprintf(`, "purchase_order_id": %q`, po)
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-M", "24.00", "0.00", onOrder, poItem(po, 1, "2", "EA", "24.00")), "")
	debit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_debit", "SD-M", "5.00", "0.00", onOrder, poItem(po, 1, "0", "EA", "5.00")), "")
	issue := func(t *testing.T, invoice map[string]any) string {
		t.Helper()
		m := e.act(t, 200, id(invoice), "match", "")
		if m["is_clean"] == true {
			return "clean"
		}
		first := m["issues"].([]any)[0].(map[string]any)
		return fmt.Sprint(first["code"], ": ", first["message"])
	}
	t.Run("nothing invoiced", func(t *testing.T) {
		if got, want := issue(t, memo), "quantity_exceeds_invoiced: order item 1: credited quantity 2 exceeds invoiced 0.000000"; got != want {
			t.Errorf("credit memo: got %s, want %s", got, want)
		}
		if got, want := issue(t, debit), "nothing_invoiced: order item 1: nothing is invoiced to revalue"; got != want {
			t.Errorf("subsequent debit: got %s, want %s", got, want)
		}
	})
	e.takeOver(t, "SAP 5100000001/2026", payables.KindInvoice, migratedItem(1, po, 1, "3"), migratedItem(2, po, 2, "1"))
	t.Run("SAP invoiced 3", func(t *testing.T) {
		if got := issue(t, memo); got != "clean" {
			t.Errorf("credit memo: got %s, want clean", got)
		}
		if got := issue(t, debit); got != "clean" {
			t.Errorf("subsequent debit: got %s, want clean", got)
		}
	})
	e.takeOver(t, "SAP 5200000001/2026", payables.KindCreditMemo, migratedItem(1, po, 1, "2"))
	t.Run("SAP credited 2 of them", func(t *testing.T) {
		if got, want := issue(t, memo), "quantity_exceeds_invoiced: order item 1: credited quantity 2 exceeds invoiced 1.000000"; got != want {
			t.Errorf("credit memo: got %s, want %s", got, want)
		}
	})
	t.Run("an invoice counts SAP's invoiced quantity against the receipt", func(t *testing.T) {
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-M", "120.00", "0.00", poItem(po, 1, "10", "EA", "120.00")), "")
		if got, want := issue(t, inv), "quantity_exceeds_received: order item 1: invoiced quantity 11.000000 exceeds received 10"; got != want {
			t.Errorf("invoice: got %s, want %s", got, want)
		}
	})
	t.Run("reconciliation reads the net quantities", func(t *testing.T) {
		other := e.order(t, e.Supplier, "PO-N")
		var got map[uuid.UUID]map[int32]string
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			var err error
			got, err = payables.Invoices{}.MigratedQuantities(ctx, []uuid.UUID{po, other})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if s := fmt.Sprint(got); s != fmt.Sprintf("map[%s:map[1:1 2:1]]", po) {
			t.Errorf("migrated quantities: got %s, want item 1 net 1 and item 2 1 on %s only", s, po)
		}
	})
}

// TestTakeOverRefusals: a migrated document names its source document,
// invoices or credits, and has items of their own numbers with a positive
// quantity on an item of an order the caller may act in; it is taken over
// once. The database refuses what the service would.
func TestTakeOverRefusals(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-R")
	good := []payables.MigratedItem{migratedItem(1, po, 1, "1")}
	e.takeOver(t, "SAP 5100000009/2026", payables.KindInvoice, good...)
	cases := []struct {
		name, code string
		in         payables.MigratedInvoice
	}{
		{"no reference", "payables.invalid_reference", payables.MigratedInvoice{Reference: " ", Kind: payables.KindInvoice, Items: good}},
		{"long reference", "payables.invalid_reference", payables.MigratedInvoice{Reference: strings.Repeat("R", 101), Kind: payables.KindInvoice, Items: good}},
		{"subsequent debit", "payables.unknown_kind", payables.MigratedInvoice{Reference: "R", Kind: payables.KindSubsequentDebit, Items: good}},
		{"no items", "payables.items_required", payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice}},
		{"item number 0", "payables.invalid_item_no", payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice, Items: []payables.MigratedItem{migratedItem(0, po, 1, "1")}}},
		{"item number twice", "payables.invalid_item_no", payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice,
			Items: []payables.MigratedItem{migratedItem(1, po, 1, "1"), migratedItem(1, po, 2, "1")}}},
		{"quantity 0", "payables.invalid_quantity", payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice, Items: []payables.MigratedItem{migratedItem(1, po, 1, "0")}}},
		{"unknown order", "payables.invalid_purchase_order", payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice,
			Items: []payables.MigratedItem{migratedItem(1, uuid.New(), 1, "1")}}},
		{"unknown order item", "payables.invalid_purchase_order", payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice,
			Items: []payables.MigratedItem{migratedItem(1, po, 3, "1")}}},
		{"taken over before", "migrated_invoice_item_once", payables.MigratedInvoice{Reference: "SAP 5100000009/2026", Kind: payables.KindInvoice, Items: good}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := endpoints(nil).TakeOverInvoiced(ctx, c.in)
				return err
			})
			pgtest.WantErr(t, err, c.code)
		})
	}
	t.Run("an order of a legal entity the caller cannot act in", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Reader(t, "Elsewhere"), func(ctx context.Context) error {
			_, err := endpoints(nil).TakeOverInvoiced(ctx, payables.MigratedInvoice{Reference: "R", Kind: payables.KindInvoice, Items: good})
			return err
		})
		pgtest.WantErr(t, err, "payables.invalid_purchase_order")
	})
	if got := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "migrated_invoice_item_once"}).Code; got != "payables.already_taken_over" {
		t.Errorf("constraint: got %s, want payables.already_taken_over", got)
	}
	t.Run("outside a request", func(t *testing.T) {
		if _, err := endpoints(nil).TakeOverInvoiced(context.Background(), payables.MigratedInvoice{}); err == nil {
			t.Error("take over: got no error, want one")
		}
		if _, err := (payables.Invoices{}).MigratedQuantities(context.Background(), nil); err == nil {
			t.Error("migrated quantities: got no error, want one")
		}
	})
	t.Run("database failures", func(t *testing.T) {
		other := e.order(t, e.Supplier, "PO-F")
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "take over", func(ctx context.Context) error {
			_, err := endpoints(nil).TakeOverInvoiced(ctx, payables.MigratedInvoice{Reference: "SAP F", Kind: payables.KindCreditMemo,
				Items: []payables.MigratedItem{migratedItem(1, other, 1, "1"), migratedItem(2, po, 2, "1"), migratedItem(3, other, 2, "1")}})
			return err
		})
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "migrated quantities", func(ctx context.Context) error {
			_, err := payables.Invoices{}.MigratedQuantities(ctx, []uuid.UUID{po})
			return err
		})
	})
}
