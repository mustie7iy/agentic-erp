package payables

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// reversible refuses the reversal of a document on a purchase order that
// would leave one of its order items past what the match holds a posting
// to (03 §3.9), as the documents posted on the order would stand without
// it:
//   - a credit memo's reversal invoicing more than received
//     (409 payables.reversal_exceeds_received);
//   - an invoice's reversal leaving credit memos crediting more than
//     remains invoiced (409 payables.reversal_exceeds_invoiced), or a
//     subsequent debit or credit revaluing an item nothing remains
//     invoiced of (409 payables.reversal_leaves_revaluation), whatever
//     invoice it names, as the match holds it (nothing_invoiced); those
//     naming this invoice are refused before (requireNoCorrections).
//
// A subsequent debit's or credit's reversal moves no quantity and is not
// checked. The order items are locked first (Orders.LockItems), after the
// document's row and before the ledger's GR/IR lock, as a posting locks
// them, so a posting, another reversal and a goods movement on them wait
// for this one and what it reads holds until it commits.
func (e *Endpoints) reversible(ctx context.Context, tx pgx.Tx, v Invoice) error {
	nos := orderItemNos(v)
	if v.PurchaseOrderID == nil || len(nos) == 0 || subsequent(v.Kind) {
		return nil
	}
	if err := e.Orders.LockItems(ctx, *v.PurchaseOrderID, nos); err != nil {
		return err
	}
	order, err := e.orderOf(ctx, v)
	if err != nil {
		return err
	}
	left, err := invoicedQuantities(ctx, tx, *v.PurchaseOrderID, v.ID)
	if err != nil {
		return err
	}
	if v.Kind == KindCreditMemo {
		return creditReversible(v, *order, nos, left)
	}
	return invoiceReversible(ctx, tx, v, *order, nos, left)
}

// creditReversible refuses a credit memo's reversal that would invoice an
// order item past what was received (left is what the other documents
// invoice), as a goods receipt's reversal since the memo may have left it.
func creditReversible(v Invoice, o procurement.Order, nos []int32, left map[int32]string) error {
	for _, no := range nos {
		received := "0"
		if i := slices.IndexFunc(o.Items, func(oi procurement.OrderItem) bool { return oi.ItemNo == no }); i >= 0 {
			received = o.Items[i].Received
		}
		invoiced := cmp.Or(left[no], "0")
		if money.ParseRat(invoiced).Cmp(money.ParseRat(received)) > 0 {
			return problem.Conflict("payables.reversal_exceeds_received", fmt.Sprintf(
				"Reversing credit memo %s would leave item %d of purchase order %s invoiced %s of %s received; receive the goods again, or credit the difference otherwise.",
				v.DocumentNumber, no, o.DocumentNumber, invoiced, received))
		}
	}
	return nil
}

// invoiceReversible refuses an invoice's reversal that would leave an
// order item credited past what remains invoiced (left is what the other
// documents invoice), or revalued by a posted subsequent debit or credit
// once nothing remains invoiced of it.
func invoiceReversible(ctx context.Context, tx pgx.Tx, v Invoice, o procurement.Order, nos []int32, left map[int32]string) error {
	revaluations, err := postedRevaluations(ctx, tx, v, nos)
	if err != nil {
		return err
	}
	for _, no := range nos {
		invoiced := money.ParseRat(cmp.Or(left[no], "0"))
		switch {
		case invoiced.Sign() < 0:
			return problem.Conflict("payables.reversal_exceeds_invoiced", fmt.Sprintf(
				"Reversing invoice %s would leave item %d of purchase order %s invoiced %s: its credit memos would credit more than the other documents invoice; reverse them first.",
				v.DocumentNumber, no, o.DocumentNumber, left[no]))
		case invoiced.Sign() == 0 && len(revaluations[no]) > 0:
			return problem.Conflict("payables.reversal_leaves_revaluation", fmt.Sprintf(
				"Reversing invoice %s would leave nothing invoiced of item %d of purchase order %s, which %s revalues; reverse it first.",
				v.DocumentNumber, no, o.DocumentNumber, strings.Join(revaluations[no], ", ")))
		}
	}
	return nil
}

// postedRevaluations are the posted subsequent debits and credits on each
// of the invoice's order items, by document number, whatever invoice they
// name: the match holds each to something invoiced of its item.
func postedRevaluations(ctx context.Context, tx pgx.Tx, v Invoice, nos []int32) (map[int32][]string, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT t.po_item_no, i.document_number FROM payables.supplier_invoice_item t
		JOIN payables.supplier_invoice i ON i.tenant_id = t.tenant_id AND i.id = t.supplier_invoice_id
		WHERE t.purchase_order_id = $1 AND t.po_item_no = ANY($2) AND NOT t.is_removed AND i.status = 'posted'
			AND i.kind IN ('subsequent_debit', 'subsequent_credit') ORDER BY 1, 2`, *v.PurchaseOrderID, nos)
	out := map[int32][]string{}
	var no int32
	var number string
	if _, err := pgx.ForEachRow(rows, []any{&no, &number}, func() error { out[no] = append(out[no], number); return nil }); err != nil {
		return nil, fmt.Errorf("payables: read the revaluations posted on order %s: %w", *v.PurchaseOrderID, err)
	}
	return out, nil
}
