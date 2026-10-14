package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	problem.RegisterConstraint("clearing_item_pkey", func() *problem.Problem {
		return problem.Conflict("ledger.item_already_cleared", "An open item was cleared by another request in the meantime.")
	})
}

// ClearingDocumentType is the document type of clearing entries (SAP AB);
// they draw numbers from the journal_ab number range.
const ClearingDocumentType = "AB"

// ItemRef identifies a journal line.
type ItemRef struct {
	FiscalYear int16     `json:"fiscal_year"`
	EntryID    uuid.UUID `json:"journal_entry_id"`
	LineNo     int32     `json:"line_no"`
}

// Clearing is a row of ledger.clearing with the open items it settled.
type Clearing struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	ClearingDate  time.Time
	EntryYear     int16
	EntryID       uuid.UUID
	Items         []ItemRef
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
}

// The clearing entry's own lines are cleared with the items but are not
// items the caller asked to clear, so they are left out.
const clearingSelect = `SELECT c.id, c.legal_entity_id, c.clearing_date, c.clearing_entry_year, c.clearing_entry_id, c.created_at, c.created_by,
		(SELECT json_agg(json_build_object('fiscal_year', i.fiscal_year, 'journal_entry_id', i.journal_entry_id, 'line_no', i.line_no)
			ORDER BY i.fiscal_year, i.journal_entry_id, i.line_no)
		 FROM ledger.clearing_item i WHERE i.tenant_id = c.tenant_id AND i.clearing_id = c.id AND i.journal_entry_id <> c.clearing_entry_id)
	FROM ledger.clearing c`

func scanClearing(row pgx.Row) (Clearing, error) {
	var c Clearing
	err := errs.Wrapf(row.Scan(&c.ID, &c.LegalEntityID, &c.ClearingDate, &c.EntryYear, &c.EntryID, &c.CreatedAt, &c.CreatedBy, &c.Items), "scan clearing")
	return c, err
}

