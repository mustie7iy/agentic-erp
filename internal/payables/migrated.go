package payables

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

func init() {
	problem.RegisterConstraint("migrated_invoice_item_once", func() *problem.Problem {
		return problem.Conflict("payables.already_taken_over", "This source document's invoiced quantities are already taken over.")
	})
}

// MigratedInvoice is a document a source system posted before migration,
// with what it invoiced or credited on purchase order items (T72b2): SAP's
// posted invoice documents are carried by their journal entries, not as
// supplier invoices, so the three-way match counts these quantities beside
// the documents posted here (invoicedQuantities). Only items that move
// quantity are taken over: a subsequent debit's or credit's revalue none.
type MigratedInvoice struct {
	Reference string // the source document, unique per tenant: "SAP 5100000001/2026"
	Kind      string // KindInvoice or KindCreditMemo, the document's direction
	Items     []MigratedItem
}

// MigratedItem is an order item quantity a migrated document invoiced or
// credited, under the document's item number.
type MigratedItem struct {
	ItemNo          int32
	PurchaseOrderID uuid.UUID
	POItemNo        int32
	Quantity        string // positive; the document's kind gives the direction
}

// TakeOverInvoiced records what a migrated document invoiced or credited
// on order items and returns its id. Each item names an item of a
// purchase order in a legal entity the caller may act in. The items of
// each order are locked as an invoice's posting locks them, so a posting
// on the order waits for the take-over and then counts it, never matching
// past it. A document is taken over once (409 payables.already_taken_over).
func (e *Endpoints) TakeOverInvoiced(ctx context.Context, in MigratedInvoice) (uuid.UUID, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if err := e.checkMigrated(ctx, in); err != nil {
		return uuid.Nil, err
	}
	if err := e.lockMigrated(ctx, in.Items); err != nil {
		return uuid.Nil, err
	}
	id := uuid.Must(uuid.NewV7())
	nos, orders, items, quantities := make([]int32, len(in.Items)), make([]uuid.UUID, len(in.Items)), make([]int32, len(in.Items)), make([]string, len(in.Items))
	for i, it := range in.Items {
		nos[i], orders[i], items[i], quantities[i] = it.ItemNo, it.PurchaseOrderID, it.POItemNo, it.Quantity
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.migrated_invoice_item (tenant_id, migrated_invoice_id, item_no, reference, kind, purchase_order_id,
			po_item_no, quantity, created_by)
		SELECT $1, $2, n, $3, $4, o, i, q::numeric, $5 FROM unnest($6::int[], $7::uuid[], $8::int[], $9::text[]) AS t(n, o, i, q)`,
		p.TenantID, id, in.Reference, in.Kind, p.ID, nos, orders, items, quantities); err != nil {
		return uuid.Nil, fmt.Errorf("payables: take over the quantities of %s: %w", in.Reference, err)
	}
	return id, audit.Record(ctx, "payables.migrated_invoice.take_over", "payables.migrated_invoice", id,
		map[string]any{"reference": in.Reference, "kind": in.Kind, "items": len(in.Items)})
}

// checkMigrated checks a migrated document: a reference, an invoice's or
// credit memo's direction, and items of distinct numbers with a positive
// quantity on an existing item of an order the caller may act in.
func (e *Endpoints) checkMigrated(ctx context.Context, in MigratedInvoice) error {
	switch {
	case strings.TrimSpace(in.Reference) == "" || len(in.Reference) > 100:
		return invalid("payables.invalid_reference", "reference", "The source document's reference has 1 to 100 characters.")
	case in.Kind != KindInvoice && in.Kind != KindCreditMemo:
		return invalid("payables.unknown_kind", "kind", "A migrated document invoices or credits quantities: invoice or credit_memo.")
	case len(in.Items) == 0:
		return invalid("payables.items_required", "items", "A migrated document takes over at least one item.")
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return err
	}
	seen := map[int32]bool{}
	for i, it := range in.Items {
		field := fmt.Sprintf("items[%d]", i)
		if it.ItemNo < 1 || seen[it.ItemNo] {
			return invalid("payables.invalid_item_no", field+".item_no", "Each item has its own positive number.")
		}
		seen[it.ItemNo] = true
		if err := money.PositiveQuantity(it.Quantity); err != nil {
			return invalid("payables.invalid_quantity", field+".quantity", err.Error())
		}
		o, ok, err := e.Orders.Order(ctx, it.PurchaseOrderID)
		if err != nil {
			return err
		}
		if !ok || !access.Allows(o.LegalEntityID) || !slices.ContainsFunc(o.Items, func(oi procurement.OrderItem) bool { return oi.ItemNo == it.POItemNo }) {
			return invalid("payables.invalid_purchase_order", field+".po_item_no", "The item names an item of a purchase order in a legal entity you can access.")
		}
	}
	return nil
}

// lockMigrated locks the order items a migrated document names, order by
// order in id order, each order's items in item order (Orders.LockItems).
func (e *Endpoints) lockMigrated(ctx context.Context, items []MigratedItem) error {
	byOrder := map[uuid.UUID][]int32{}
	for _, it := range items {
		byOrder[it.PurchaseOrderID] = append(byOrder[it.PurchaseOrderID], it.POItemNo)
	}
	orders := make([]uuid.UUID, 0, len(byOrder))
	for o := range byOrder {
		orders = append(orders, o)
	}
	slices.SortFunc(orders, func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
	for _, o := range orders {
		if err := e.Orders.LockItems(ctx, o, byOrder[o]); err != nil {
			return err
		}
	}
	return nil
}

// MigratedQuantities are the net quantities migrated documents invoiced on
// the orders' items (credit memos negative), by order and item, for the
// migration's reconciliation; an item none of them names has no entry.
func (Invoices) MigratedQuantities(ctx context.Context, orders []uuid.UUID) (map[uuid.UUID]map[int32]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT purchase_order_id, po_item_no, trim_scale(sum(CASE WHEN kind = 'credit_memo' THEN -quantity ELSE quantity END))::text
		FROM payables.migrated_invoice_item WHERE purchase_order_id = ANY($1) GROUP BY 1, 2`, orders)
	out := map[uuid.UUID]map[int32]string{}
	var order uuid.UUID
	var no int32
	var qty string
	if _, err := pgx.ForEachRow(rows, []any{&order, &no, &qty}, func() error {
		if out[order] == nil {
			out[order] = map[int32]string{}
		}
		out[order][no] = qty
		return nil
	}); err != nil {
		return nil, fmt.Errorf("payables: read the migrated quantities of %d orders: %w", len(orders), err)
	}
	return out, nil
}
