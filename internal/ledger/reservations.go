package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Open-item reservations (03-data-model §3.7): a context about to settle
// open items (a payment run) reserves them here, in the ledger's own
// ledger.item_reservation, so the ledger's manual clearing and reversal
// refuse them without reading the owner's tables: payments depends on the
// ledger, never the other way round. The owner reserves and frees its items
// through Postings in the transaction that changes its document; its
// settlement (Settlement.ReservedFor) pays them and ends the reservation.
// Reserve and Release write no audit record of their own: they change
// nothing but the owner's document does, and the owner's audit record of
// that change (a run proposed, rejected, or a payment dropped or posted)
// covers them (ADR-0011).

func init() {
	problem.RegisterConstraint("item_reserved_once", func() *problem.Problem {
		return problem.Conflict("ledger.item_reserved", "An open item was reserved by another request in the meantime.")
	})
}

// Owner is who holds reserved open items: a document of another context.
type Owner struct {
	Context   string    // the owner's context: payments
	ID        uuid.UUID // the owner's document: a payment run
	Reference string    // what refusals name: the run's document number
}

// reservedItem refuses item i, held by another owner.
func reservedItem(i int, by Owner) *problem.Problem {
	return problem.Conflict("ledger.item_reserved", "Item "+fmt.Sprint(i+1)+" is reserved by "+by.Context+" document "+by.Reference+
		", which is about to settle it; it is cleared there, or freed when that document lets it go (a payment run rejected).")
}

// Reserve reserves open items for the owner. Each must be an open item on
// a reconciliation account, not reversed and not held by another owner.
// The entries are locked exclusively first, so a clearing or reversal of
// them under way finishes before the check, and one that starts later
// waits for this transaction and then sees the reservation.
func (Postings) Reserve(ctx context.Context, owner Owner, refs []ItemRef) error {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil || len(refs) == 0 {
		return err
	}
	entries := make([]uuid.UUID, len(refs))
	for i, r := range refs {
		entries[i] = r.EntryID
	}
	if err := lockEntries(ctx, tx, false, entries); err != nil {
		return err
	}
	items, err := loadItems(ctx, tx, refs)
	if err != nil {
		return err
	}
	for i, it := range items {
		if err := requireReservable(i, it); err != nil {
			return err
		}
	}
	years, ids, nos := columns(refs)
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context,
			owner_reference, created_by)
		SELECT $1, $2, y, je, n, $3, $4, $5 FROM unnest($6::smallint[], $7::uuid[], $8::int[]) AS x(y, je, n)`,
		actor.TenantID, owner.ID, owner.Context, owner.Reference, actor.ID, years, ids, nos); err != nil {
		return fmt.Errorf("ledger: reserve %d items for %s %s: %w", len(refs), owner.Context, owner.ID, err)
	}
	return nil
}

// requireReservable checks item i: an open item, not reversed, not held.
func requireReservable(i int, it item) error {
	switch {
	case !it.found || !it.recon:
		return problem.Unprocessable("ledger.not_an_open_item", "Item "+fmt.Sprint(i+1)+" is not a line on a reconciliation account.")
	case it.reversed:
		return reversedItem(i)
	case !it.open:
		return problem.Conflict("ledger.item_already_cleared", "Item "+fmt.Sprint(i+1)+" is already cleared.")
	case it.reservedBy != nil:
		return reservedItem(i, *it.reservedBy)
	}
	return nil
}

// Release frees the owner's reserved items; refs nil frees every item the
// owner holds. Items not held by the owner are left alone, so releasing
// twice changes nothing.
func (Postings) Release(ctx context.Context, owner uuid.UUID, refs []ItemRef) error {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	return release(ctx, tx, actor, owner, refs)
}

func release(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, owner uuid.UUID, refs []ItemRef) error {
	q := `UPDATE ledger.item_reservation SET released_at = now(), released_by = $2 WHERE owner_id = $1 AND released_at IS NULL`
	args := []any{owner, actor.ID}
	if refs != nil {
		years, ids, nos := columns(refs)
		q += ` AND (fiscal_year, journal_entry_id, line_no) IN (SELECT * FROM unnest($3::smallint[], $4::uuid[], $5::int[]))`
		args = append(args, years, ids, nos)
	}
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("ledger: release the items reserved by %s: %w", owner, err)
	}
	return nil
}

// columns splits item references into the arrays unnest takes.
func columns(refs []ItemRef) (years []int16, entries []uuid.UUID, nos []int32) {
	years, entries, nos = make([]int16, len(refs)), make([]uuid.UUID, len(refs)), make([]int32, len(refs))
	for i, r := range refs {
		years[i], entries[i], nos[i] = r.FiscalYear, r.EntryID, r.LineNo
	}
	return years, entries, nos
}

// requireUnreserved refuses to reverse an entry with a reserved line: its
// owner is about to settle it.
func requireUnreserved(ctx context.Context, tx pgx.Tx, orig Entry) error {
	var owner Owner
	err := tx.QueryRow(ctx, `SELECT owner_context, owner_id, owner_reference FROM ledger.item_reservation
		WHERE fiscal_year = $1 AND journal_entry_id = $2 AND released_at IS NULL LIMIT 1`, orig.FiscalYear, orig.ID).
		Scan(&owner.Context, &owner.ID, &owner.Reference)
	switch {
	case err == nil:
		return problem.Conflict("ledger.item_reserved", "Lines of the journal entry are reserved by "+owner.Context+" document "+owner.Reference+
			", which is about to settle them; free them there first (reject the payment run).")
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("ledger: reverse entry %s: read its reservations: %w", orig.ID, err)
	}
	return nil
}