func getClearing(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Clearing, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Clearing{}, err
	}
	c, err := scanClearing(tx.QueryRow(ctx, clearingSelect+` WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(c.LegalEntityID)) {
		return Clearing{}, problem.NotFound("No clearing with this id.")
	}
	if err != nil {
		return Clearing{}, fmt.Errorf("ledger: read clearing %s: %w", id, err)
	}
	return c, nil
}

// GetClearing returns a clearing.
func GetClearing(ctx context.Context, id uuid.UUID) (Clearing, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Clearing{}, err
	}
	return getClearing(ctx, tx, id)
}

// item is an open item to clear, with what clearing needs to know about it.
type item struct {
	ref                              ItemRef
	found, open, recon, reversed     bool
	legalEntity, ledger              uuid.UUID
	account                          uuid.UUID
	partner                          *uuid.UUID
	tc, lc, gc                       money.Minor
	currency, currencyLC, currencyGC string
	rateTCLC, rateLCGC               string
	reservedBy                       *Owner // the owner holding the item (item_reservation), or nil
}

// createClearing clears open items against each other. The items must be
// open items of one legal entity, ledger and transaction currency that net
// to zero per partner in every currency. The clearing entry (document type
// AB, on the clearing date) offsets each item on its account, so each
// reconciliation account keeps the balance of its open items; its lines are
// cleared together with the items.
func (e *Endpoints) createClearing(ctx context.Context, legalEntity uuid.UUID, date time.Time, refs []ItemRef) (Clearing, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Clearing{}, err
	}
	le, err := e.legalEntity(ctx, legalEntity, "legal_entity_id")
	if err != nil {
		return Clearing{}, err
	}
	if err := requireDistinct(refs); err != nil {
		return Clearing{}, err
	}
	items, err := loadItems(ctx, tx, refs)
	if err != nil {
		return Clearing{}, err
	}
	text := "Clearing"
	lines, err := clearingLines(items, le.ID, &text)
	if err != nil {
		return Clearing{}, err
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Clearing{}, err
	}
	first := items[0]
	entry, err := e.post(ctx, tx, p, draft{entity: le, ledgerID: first.ledger, documentType: ClearingDocumentType, documentDate: date, postingDate: date,
		currencyTC: first.currency, currencyLC: first.currencyLC, currencyGC: first.currencyGC, rateTCLC: first.rateTCLC,
		rateLCGC: first.rateLCGC, headerText: &text, sourceContext: "ledger", lines: lines})
	if err != nil {
		return Clearing{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, id, le.ID, date, entry.FiscalYear, entry.ID, p.ID); err != nil {
		return Clearing{}, fmt.Errorf("ledger: create clearing %s: %w", id, err)
	}
	cleared := slices.Clone(refs)
	for _, l := range entry.Lines {
		cleared = append(cleared, ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: l.LineNo})
	}
	if err := markCleared(ctx, tx, p.TenantID, id, cleared); err != nil {
		return Clearing{}, err
	}
	c, err := getClearing(ctx, tx, id)
	if err != nil {
		return Clearing{}, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "items_cleared", AggregateType: "ledger.clearing", AggregateID: id,
		Payload: map[string]any{"clearing_id": id, "legal_entity_id": le.ID, "clearing_date": date.Format(time.DateOnly),
			"clearing_entry_id": entry.ID, "clearing_entry_year": entry.FiscalYear, "items": clearedItemsPayload(items, entry)}}); err != nil {
		return Clearing{}, err
	}
	return c, audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"items": len(items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}})
}

// requireDistinct rejects a clearing that lists an open item twice.
func requireDistinct(refs []ItemRef) error {
	seen := map[ItemRef]bool{}
	for i, r := range refs {
		if seen[r] {
			return problem.Unprocessable("ledger.duplicate_item", "An open item is listed twice.",
				problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "duplicate", Message: "each item once"})
		}
		seen[r] = true
	}
	return nil
}

// clearingLines are the lines that offset each item on its account. The
// items must be open items on reconciliation accounts of the legal entity,
// in one ledger and transaction currency, and net to zero per partner in
// every currency.
func clearingLines(items []item, legalEntity uuid.UUID, text *string) ([]Line, error) {
	type amounts struct{ tc, lc, gc []money.Minor }
	perPartner := map[uuid.UUID]*amounts{}
	lines := make([]Line, len(items))
	for i, it := range items {
		if err := requireClearable(i, it, items[0], legalEntity); err != nil {
			return nil, err
		}
		s := perPartner[*it.partner]
		if s == nil {
			s = &amounts{}
			perPartner[*it.partner] = s
		}
		s.tc, s.lc, s.gc = append(s.tc, it.tc), append(s.lc, it.lc), append(s.gc, it.gc)
		lines[i] = Line{AccountID: it.account, TC: -it.tc, LC: -it.lc, GC: -it.gc, PartnerID: it.partner, Text: text}
	}
	// Partners in id order: the refusal names the same partner on every run.
	for _, partner := range slices.SortedFunc(maps.Keys(perPartner), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		if s := perPartner[partner]; !netsToZero(s.tc...) || !netsToZero(s.lc...) || !netsToZero(s.gc...) {
			return nil, problem.Unprocessable("ledger.clearing_not_balanced",
				"The items of partner "+partner.String()+" do not net to zero in every currency; settle the difference with a posting first.",
				problem.FieldError{Field: "items", Code: "not_balanced", Message: "items that net to zero per partner"})
		}
	}
	return lines, nil
}

// requireClearable checks item i: an open line on a reconciliation account
// of the legal entity, in the ledger and currency of the first item.
func requireClearable(i int, it, first item, legalEntity uuid.UUID) error {
	field := fmt.Sprintf("items[%d]", i)
	switch {
	case !it.found || !it.recon || it.legalEntity != legalEntity:
		return problem.Unprocessable("ledger.not_an_open_item", "Item "+fmt.Sprint(i+1)+" is not a line on a reconciliation account of this legal entity.",
			problem.FieldError{Field: field, Code: "not_an_open_item", Message: "a line on a supplier or customer reconciliation account"})
	case it.reversed:
		return reversedItem(i)
	case !it.open:
		return problem.Conflict("ledger.item_already_cleared", "Item "+fmt.Sprint(i+1)+" is already cleared.")
	case it.reservedBy != nil:
		return reservedItem(i, *it.reservedBy)
	case it.currency != first.currency || it.ledger != first.ledger:
		return problem.Unprocessable("ledger.mixed_items", "The items of one clearing share their ledger and transaction currency.",
			problem.FieldError{Field: field, Code: "mixed", Message: "an item in " + first.currency + " of the same ledger"})
	}
	return nil
}

// clearedItemsPayload describes the cleared items for the items_cleared
// event.
func clearedItemsPayload(items []item, entry Entry) []map[string]any {
	payload := make([]map[string]any, len(items))
	for i, it := range items {
		payload[i] = map[string]any{"fiscal_year": it.ref.FiscalYear, "journal_entry_id": it.ref.EntryID, "line_no": it.ref.LineNo,
			"account_id": it.account, "partner_id": it.partner, "amount_tc": money.EventAmount(it.tc, entry.CurrencyTC, entry.ExpTC)}
	}
	return payload
}

// loadItems reads the journal lines to clear, in the order of refs; lines
// that do not exist come back with found false, and lines of a reversed
// entry or of a reversal with reversed true.
func loadItems(ctx context.Context, tx pgx.Tx, refs []ItemRef) ([]item, error) {
	years, entries, nos := columns(refs)
	// Locked before reading, so the reversal check below sees any reversal
	// that committed while this waited (the transaction is read committed).
	if err := lockEntries(ctx, tx, true, entries); err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT x.ord, l.journal_entry_id IS NOT NULL, ci.line_no IS NULL, coalesce(a.reconciliation_kind IN ('supplier', 'customer'), false),
			coalesce(l.legal_entity_id, $4), coalesce(l.ledger_id, $4), coalesce(l.account_id, $4), l.partner_id,
			coalesce(l.amount_tc, 0), coalesce(l.amount_lc, 0), coalesce(l.amount_gc, 0), coalesce(e.currency_tc, ''),
			coalesce(e.currency_lc, ''), coalesce(e.currency_gc, ''), coalesce(e.rate_tc_lc::text, ''), coalesce(e.rate_lc_gc::text, ''),
			EXISTS (SELECT FROM ledger.reversal r WHERE r.tenant_id = l.tenant_id
				AND ((r.original_year = l.fiscal_year AND r.original_entry_id = l.journal_entry_id)
					OR (r.reversal_year = l.fiscal_year AND r.reversal_entry_id = l.journal_entry_id))),
			ir.owner_id, coalesce(ir.owner_context, ''), coalesce(ir.owner_reference, '')
		FROM unnest($1::smallint[], $2::uuid[], $3::int[]) WITH ORDINALITY AS x(y, je, n, ord)
		LEFT JOIN ledger.journal_line l ON l.fiscal_year = x.y AND l.journal_entry_id = x.je AND l.line_no = x.n
		LEFT JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		LEFT JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		LEFT JOIN ledger.clearing_item ci ON ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year
			AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no
		LEFT JOIN ledger.item_reservation ir ON ir.tenant_id = l.tenant_id AND ir.fiscal_year = l.fiscal_year
			AND ir.journal_entry_id = l.journal_entry_id AND ir.line_no = l.line_no AND ir.released_at IS NULL
		ORDER BY x.ord`, years, entries, nos, uuid.Nil)
	items := make([]item, 0, len(refs))
	var it item
	var ord int64
	var owner Owner
	var ownerID *uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&ord, &it.found, &it.open, &it.recon, &it.legalEntity, &it.ledger, &it.account, &it.partner,
		&it.tc, &it.lc, &it.gc, &it.currency, &it.currencyLC, &it.currencyGC, &it.rateTCLC, &it.rateLCGC, &it.reversed,
		&ownerID, &owner.Context, &owner.Reference}, func() error {
		it.ref, it.reservedBy = refs[ord-1], nil
		if ownerID != nil {
			holder := owner
			holder.ID = *ownerID
			it.reservedBy = &holder
		}
		items = append(items, it)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: load %d open items: %w", len(refs), err)
	}
	return items, nil
}

