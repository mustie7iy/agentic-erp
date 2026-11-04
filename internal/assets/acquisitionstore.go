package assets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Acquisition is a stored acquisition with its asset's number and
// description, its source line or offset account, its journal entries per
// ledger and its reversal.
type Acquisition struct {
	ID, LegalEntityID, AssetID       uuid.UUID
	AssetNumber, AssetDescription    string
	Seq                              int32
	ValueDate, PostingDate           time.Time
	FiscalYear                       int16
	Currency, CurrencyLC, CurrencyGC string
	Amount, LC, GC                   money.Minor
	Source                           *Source
	OffsetAccountID                  *uuid.UUID
	Text                             *string
	Status                           string
	Reversal                         *Reversal
	Entries                          []DocumentEntry
	Version                          int32
	CreatedAt, UpdatedAt             time.Time
	CreatedBy, UpdatedBy             uuid.UUID
}

// Source is the line an acquisition capitalised, with its entry's
// document number and origin.
type Source struct {
	LineRef
	DocumentNumber, Context string
	DocumentID              *uuid.UUID
}

// Reversal is when and why an assets document was reversed.
type Reversal struct {
	PostingDate time.Time
	Reason      string
}

// DocumentEntry is one ledger's journal entry of an assets document, its
// main one or a center transfer's catch-up (Part), with its reversal entry
// once reversed.
type DocumentEntry struct {
	LedgerID, EntryID uuid.UUID
	Part              string
	FiscalYear        int16
	DocumentNumber    string
	Reversal          *ReversalEntry
}

// Parts of a document's entries in a ledger (ADR-0057 §5): its main entry,
// and a center transfer's catch-up posted in the period before.
const (
	partMain    = "main"
	partCatchUp = "catch_up"
)

// clearingUse is what is used of a capitalised line (assets.clearing_use).
type clearingUse struct {
	currency   string
	line, used share
}

