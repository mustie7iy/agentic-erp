package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
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
	err := row.Scan(&c.ID, &c.LegalEntityID, &c.ClearingDate, &c.EntryYear, &c.EntryID, &c.CreatedAt, &c.CreatedBy, &c.Items)
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
	return c, err
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
	found, open, recon               bool
	legalEntity, ledger              uuid.UUID
	account                          uuid.UUID
	partner                          *uuid.UUID
	tc, lc, gc                       int64
	currency, currencyLC, currencyGC string
	rateTCLC, rateLCGC               string
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
	seen := map[ItemRef]bool{}
	for i, r := range refs {
		if seen[r] {
			return Clearing{}, problem.Unprocessable("ledger.duplicate_item", "An open item is listed twice.",
				problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "duplicate", Message: "each item once"})
		}
		seen[r] = true
	}
	items, err := loadItems(ctx, tx, refs)
	if err != nil {
		return Clearing{}, err
	}
	type sums struct{ tc, lc, gc int64 }
	perPartner := map[uuid.UUID]*sums{}
	lines := make([]Line, len(items))
	text := "Clearing"
	for i, it := range items {
		field := fmt.Sprintf("items[%d]", i)
		switch {
		case !it.found || !it.recon || it.legalEntity != le.ID:
			return Clearing{}, problem.Unprocessable("ledger.not_an_open_item", "Item "+fmt.Sprint(i+1)+" is not a line on a reconciliation account of this legal entity.",
				problem.FieldError{Field: field, Code: "not_an_open_item", Message: "a line on a supplier or customer reconciliation account"})
		case !it.open:
			return Clearing{}, problem.Conflict("ledger.item_already_cleared", "Item "+fmt.Sprint(i+1)+" is already cleared.")
		case it.currency != items[0].currency || it.ledger != items[0].ledger:
			return Clearing{}, problem.Unprocessable("ledger.mixed_items", "The items of one clearing share their ledger and transaction currency.",
				problem.FieldError{Field: field, Code: "mixed", Message: "an item in " + items[0].currency + " of the same ledger"})
		}
		s := perPartner[*it.partner]
		if s == nil {
			s = &sums{}
			perPartner[*it.partner] = s
		}
		s.tc, s.lc, s.gc = s.tc+it.tc, s.lc+it.lc, s.gc+it.gc
		lines[i] = Line{AccountID: it.account, TC: -it.tc, LC: -it.lc, GC: -it.gc, PartnerID: it.partner, Text: &text}
	}
	for partner, s := range perPartner {
		if *s != (sums{}) {
			return Clearing{}, problem.Unprocessable("ledger.clearing_not_balanced",
				"The items of partner "+partner.String()+" do not net to zero in every currency; settle the difference with a posting first.",
				problem.FieldError{Field: "items", Code: "not_balanced", Message: "items that net to zero per partner"})
		}
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Clearing{}, err
	}
	first := items[0]
	entry, err := e.post(ctx, draft{entity: le, ledgerID: first.ledger, documentType: ClearingDocumentType, documentDate: date, postingDate: date,
		currencyTC: first.currency, currencyLC: first.currencyLC, currencyGC: first.currencyGC, rateTCLC: first.rateTCLC,
		rateLCGC: first.rateLCGC, headerText: &text, sourceContext: "ledger", lines: lines})
	if err != nil {
		return Clearing{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, id, le.ID, date, entry.FiscalYear, entry.ID, p.ID); err != nil {
		return Clearing{}, err
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
	payload := make([]map[string]any, len(items))
	for i, it := range items {
		payload[i] = map[string]any{"fiscal_year": it.ref.FiscalYear, "journal_entry_id": it.ref.EntryID, "line_no": it.ref.LineNo,
			"account_id": it.account, "partner_id": it.partner, "amount_tc": amount(it.tc, entry.CurrencyTC, entry.ExpTC)}
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "items_cleared", AggregateType: "ledger.clearing", AggregateID: id,
		Payload: map[string]any{"clearing_id": id, "legal_entity_id": le.ID, "clearing_date": date.Format(time.DateOnly),
			"clearing_entry_id": entry.ID, "clearing_entry_year": entry.FiscalYear, "items": payload}}); err != nil {
		return Clearing{}, err
	}
	return c, audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"items": len(items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}})
}

// loadItems reads the journal lines to clear, in the order of refs; lines
// that do not exist come back with found false.
func loadItems(ctx context.Context, tx pgx.Tx, refs []ItemRef) ([]item, error) {
	years, entries, nos := make([]int16, len(refs)), make([]uuid.UUID, len(refs)), make([]int32, len(refs))
	for i, r := range refs {
		years[i], entries[i], nos[i] = r.FiscalYear, r.EntryID, r.LineNo
	}
	rows, _ := tx.Query(ctx, `SELECT x.ord, l.journal_entry_id IS NOT NULL, ci.line_no IS NULL, coalesce(a.reconciliation_kind IN ('supplier', 'customer'), false),
			coalesce(l.legal_entity_id, $4), coalesce(l.ledger_id, $4), coalesce(l.account_id, $4), l.partner_id,
			coalesce(l.amount_tc, 0), coalesce(l.amount_lc, 0), coalesce(l.amount_gc, 0), coalesce(e.currency_tc, ''),
			coalesce(e.currency_lc, ''), coalesce(e.currency_gc, ''), coalesce(e.rate_tc_lc::text, ''), coalesce(e.rate_lc_gc::text, '')
		FROM unnest($1::smallint[], $2::uuid[], $3::int[]) WITH ORDINALITY AS x(y, je, n, ord)
		LEFT JOIN ledger.journal_line l ON l.fiscal_year = x.y AND l.journal_entry_id = x.je AND l.line_no = x.n
		LEFT JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		LEFT JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		LEFT JOIN ledger.clearing_item ci ON ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year
			AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no
		ORDER BY x.ord`, years, entries, nos, uuid.Nil)
	items := make([]item, 0, len(refs))
	var it item
	var ord int64
	if _, err := pgx.ForEachRow(rows, []any{&ord, &it.found, &it.open, &it.recon, &it.legalEntity, &it.ledger, &it.account, &it.partner,
		&it.tc, &it.lc, &it.gc, &it.currency, &it.currencyLC, &it.currencyGC, &it.rateTCLC, &it.rateLCGC}, func() error {
		it.ref = refs[ord-1]
		items = append(items, it)
		return nil
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// markCleared records the lines as cleared by the clearing.
func markCleared(ctx context.Context, tx pgx.Tx, tenant, clearing uuid.UUID, refs []ItemRef) error {
	years, entries, nos := make([]int16, len(refs)), make([]uuid.UUID, len(refs)), make([]int32, len(refs))
	for i, r := range refs {
		years[i], entries[i], nos[i] = r.FiscalYear, r.EntryID, r.LineNo
	}
	_, err := tx.Exec(ctx, `INSERT INTO ledger.clearing_item (tenant_id, fiscal_year, journal_entry_id, line_no, clearing_id)
		SELECT $1, y, je, n, $2 FROM unnest($3::smallint[], $4::uuid[], $5::int[]) AS x(y, je, n)`, tenant, clearing, years, entries, nos)
	return err
}
