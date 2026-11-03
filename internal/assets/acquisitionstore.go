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
// description, its source line and its journal entries per ledger.
type Acquisition struct {
	ID, LegalEntityID, AssetID       uuid.UUID
	AssetNumber, AssetDescription    string
	Seq                              int32
	ValueDate, PostingDate           time.Time
	FiscalYear                       int16
	Currency, CurrencyLC, CurrencyGC string
	Amount, LC, GC                   money.Minor
	Source                           *Source
	Text                             *string
	Status                           string
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

// DocumentEntry is one ledger's journal entry of an assets document.
type DocumentEntry struct {
	LedgerID, EntryID uuid.UUID
	FiscalYear        int16
	DocumentNumber    string
}

// clearingUse is what is used of a capitalised line (assets.clearing_use).
type clearingUse struct {
	currency   string
	line, used share
}

// lockUse reads what is used of the line FOR UPDATE, after inserting its
// row with nothing used when it has none, so acquisitions from one line
// serialise (ADR-0057 §5).
func lockUse(ctx context.Context, tx pgx.Tx, l Line) (clearingUse, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO assets.clearing_use (tenant_id, entry_year, entry_id, line_no, legal_entity_id, currency, line_amount, line_lc, line_gc)
		VALUES (platform.current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		l.FiscalYear, l.EntryID, l.LineNo, l.LegalEntityID, l.Currency, l.Amount, l.LC, l.GC); err != nil {
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
	l := a.line
	if _, err := tx.Exec(ctx, `INSERT INTO assets.acquisition (tenant_id, id, legal_entity_id, asset_id, seq, value_date, posting_date, fiscal_year, currency,
			amount, amount_lc, amount_gc, currency_lc, currency_gc, source_entry_year, source_entry_id, source_line_no, source_document_number, source_context,
			source_document_id, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, 'posted', $22, $22)`,
		p.TenantID, id, a.company.ID, a.asset.ID, seq, a.value, a.posting, entries[0].FiscalYear, l.Currency, a.amount.tc, a.amount.lc, a.amount.gc,
		l.CurrencyLC, l.CurrencyGC, l.FiscalYear, l.EntryID, l.LineNo, l.DocumentNumber, l.SourceContext, l.SourceDocumentID, text, p.ID); err != nil {
		return fmt.Errorf("assets: insert acquisition of asset %s: %w", a.asset.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE assets.clearing_use SET used = used + $4, used_lc = used_lc + $5, used_gc = used_gc + $6, version = version + 1
		WHERE entry_year = $1 AND entry_id = $2 AND line_no = $3`, l.FiscalYear, l.EntryID, l.LineNo, a.amount.tc, a.amount.lc, a.amount.gc); err != nil {
		return fmt.Errorf("assets: use line %d of entry %s: %w", l.LineNo, l.EntryID, err)
	}
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
	return derive(ctx, tx, p, a.asset.ID, seq)
}

// derive sets the asset's last seq and derives its status and
// capitalisation date from its posted acquisitions (ADR-0057 §5): planned
// while it holds none, active with the earliest value date otherwise.
// Retirements and transfers in join the rule with their slices (T57g,
// T57h).
func derive(ctx context.Context, tx pgx.Tx, p reqctx.Principal, asset uuid.UUID, seq int32) error {
	_, err := tx.Exec(ctx, `UPDATE assets.asset a SET last_seq = $2, capitalised_on = x.on, status = CASE WHEN x.on IS NULL THEN 'planned' ELSE 'active' END,
			version = a.version + 1, updated_by = $3
		FROM (SELECT min(value_date) AS on FROM assets.acquisition WHERE asset_id = $1 AND status = 'posted') x WHERE a.id = $1`, asset, seq, p.ID)
	return errs.Wrapf(err, "assets: derive asset %s", asset)
}

// acquisitionSelect reads acquisitions with their assets' numbers and
// descriptions.
const acquisitionSelect = `SELECT q.id, q.legal_entity_id, q.asset_id, a.asset_number, a.description, q.seq, q.value_date, q.posting_date, q.fiscal_year,
		q.currency, q.currency_lc, q.currency_gc, q.amount, q.amount_lc, q.amount_gc, q.source_entry_year, q.source_entry_id, q.source_line_no,
		q.source_document_number, q.source_context, q.source_document_id, q.text, q.status, q.version, q.created_at, q.created_by, q.updated_at, q.updated_by
	FROM assets.acquisition q JOIN assets.asset a ON a.tenant_id = q.tenant_id AND a.id = q.asset_id`

func scanAcquisition(r pgx.Row) (Acquisition, error) {
	var q Acquisition
	var year *int16
	var entry *uuid.UUID
	var line *int32
	var number, context *string
	var doc *uuid.UUID
	err := r.Scan(&q.ID, &q.LegalEntityID, &q.AssetID, &q.AssetNumber, &q.AssetDescription, &q.Seq, &q.ValueDate, &q.PostingDate, &q.FiscalYear,
		&q.Currency, &q.CurrencyLC, &q.CurrencyGC, &q.Amount, &q.LC, &q.GC, &year, &entry, &line, &number, &context, &doc, &q.Text, &q.Status,
		&q.Version, &q.CreatedAt, &q.CreatedBy, &q.UpdatedAt, &q.UpdatedBy)
	if err == nil && entry != nil { // the source columns are null together (acquisition_source)
		q.Source = &Source{LineRef: LineRef{FiscalYear: *year, EntryID: *entry, LineNo: *line}, DocumentNumber: *number, Context: *context, DocumentID: doc}
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

// readEntries reads the main journal entries of documents of a kind, by
// document.
func readEntries(ctx context.Context, tx pgx.Tx, kind string, ids []uuid.UUID) (map[uuid.UUID][]DocumentEntry, error) {
	rows, _ := tx.Query(ctx, `SELECT document_id, ledger_id, journal_entry_year, journal_entry_id, document_number FROM assets.document_entry
		WHERE document_kind = $1 AND document_id = ANY($2) AND part = 'main'`, kind, ids)
	out := map[uuid.UUID][]DocumentEntry{}
	var doc uuid.UUID
	var en DocumentEntry
	_, err := pgx.ForEachRow(rows, []any{&doc, &en.LedgerID, &en.FiscalYear, &en.EntryID, &en.DocumentNumber}, func() error {
		out[doc] = append(out[doc], en)
		return nil
	})
	return out, errs.Wrapf(err, "assets: entries of %d documents", len(ids))
}

// recordAcquisition audits the acquisition and publishes it as
// AssetMovementPosted, with the asset's status and version after it.
func recordAcquisition(ctx context.Context, tx pgx.Tx, q Acquisition, a acquiring) error {
	asset, err := readAsset(ctx, tx, q.AssetID, false)
	if err != nil {
		return err
	}
	expLC, err := currency.Exponent(ctx, tx, q.CurrencyLC, nil)
	if err != nil {
		return fmt.Errorf("assets: currency %s: %w", q.CurrencyLC, err)
	}
	expGC, err := currency.Exponent(ctx, tx, q.CurrencyGC, nil)
	if err != nil {
		return fmt.Errorf("assets: currency %s: %w", q.CurrencyGC, err)
	}
	ledgers := make([]map[string]any, len(q.Entries))
	for i, en := range q.Entries {
		ledgers[i] = map[string]any{"ledger_id": en.LedgerID, "cost": money.EventAmount(q.LC, q.CurrencyLC, expLC),
			"cost_gc": money.EventAmount(q.GC, q.CurrencyGC, expGC), "journal_entry_id": en.EntryID, "fiscal_year": en.FiscalYear,
			"document_number": en.DocumentNumber}
	}
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": q.Seq, "value_date": q.ValueDate.Format(time.DateOnly),
		"posting_date": q.PostingDate.Format(time.DateOnly), "currency": q.Currency, "amount": int64(q.Amount), "amount_lc": int64(q.LC),
		"amount_gc": int64(q.GC), "source_entry_id": a.line.EntryID, "source_line_no": a.line.LineNo, "ledgers": ledgers}
	return audit.Record(ctx, "assets.acquisition.post", "assets.acquisition", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_posted", AggregateType: "assets.acquisition", AggregateID: q.ID,
			Payload: map[string]any{"document_kind": "acquisition", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
				"asset_number": q.AssetNumber, "class_code": asset.Class, "value_date": q.ValueDate.Format(time.DateOnly),
				"posting_date": q.PostingDate.Format(time.DateOnly), "currency_lc": q.CurrencyLC, "currency_gc": q.CurrencyGC, "ledgers": ledgers,
				"asset_status": asset.Status, "asset_version": asset.Version}})
}