// lockUse reads what is used of the line FOR UPDATE, after inserting its
// row with nothing used when it has none, so acquisitions from one line
// serialise (ADR-0057 §5). A reversal's line's row names the line it
// reverses, so the worklist finds what assets took off through it.
func lockUse(ctx context.Context, tx pgx.Tx, l Line, reverses *LineRef) (clearingUse, error) {
	var year *int16
	var entry *uuid.UUID
	if reverses != nil {
		year, entry = &reverses.FiscalYear, &reverses.EntryID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.clearing_use (tenant_id, entry_year, entry_id, line_no, legal_entity_id, currency, line_amount, line_lc, line_gc,
			reverses_entry_year, reverses_entry_id)
		VALUES (platform.current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
		l.FiscalYear, l.EntryID, l.LineNo, l.LegalEntityID, l.Currency, l.Amount, l.LC, l.GC, year, entry); err != nil {
		return clearingUse{}, fmt.Errorf("assets: use of line %d of entry %s: %w", l.LineNo, l.EntryID, err)
	}
	var u clearingUse
	err := tx.QueryRow(ctx, `SELECT currency, line_amount, line_lc, line_gc, used, used_lc, used_gc FROM assets.clearing_use
		WHERE entry_year = $1 AND entry_id = $2 AND line_no = $3 FOR UPDATE`, l.FiscalYear, l.EntryID, l.LineNo).
		Scan(&u.currency, &u.line.tc, &u.line.lc, &u.line.gc, &u.used.tc, &u.used.lc, &u.used.gc)
	return u, errs.Wrapf(err, "assets: lock the use of line %d of entry %s", l.LineNo, l.EntryID)
}

// costStaysPositive refuses a negative acquisition that would leave the
// asset's cost below zero in a ledger, or one onto an asset holding no
// cost (422 assets.cost_negative, ADR-0057 §6).
func costStaysPositive(ctx context.Context, tx pgx.Tx, a acquiring) error {
	if a.amount.tc > 0 {
		return nil
	}
	var short bool
	err := tx.QueryRow(ctx, `SELECT count(*) = 0 OR bool_or(c + $2 < 0) FROM (
			SELECT sum(cost) AS c FROM assets.movement WHERE asset_id = $1 GROUP BY ledger_id) x`, a.asset.ID, a.amount.lc).Scan(&short)
	if err != nil {
		return fmt.Errorf("assets: cost of asset %s: %w", a.asset.Number, err)
	}
	if short {
		return invalid("assets.cost_negative", fmt.Sprintf("A reduction of %s minor units would leave asset %s's cost below zero in a ledger.",
			a.amount.lc.String(), a.asset.Number), "amount", "at most the asset's cost")
	}
	return nil
}

// latestAssignment is the day the asset's latest assignment took effect
// (-infinity for its first, read as the zero time).
func latestAssignment(ctx context.Context, tx pgx.Tx, asset uuid.UUID) (time.Time, error) {
	var from *time.Time
	err := tx.QueryRow(ctx, `SELECT CASE WHEN valid_from = '-infinity' THEN NULL ELSE valid_from END FROM assets.asset_assignment
		WHERE asset_id = $1 ORDER BY seq DESC LIMIT 1`, asset).Scan(&from)
	if err != nil || from == nil {
		return time.Time{}, errs.Wrapf(err, "assets: latest assignment of asset %s", asset)
	}
	return *from, nil
}

// writeAcquisition writes the acquisition with the asset's next seq, what
// it used of its line, its movements and entries per ledger, and the
// asset's status and capitalisation date derived again (ADR-0057 §5).
func writeAcquisition(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, seq int32, a acquiring, text *string, entries []PostedEntry) error {
	// The source columns are null together for a manual acquisition, the
	// offset account for one from a line (acquisition_source).
	var src [6]any
	var offset *uuid.UUID
	if l := a.line; a.offset == nil {
		src = [6]any{l.FiscalYear, l.EntryID, l.LineNo, l.DocumentNumber, l.SourceContext, l.SourceDocumentID}
	} else {
		offset = &a.offset.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.acquisition (tenant_id, id, legal_entity_id, asset_id, seq, value_date, posting_date, fiscal_year, currency,
			amount, amount_lc, amount_gc, currency_lc, currency_gc, source_entry_year, source_entry_id, source_line_no, source_document_number, source_context,
			source_document_id, offset_account_id, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, 'posted', $23, $23)`,
		p.TenantID, id, a.company.ID, a.asset.ID, seq, a.value, a.posting, entries[0].FiscalYear, a.currency, a.amount.tc, a.amount.lc, a.amount.gc,
		a.currencyLC, a.currencyGC, src[0], src[1], src[2], src[3], src[4], src[5], offset, text, p.ID); err != nil {
		return fmt.Errorf("assets: insert acquisition of asset %s: %w", a.asset.Number, err)
	}
	if a.offset == nil {
		if err := useLine(ctx, tx, LineRef{FiscalYear: a.line.FiscalYear, EntryID: a.line.EntryID, LineNo: a.line.LineNo}, a.amount); err != nil {
			return err
		}
	}
	if err := writeMovements(ctx, tx, p, id, seq, a, entries); err != nil {
		return err
	}
	if err := derive(ctx, tx, p, a.asset.ID, seq); err != nil {
		return err
	}
	return keepTotals(ctx, tx, a.asset.ID)
}

// useLine adds what an acquisition takes of its line to what is used of
// it; a reversal gives it back with the amounts negated.
func useLine(ctx context.Context, tx pgx.Tx, l LineRef, amount share) error {
	_, err := tx.Exec(ctx, `UPDATE assets.clearing_use SET used = used + $4, used_lc = used_lc + $5, used_gc = used_gc + $6, version = version + 1
		WHERE entry_year = $1 AND entry_id = $2 AND line_no = $3`, l.FiscalYear, l.EntryID, l.LineNo, amount.tc, amount.lc, amount.gc)
	return errs.Wrapf(err, "assets: use line %d of entry %s", l.LineNo, l.EntryID)
}

// writeMovements writes an acquisition's movement and journal entry per
// ledger.
func writeMovements(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, seq int32, a acquiring, entries []PostedEntry) error {
	for _, en := range entries {
		if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
				posting_date, fiscal_year, fiscal_period, cost, cost_gc, document_kind, document_id, journal_entry_year, journal_entry_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'acquisition', $8, $9, $10, $11, $12, $13, 'acquisition', $14, $10, $15, $16)`,
			p.TenantID, uuid.Must(uuid.NewV7()), a.asset.ID, seq, a.company.ID, en.LedgerID, a.asset.Class, a.value, a.posting, en.FiscalYear, en.Period,
			a.amount.lc, a.amount.gc, id, en.ID, p.ID); err != nil {
			return fmt.Errorf("assets: movement of asset %s in ledger %s: %w", a.asset.Number, en.LedgerID, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO assets.document_entry (tenant_id, document_kind, document_id, ledger_id, part, journal_entry_year,
				journal_entry_id, document_number) VALUES ($1, 'acquisition', $2, $3, 'main', $4, $5, $6)`,
			p.TenantID, id, en.LedgerID, en.FiscalYear, en.ID, en.DocumentNumber); err != nil {
			return fmt.Errorf("assets: entry of acquisition %s in ledger %s: %w", id, en.LedgerID, err)
		}
	}
	return nil
}

// derive sets the asset's last seq and derives its status and
// capitalisation date (ADR-0057 §5): planned while it holds no posted
// acquisition that adds cost (a reduction, from a credit line,
// capitalises nothing) and no posted asset transfer in, with the earliest
// one's value date otherwise; retired when its cost is zero in every
// ledger and its latest movement that reduced its cost or took a full
// share, a reversed one and its reversal set aside, is a retirement or
// transfer out (decided by default 21: a credit memo bringing the cost to
// zero leaves it active); active otherwise.
func derive(ctx context.Context, tx pgx.Tx, p reqctx.Principal, asset uuid.UUID, seq int32) error {
	_, err := tx.Exec(ctx, `UPDATE assets.asset a SET last_seq = $2, capitalised_on = x.on,
			status = CASE WHEN x.on IS NULL THEN 'planned' WHEN y.retired THEN 'retired' ELSE 'active' END, version = a.version + 1, updated_by = $3
		FROM (SELECT least((SELECT min(value_date) FROM assets.acquisition WHERE asset_id = $1 AND status = 'posted' AND amount > 0),
				(SELECT min(value_date) FROM assets.transfer WHERE to_asset_id = $1 AND status = 'posted')) AS on) x,
			(SELECT coalesce((SELECT bool_and(c = 0) FROM (SELECT sum(cost) AS c FROM assets.movement WHERE asset_id = $1 GROUP BY ledger_id) s), false)
				AND coalesce((SELECT m.kind IN ('retirement', 'transfer_out') FROM assets.movement m
					WHERE m.asset_id = $1 AND m.kind <> 'depreciation' AND m.reverses_id IS NULL AND (m.cost < 0 OR m.share = 1)
						AND NOT EXISTS (SELECT FROM assets.movement r WHERE r.tenant_id = m.tenant_id AND r.reverses_id = m.id)
					ORDER BY m.seq DESC LIMIT 1), false) AS retired) y
		WHERE a.id = $1`, asset, seq, p.ID)
	return errs.Wrapf(err, "assets: derive asset %s", asset)
}

// acquisitionSelect reads acquisitions with their assets' numbers and
// descriptions.
const acquisitionSelect = `SELECT q.id, q.legal_entity_id, q.asset_id, a.asset_number, a.description, q.seq, q.value_date, q.posting_date, q.fiscal_year,
		q.currency, q.currency_lc, q.currency_gc, q.amount, q.amount_lc, q.amount_gc, q.source_entry_year, q.source_entry_id, q.source_line_no,
		q.source_document_number, q.source_context, q.source_document_id, q.offset_account_id, q.text, q.status, q.reversal_posting_date, q.reversal_reason,
		q.version, q.created_at, q.created_by, q.updated_at, q.updated_by
	FROM assets.acquisition q JOIN assets.asset a ON a.tenant_id = q.tenant_id AND a.id = q.asset_id`

func scanAcquisition(r pgx.Row) (Acquisition, error) {
	var q Acquisition
	var year *int16
	var entry *uuid.UUID
	var line *int32
	var number, context *string
	var doc *uuid.UUID
	var reversed *time.Time
	var reason *string
	err := r.Scan(&q.ID, &q.LegalEntityID, &q.AssetID, &q.AssetNumber, &q.AssetDescription, &q.Seq, &q.ValueDate, &q.PostingDate, &q.FiscalYear,
		&q.Currency, &q.CurrencyLC, &q.CurrencyGC, &q.Amount, &q.LC, &q.GC, &year, &entry, &line, &number, &context, &doc, &q.OffsetAccountID, &q.Text,
		&q.Status, &reversed, &reason, &q.Version, &q.CreatedAt, &q.CreatedBy, &q.UpdatedAt, &q.UpdatedBy)
	if err == nil && entry != nil { // the source columns are null together (acquisition_source)
		q.Source = &Source{LineRef: LineRef{FiscalYear: *year, EntryID: *entry, LineNo: *line}, DocumentNumber: *number, Context: *context, DocumentID: doc}
	}
	if err == nil && reversed != nil { // set together with the status (acquisition_reversal)
		q.Reversal = &Reversal{PostingDate: *reversed, Reason: *reason}
	}
	return q, errs.Wrapf(err, "scan acquisition")
}

// readAcquisition reads an acquisition of a legal entity the caller may
// act in, with its entries; any other is not found.
func readAcquisition(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Acquisition, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Acquisition{}, err
	}
	q, err := scanAcquisition(tx.QueryRow(ctx, acquisitionSelect+` WHERE q.id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(q.LegalEntityID)):
		return Acquisition{}, problem.NotFoundOf("asset acquisition")
	case err != nil:
		return Acquisition{}, fmt.Errorf("assets: acquisition %s: %w", id, err)
	}
	entries, err := readEntries(ctx, tx, "acquisition", []uuid.UUID{id})
	q.Entries = entries[id]
	return q, err
}

// readEntries reads the journal entries of documents of a kind, by
// document, each with its part (a center transfer's catch-up before its
// main entry) and its reversal entry once reversed.
func readEntries(ctx context.Context, tx pgx.Tx, kind string, ids []uuid.UUID) (map[uuid.UUID][]DocumentEntry, error) {
	rows, _ := tx.Query(ctx, `SELECT document_id, ledger_id, part, journal_entry_year, journal_entry_id, document_number, reversal_entry_year,
			reversal_entry_id, reversal_document_number
		FROM assets.document_entry WHERE document_kind = $1 AND document_id = ANY($2) ORDER BY part`, kind, ids)
	out := map[uuid.UUID][]DocumentEntry{}
	var doc uuid.UUID
	var en DocumentEntry
	var rev nullableReversal
	_, err := pgx.ForEachRow(rows, []any{&doc, &en.LedgerID, &en.Part, &en.FiscalYear, &en.EntryID, &en.DocumentNumber, &rev.year, &rev.id, &rev.number},
		func() error {
			en.Reversal = rev.entry()
			out[doc] = append(out[doc], en)
			return nil
		})
	return out, errs.Wrapf(err, "assets: entries of %d documents", len(ids))
}

// nullableReversal is a document entry's reversal entry, all null until
// it is reversed (document_entry_reversal, document_entry_reversal_number).
type nullableReversal struct {
	year   *int16
	id     *uuid.UUID
	number *string
}

func (n nullableReversal) entry() *ReversalEntry {
	if n.id == nil {
		return nil
	}
	return &ReversalEntry{FiscalYear: *n.year, EntryID: *n.id, DocumentNumber: *n.number}
}

// recordAcquisition audits the acquisition and publishes it as
// AssetMovementPosted, with the asset's status and version after it.
func recordAcquisition(ctx context.Context, tx pgx.Tx, q Acquisition) error {
	asset, ledgers, err := eventLedgers(ctx, tx, q, 1, func(en DocumentEntry) map[string]any {
		return map[string]any{"journal_entry_id": en.EntryID, "fiscal_year": en.FiscalYear, "document_number": en.DocumentNumber}
	})
	if err != nil {
		return err
	}
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": q.Seq, "value_date": q.ValueDate.Format(time.DateOnly),
		"posting_date": q.PostingDate.Format(time.DateOnly), "currency": q.Currency, "amount": int64(q.Amount), "amount_lc": int64(q.LC),
		"amount_gc": int64(q.GC), "ledgers": ledgers}
	if q.Source != nil {
		detail["source_entry_id"], detail["source_line_no"] = q.Source.EntryID, q.Source.LineNo
	} else {
		detail["offset_account_id"] = q.OffsetAccountID
	}
	return audit.Record(ctx, "assets.acquisition.post", "assets.acquisition", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_posted", AggregateType: "assets.acquisition", AggregateID: q.ID,
			Payload: map[string]any{"document_kind": "acquisition", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
				"asset_number": q.AssetNumber, "class_code": asset.Class, "value_date": q.ValueDate.Format(time.DateOnly),
				"posting_date": q.PostingDate.Format(time.DateOnly), "currency_lc": q.CurrencyLC, "currency_gc": q.CurrencyGC, "ledgers": ledgers,
				"asset_status": asset.Status, "asset_version": asset.Version}})
}