// reversedItem refuses item i, a line of a reversed journal entry or of a
// reversal: the reversal cancelled it, so it is not an open item to settle.
// Callers check it after the item is known to be a line on a
// reconciliation account of the legal entity, so the refusal tells nothing
// about other legal entities, and before whether it is cleared: the
// reversal clears the pair (clearReversal), and the refusal names why.
func reversedItem(i int) *problem.Problem {
	return problem.Conflict("ledger.entry_reversed", "Item "+fmt.Sprint(i+1)+
		" is a line of a reversed journal entry or of a reversal; the reversal cancelled it, so it cannot be cleared.")
}

// lockEntries serializes clearing and reversal of the journal entries until
// the transaction ends: clearings take a shared lock on the entries of their
// items, so they do not wait for each other (clearing_item_pkey catches a
// line cleared twice); a reversal takes an exclusive one. The two write
// different tables, so no constraint would stop an entry from being both
// reversed and cleared. The lock is advisory because journal entries are
// insert-only: the application role has no UPDATE privilege on them, which
// SELECT ... FOR SHARE requires.
//
// The lock is taken on the entry's bucket, one of EntryLockBuckets per
// tenant, not on the entry: a transaction holds at most that many locks
// however many entries it touches, where a proposal or a release check
// over 100,000 items would otherwise exhaust the server's shared lock
// table (max_locks_per_transaction, 64 by default, per connection) and
// fail other transactions with it. The tenant is part of the key, so one
// tenant's proposal never holds another's clearings up. Two entries of one
// bucket serialize a reversal against a clearing of the other; shared
// locks never conflict. Each transaction locks its buckets once, in one
// sorted call, so two transactions never wait for each other in a cycle.
func lockEntries(ctx context.Context, tx pgx.Tx, shared bool, ids []uuid.UUID) error {
	buckets := make([]int32, len(ids))
	for i, id := range ids {
		buckets[i] = EntryLockBucket(id)
	}
	buckets = slices.Compact(slices.Sorted(slices.Values(buckets)))
	lock := "pg_advisory_xact_lock"
	if shared {
		lock = "pg_advisory_xact_lock_shared"
	}
	if _, err := tx.Exec(ctx, `SELECT `+lock+`(hashtextextended('ledger.journal_entry:' || platform.current_tenant() || ':' || b, 0)) FROM unnest($1::int[]) AS x(b)`,
		buckets); err != nil {
		return fmt.Errorf("ledger: lock the entries of %d buckets: %w", len(buckets), err)
	}
	return nil
}

