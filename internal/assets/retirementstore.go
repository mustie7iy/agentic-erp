package assets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Retirement is a stored retirement with its asset's number and
// description, what it took per ledger, its journal entries per ledger and
// its reversal.
type Retirement struct {
	ID, LegalEntityID, AssetID    uuid.UUID
	AssetNumber, AssetDescription string
	Seq                           int32
	Kind                          string
	ValueDate, PostingDate        time.Time
	FiscalYear, Period            int16
	Currency, CurrencyGC          string
	SharePercent                  *string
	ShareCost                     *money.Minor
	Proceeds, ProceedsGC          money.Minor
	Text                          *string
	Status                        string
	Reversal                      *Reversal
	Ledgers                       []RetiredValues
	Entries                       []DocumentEntry
	Version                       int32
	CreatedAt, UpdatedAt          time.Time
	CreatedBy, UpdatedBy          uuid.UUID
}

// RetiredValues are what a retirement took in one ledger, from its
// movements there: the fraction of cost, the catch-up (the depreciation it
// posted, an expense debit positive), and the cost (positive), accumulated
// depreciation (negative, the credit it was) and residual value taken, in
// local and group currency.
type RetiredValues struct {
	LedgerID                                 uuid.UUID
	Share                                    string
	CatchUp, CatchUpGC                       money.Minor
	Cost, CostGC, Accumulated, AccumulatedGC money.Minor
	Residual                                 money.Minor
}

// writeRetirement writes the retirement with the asset's seq, per ledger
// its catch-up's depreciation movement (always, with the asset's
// booked_through before it, ADR-0057 §5) and its retirement movement on
// the ledger's entry (none when the ledger posted nothing) and the entry,
// raises booked_through to the period before V unless it is V, and
// derives the asset's status again.
func writeRetirement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, seq int32, r *retiring) error {
	var percent *string
	if r.in.Percent != nil {
		s := fmt.Sprintf("%d.%04d", *r.in.Percent/10_000, *r.in.Percent%10_000) // ten-thousandths of a percent as a plain decimal
		percent = &s
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.retirement (tenant_id, id, legal_entity_id, asset_id, seq, kind, value_date, posting_date, fiscal_year,
			fiscal_period, currency, share_percent, share_cost, proceeds, currency_gc, proceeds_gc, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13, $14, $15, $16, $17, 'posted', $18, $18)`,
		p.TenantID, id, r.company.ID, r.asset.ID, seq, r.in.Kind, r.value, r.posting, r.period.FiscalYear, r.period.Number, r.company.Currency, percent,
		r.in.Cost, r.in.Proceeds, r.group, r.proceedsGC, r.in.Text, p.ID); err != nil {
		return fmt.Errorf("assets: insert retirement of asset %s: %w", r.asset.Number, err)
	}
	for _, l := range r.ledgers {
		if err := writeRetiredLedger(ctx, tx, p, id, seq, r, l); err != nil {
			return err
		}
	}
	return derive(ctx, tx, p, r.asset.ID, seq)
}

// writeRetiredLedger writes a retirement's movements and entry in one
// ledger and raises the asset's booked_through there.
func writeRetiredLedger(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, seq int32, r *retiring, l *retiredLedger) error {
	year, period := r.period.FiscalYear, r.period.Number
	var entryYear *int16
	var entry *uuid.UUID
	if l.entry != nil {
		year, period, entryYear, entry = l.entry.FiscalYear, l.entry.Period, &l.entry.FiscalYear, &l.entry.ID
	}
	share := l.share.FloatString(12)
	if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
			posting_date, fiscal_year, fiscal_period, cost, cost_gc, accumulated, accumulated_gc, residual, proceeds, share, document_kind, document_id,
			journal_entry_year, journal_entry_id, booked_through_before_year, booked_through_before_period, created_by)
		VALUES ($1, $2, $4, $5, $6, $7, $8, 'depreciation', $9, $10, $11, $12, 0, 0, $13, $14, 0, 0, NULL, 'retirement', $3, $15, $16, $17, $18, $19),
			($1, $20, $4, $5, $6, $7, $8, 'retirement', $9, $10, $11, $12, $21, $22, $23, $24, $25, $26, $27::numeric, 'retirement', $3, $15, $16, NULL, NULL, $19)`,
		p.TenantID, uuid.Must(uuid.NewV7()), id, r.asset.ID, seq, r.company.ID, l.ledger.ID, r.asset.Class, r.value, r.posting, year, period,
		-l.catchUp, -l.catchUpGC, entryYear, entry, l.terms.BookedYear, l.terms.BookedPeriod, p.ID, uuid.Must(uuid.NewV7()),
		-l.cost, -l.costGC, -l.accumulated, -l.accumulatedGC, -l.residual, r.in.Proceeds, share); err != nil {
		return fmt.Errorf("assets: movements of asset %s in ledger %s: %w", r.asset.Number, l.ledger.Code, err)
	}
	if l.entry != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO assets.document_entry (tenant_id, document_kind, document_id, ledger_id, part, journal_entry_year,
				journal_entry_id, document_number) VALUES ($1, 'retirement', $2, $3, 'main', $4, $5, $6)`,
			p.TenantID, id, l.ledger.ID, l.entry.FiscalYear, l.entry.ID, l.entry.DocumentNumber); err != nil {
			return fmt.Errorf("assets: entry of retirement %s in ledger %s: %w", id, l.ledger.Code, err)
		}
	}
	_, err := tx.Exec(ctx, `UPDATE assets.asset_terms SET booked_through_year = $3, booked_through_period = $4, version = version + 1, updated_by = $5
		WHERE asset_id = $1 AND ledger_id = $2 AND (booked_through_year IS NULL OR (booked_through_year, booked_through_period) < ($3, $4))`,
		r.asset.ID, l.ledger.ID, r.before.year, r.before.period, p.ID)
	return errs.Wrapf(err, "assets: book asset %s through %d/%d in ledger %s", r.asset.Number, r.before.year, r.before.period, l.ledger.Code)
}

// retirementSelect reads retirements with their assets' numbers and
// descriptions.
const retirementSelect = `SELECT q.id, q.legal_entity_id, q.asset_id, a.asset_number, a.description, q.seq, q.kind, q.value_date, q.posting_date,
		q.fiscal_year, q.fiscal_period, q.currency, q.currency_gc, trim_scale(q.share_percent)::text, q.share_cost, q.proceeds, q.proceeds_gc, q.text,
		q.status, q.reversal_posting_date, q.reversal_reason, q.version, q.created_at, q.created_by, q.updated_at, q.updated_by
	FROM assets.retirement q JOIN assets.asset a ON a.tenant_id = q.tenant_id AND a.id = q.asset_id`

func scanRetirement(r pgx.Row) (Retirement, error) {
	var q Retirement
	var reversed *time.Time
	var reason *string
	err := r.Scan(&q.ID, &q.LegalEntityID, &q.AssetID, &q.AssetNumber, &q.AssetDescription, &q.Seq, &q.Kind, &q.ValueDate, &q.PostingDate,
		&q.FiscalYear, &q.Period, &q.Currency, &q.CurrencyGC, &q.SharePercent, &q.ShareCost, &q.Proceeds, &q.ProceedsGC, &q.Text, &q.Status,
		&reversed, &reason, &q.Version, &q.CreatedAt, &q.CreatedBy, &q.UpdatedAt, &q.UpdatedBy)
	if err == nil && reversed != nil { // set together with the status (retirement_reversal)
		q.Reversal = &Reversal{PostingDate: *reversed, Reason: *reason}
	}
	return q, errs.Wrapf(err, "scan retirement")
}

// readRetirement reads a retirement of a legal entity the caller may act
// in, with what it took per ledger and its entries; any other is not
// found.
func readRetirement(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Retirement, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Retirement{}, err
	}
	q, err := scanRetirement(tx.QueryRow(ctx, retirementSelect+` WHERE q.id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(q.LegalEntityID)):
		return Retirement{}, problem.NotFoundOf("asset retirement")
	case err != nil:
		return Retirement{}, fmt.Errorf("assets: retirement %s: %w", id, err)
	}
	qs := []Retirement{q}
	err = retirementDetails(ctx, tx, qs)
	return qs[0], err
}

