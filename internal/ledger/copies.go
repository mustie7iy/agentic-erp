package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// itemLedgers are the ledgers a clearing's own entry posts to (ADR-0045,
// T52c): those in which every item's entry has an entry of its posting
// group, the leading ledger first. An item posted to the leading ledger
// alone (before T52b, or as a ledger-specific posting) narrows the clearing
// to the leading ledger, so no ledger gets the offset of an item it never
// had. The items are the leading ledger's (loadItems), so the leading
// ledger is always among them.
func itemLedgers(ctx context.Context, tx pgx.Tx, items []item) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]bool{}
	var years []int16
	var entries []uuid.UUID
	for _, it := range items {
		if !seen[it.ref.EntryID] {
			seen[it.ref.EntryID] = true
			years, entries = append(years, it.ref.FiscalYear), append(entries, it.ref.EntryID)
		}
	}
	ledgers := []uuid.UUID{}
	err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(x.ledger_id ORDER BY NOT g.is_leading, g.code), '{}') FROM (
			SELECT c.ledger_id FROM unnest($1::smallint[], $2::uuid[]) AS i(y, je)
			JOIN ledger.journal_entry o ON o.fiscal_year = i.y AND o.id = i.je
			JOIN ledger.journal_entry c ON c.tenant_id = o.tenant_id AND c.fiscal_year = o.fiscal_year
				AND (c.id = o.id OR c.posting_group_id = o.posting_group_id)
			GROUP BY c.ledger_id HAVING count(DISTINCT o.id) = $3) x
		JOIN ledger.ledger g ON g.id = x.ledger_id`, years, entries, len(entries)).Scan(&ledgers)
	if err != nil {
		return nil, fmt.Errorf("ledger: ledgers of %d cleared entries: %w", len(entries), err)
	}
	return ledgers, nil
}

// clearCopies applies a clearing to the copies of its lines in the other
// ledgers of their posting groups (ADR-0045, T52c), in the clearing's
// transaction: in each ledger where the clearing document doc.by has a
// copy, a clearing by that copy clears the copies of the cleared lines.
// The lines clear in sets (sets[i] is the set of cleared[i]: an item with
// the line that offsets it, or every item of a migrated clearing), and a
// set is cleared in a ledger only when each of its lines has a copy there,
// so a ledger never clears an item without its offset. The copies'
// clearings are part of the clearing: audited and announced (items_cleared)
// once, by the leading ledger's, which is the one the API shows.
func clearCopies(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, doc clearingDoc, cleared []ItemRef, sets []int) error {
	years, entries := []int16{doc.by.FiscalYear}, []uuid.UUID{doc.by.EntryID}
	for _, r := range cleared {
		years, entries = append(years, r.FiscalYear), append(entries, r.EntryID)
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT o.id, c.ledger_id, c.id FROM unnest($1::smallint[], $2::uuid[]) AS i(y, je)
		JOIN ledger.journal_entry o ON o.fiscal_year = i.y AND o.id = i.je
		JOIN ledger.journal_entry c ON c.tenant_id = o.tenant_id AND c.fiscal_year = o.fiscal_year
			AND c.posting_group_id = o.posting_group_id AND c.id <> o.id
		ORDER BY c.ledger_id`, years, entries)
	copies := map[uuid.UUID]map[uuid.UUID]uuid.UUID{} // entry → ledger → its copy there
	var ledgers []uuid.UUID                           // the ledgers of doc.by's copies, in order
	var entry, ledger, copied uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&entry, &ledger, &copied}, func() error {
		if copies[entry] == nil {
			copies[entry] = map[uuid.UUID]uuid.UUID{}
		}
		copies[entry][ledger] = copied
		if entry == doc.by.EntryID {
			ledgers = append(ledgers, ledger)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ledger: copies of clearing by entry %s: %w", doc.by.EntryID, err)
	}
	for _, ledger := range ledgers {
		refs := copiedRefs(cleared, sets, copies, ledger)
		if len(refs) == 0 {
			continue
		}
		copyDoc := doc
		copyDoc.by.EntryID = copies[doc.by.EntryID][ledger]
		id, err := insertClearing(ctx, tx, actor, copyDoc)
		if err != nil {
			return err
		}
		if err := markCleared(ctx, tx, actor.TenantID, id, refs); err != nil {
			return err
		}
	}
	return nil
}

// copiedRefs are the copies in ledger of the cleared lines whose set has a
// copy of every line there. A copy has its first entry's lines under the
// same numbers and fiscal year (postAll).
func copiedRefs(cleared []ItemRef, sets []int, copies map[uuid.UUID]map[uuid.UUID]uuid.UUID, ledger uuid.UUID) []ItemRef {
	incomplete := map[int]bool{}
	for i, r := range cleared {
		if _, ok := copies[r.EntryID][ledger]; !ok {
			incomplete[sets[i]] = true
		}
	}
	var refs []ItemRef
	for i, r := range cleared {
		if !incomplete[sets[i]] {
			refs = append(refs, ItemRef{FiscalYear: r.FiscalYear, EntryID: copies[r.EntryID][ledger], LineNo: r.LineNo})
		}
	}
	return refs
}