// EntryLockBuckets is how many locks a tenant's journal entries share
// (lockEntries): no more than max_locks_per_transaction's default of 64, so
// a transaction holding all of them stays within its share of the lock
// table.
const EntryLockBuckets = 64

// EntryLockBucket is the lock bucket of a journal entry: the low bits of
// its id, which are random because entry ids are version 7 (uuid.NewV7 in
// post). Its advisory lock key is
// hashtextextended('ledger.journal_entry:' || tenant || ':' || bucket, 0).
func EntryLockBucket(id uuid.UUID) int32 {
	return (int32(id[14])<<8 | int32(id[15])) % EntryLockBuckets
}

// markCleared records the lines as cleared by the clearing.
func markCleared(ctx context.Context, tx pgx.Tx, tenant, clearing uuid.UUID, refs []ItemRef) error {
	years, entries, nos := columns(refs)
	_, err := tx.Exec(ctx, `INSERT INTO ledger.clearing_item (tenant_id, fiscal_year, journal_entry_id, line_no, clearing_id)
		SELECT $1, y, je, n, $2 FROM unnest($3::smallint[], $4::uuid[], $5::int[]) AS x(y, je, n)`, tenant, clearing, years, entries, nos)
	if err != nil {
		return fmt.Errorf("ledger: mark items cleared by %s: %w", clearing, err)
	}
	return nil
}

// clearReversal clears the open items of a reversed entry against the
// matching lines of its reversal, with the reversal as the clearing
// document, as SAP does: the pair cancels out, so neither entry stays an
// open item, and nothing clears its lines again (a line is cleared once,
// and other clearings of a reversed entry's lines are refused). It runs in
// the reversal's transaction, which holds the exclusive lock on the
// original. The reversal itself needs no lock: no other transaction can
// see it before this one commits, and a second lock taken out of order
// with the first could deadlock against a transaction that shares their
// buckets (lockEntries). The reversal's lines are
// the original's negated, in the same order (reversalLines, and post
// numbers lines by position), so rev.Lines[i] offsets orig.Lines[i] and
// each pair nets to zero in every currency. An entry without open items
// needs no clearing.
func clearReversal(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, orig, rev Entry) error {
	refs := make([]ItemRef, len(orig.Lines))
	for i, l := range orig.Lines {
		refs[i] = ItemRef{FiscalYear: orig.FiscalYear, EntryID: orig.ID, LineNo: l.LineNo}
	}
	loaded, err := loadItems(ctx, tx, refs)
	if err != nil {
		return err
	}
	var items []item
	var offsets []int32
	for i, it := range loaded {
		if it.recon {
			items, offsets = append(items, it), append(offsets, rev.Lines[i].LineNo)
		}
	}
	if len(items) == 0 {
		return nil
	}
	id, err := clearWithEntry(ctx, tx, actor, orig.LegalEntityID, rev.PostingDate, rev, items, offsets)
	if err != nil {
		return err
	}
	return audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"items": len(items), "clearing_entry_id": rev.ID, "document_number": rev.DocumentNumber, "reversal_of_id": orig.ID}})
}