// retirementDetails reads what the retirements took per ledger and their
// entries into them.
func retirementDetails(ctx context.Context, tx pgx.Tx, qs []Retirement) error {
	ids := make([]uuid.UUID, len(qs))
	for i, q := range qs {
		ids[i] = q.ID
	}
	entries, err := readEntries(ctx, tx, "retirement", ids)
	if err != nil {
		return err
	}
	rows, _ := tx.Query(ctx, `SELECT document_id, ledger_id, trim_scale(max(share))::text, -sum(accumulated) FILTER (WHERE kind = 'depreciation'),
			-sum(accumulated_gc) FILTER (WHERE kind = 'depreciation'), -sum(cost), -sum(cost_gc), -sum(accumulated) FILTER (WHERE kind = 'retirement'),
			-sum(accumulated_gc) FILTER (WHERE kind = 'retirement'), -sum(residual)
		FROM assets.movement WHERE document_kind = 'retirement' AND document_id = ANY($1) AND reverses_id IS NULL GROUP BY document_id, ledger_id`, ids)
	values := map[uuid.UUID][]RetiredValues{}
	var doc uuid.UUID
	var v RetiredValues
	_, err = pgx.ForEachRow(rows, []any{&doc, &v.LedgerID, &v.Share, &v.CatchUp, &v.CatchUpGC, &v.Cost, &v.CostGC, &v.Accumulated, &v.AccumulatedGC,
		&v.Residual}, func() error {
		values[doc] = append(values[doc], v)
		return nil
	})
	for i := range qs {
		qs[i].Entries, qs[i].Ledgers = entries[qs[i].ID], values[qs[i].ID]
	}
	return errs.Wrapf(err, "assets: values of %d retirements", len(ids))
}
