package receivables

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A relieved invoice cleared apart (ADR-0049 As built batch 94). Only an
// incoming payment charges a relieved invoice's tax again (recharge.go):
// a ledger clearing (createClearing) of the invoice's items, the residual
// items its payments left or its write-offs' credits against an item
// that is none of them, such as a credit posted straight to the
// customer, would settle what was relieved without charging its tax
// again, and paidOf does not count it. So such a clearing is refused, and
// so is the relief of an invoice whose lines a clearing so settled before
// it, each 422 receivables.relieved_invoice_clearing. A clearing of those
// lines with each other alone, such as the invoice's item with its
// write-off's credit, settles nothing the customer paid and is allowed.

// relievedClearing is the refusals' problem code.
const relievedClearing = "receivables.relieved_invoice_clearing"

// relievedLines are the lines of a relieved invoice that what the
// customer pays of it applies (paidOf): every line of its entry, the
// residual items its payments left and its write-offs' credits.
type relievedLines struct {
	invoice Invoice
	lines   []ledger.ItemRef
}

// has tells whether the line is one of them.
func (r relievedLines) has(ref ledger.ItemRef) bool {
	return (ref.FiscalYear == *r.invoice.EntryYear && ref.EntryID == *r.invoice.EntryID) || slices.Contains(r.lines, ref)
}

// relievedLinesOf reads the invoice's lines, credits its write-offs'
// credits: the residual items are those its payments' items left
// (appliedItems), undone or not, since a residual an undoing reversed is
// cleared with its reversal and no clearing takes it again.
func relievedLinesOf(ctx context.Context, tx pgx.Tx, v Invoice, credits []ledger.ItemRef) (relievedLines, error) {
	years, entries, lines := ledger.ItemColumns(credits)
	rows, _ := tx.Query(ctx, appliedItems+`
		SELECT DISTINCT cy, ce, residual_line_no FROM applied WHERE residual_line_no IS NOT NULL ORDER BY cy, ce, residual_line_no`,
		*v.EntryID, *v.EntryYear, years, entries, lines)
	residuals, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ledger.ItemRef])
	if err != nil {
		return relievedLines{}, fmt.Errorf("receivables: the residual items of customer invoice %s: %w", v.DocumentNumber, err)
	}
	return relievedLines{invoice: v, lines: append(slices.Clone(credits), residuals...)}, nil
}

// Clearings is receivables' guard of the ledger's clearings
// (ledger.ClearingGuard; wired in internal/app): what a customer pays of
// a relieved invoice is applied by an incoming payment, never cleared
// against another item (see above).
type Clearings struct {
	WriteOffs WriteOffs
}

// GuardClearing holds the partners as customers of the company
// (holdRelieved, which orders the clearing with the customers' payments
// and reliefs and locks their relieved invoices), reads those invoices'
// lines and refuses items that clear one of them with an item that is
// none of them.
func (c Clearings) GuardClearing(ctx context.Context, legalEntity uuid.UUID, partners []uuid.UUID, items []ledger.ItemRef) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	invoices, err := holdRelieved(ctx, tx, legalEntity, partners)
	if err != nil {
		return err
	}
	sets := make([]relievedLines, 0, len(invoices))
	for _, id := range invoices {
		v, err := get(ctx, tx, id, false) // locked by holdRelieved
		if err != nil {
			return fmt.Errorf("receivables: relieved customer invoice %s: %w", id, err)
		}
		s, err := reliefStateOf(ctx, tx, c.WriteOffs, v)
		if err != nil {
			return err
		}
		r, err := relievedLinesOf(ctx, tx, v, s.credits)
		if err != nil {
			return err
		}
		sets = append(sets, r)
	}
	return clearedApart(sets, items)
}

// clearedApart refuses items of which one is a line of a relieved
// invoice (of sets) and another is no such line, naming the first of
// those in the request's items (422 receivables.relieved_invoice_clearing).
func clearedApart(sets []relievedLines, items []ledger.ItemRef) error {
	var relieved *Invoice
	other := -1
	for i, it := range items {
		at := slices.IndexFunc(sets, func(r relievedLines) bool { return r.has(it) })
		switch {
		case at < 0 && other < 0:
			other = i
		case at >= 0 && relieved == nil:
			relieved = &sets[at].invoice
		}
	}
	if relieved == nil || other < 0 {
		return nil
	}
	return problem.Unprocessable(relievedClearing, "Customer invoice "+relieved.DocumentNumber+" has a bad debt relief: what the customer pays of "+
		"it is applied with an incoming payment (POST /v1/incoming-payments), which charges the relieved tax again, and its items, the residual "+
		"items its payments left and its write-offs' credits are cleared against none but each other.",
		problem.FieldError{Field: fmt.Sprintf("items[%d]", other), Code: "not_relieved_invoice",
			Message: "a line of customer invoice " + relieved.DocumentNumber + ", a residual item its payments left or a credit of its write-offs"})
}

// notClearedApart refuses the relief of an invoice whose lines (r) a
// clearing settled against an item that is none of them
// (ledger.OpenItems.ClearedApart): what that clearing settled is not
// counted as paid (paidOf), so whether the debt is bad cannot be told
// (422 receivables.relieved_invoice_clearing naming customer_invoice_id
// and the first such clearing). The customer's hold, which a clearing's
// guard takes too, orders the two.
func (e *Endpoints) notClearedApart(ctx context.Context, r relievedLines) error {
	v := r.invoice
	apart, err := e.OpenItems.ClearedApart(ctx, *v.EntryYear, *v.EntryID, r.lines)
	if err != nil {
		return fmt.Errorf("receivables: the clearings of customer invoice %s: %w", v.DocumentNumber, err)
	}
	if len(apart) == 0 {
		return nil
	}
	return problem.InvalidDetail(relievedClearing, "Clearing "+apart[0].DocumentNumber+" (id "+apart[0].ID.String()+") cleared customer invoice "+
		v.DocumentNumber+"'s items or its write-offs' credits against an item that is none of them, such as a credit posted straight to the "+
		"customer: what it settled is not counted as paid, so the invoice is no bad debt to relieve.", "customer_invoice_id",
		"a customer invoice whose items no clearing settled against another item")
}