// eventLedgers reads the asset after an acquisition's posting or reversal
// and, per ledger, the change of cost it made (its amounts times sign) with
// the entry fields of each ledger's entry.
func eventLedgers(ctx context.Context, tx pgx.Tx, q Acquisition, sign money.Minor, entry func(DocumentEntry) map[string]any) (Asset, []map[string]any, error) {
	asset, err := readAsset(ctx, tx, q.AssetID, false)
	if err != nil {
		return Asset{}, nil, err
	}
	expLC, err := currency.Exponent(ctx, tx, q.CurrencyLC, nil)
	if err != nil {
		return Asset{}, nil, fmt.Errorf("assets: currency %s: %w", q.CurrencyLC, err)
	}
	expGC, err := currency.Exponent(ctx, tx, q.CurrencyGC, nil)
	if err != nil {
		return Asset{}, nil, fmt.Errorf("assets: currency %s: %w", q.CurrencyGC, err)
	}
	ledgers := make([]map[string]any, len(q.Entries))
	for i, en := range q.Entries {
		ledgers[i] = entry(en)
		ledgers[i]["ledger_id"] = en.LedgerID
		ledgers[i]["cost"], ledgers[i]["cost_gc"] = money.EventAmount(sign*q.LC, q.CurrencyLC, expLC), money.EventAmount(sign*q.GC, q.CurrencyGC, expGC)
	}
	return asset, ledgers, nil
}
