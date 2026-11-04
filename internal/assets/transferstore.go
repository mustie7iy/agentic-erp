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

// Transfer is a stored transfer with its asset's (and an asset transfer's
// receiver's) number and description, what it moved per ledger, its
// journal entries per ledger and part, and its reversal.
type Transfer struct {
	ID, LegalEntityID, AssetID                 uuid.UUID
	AssetNumber, AssetDescription              string
	Seq                                        int32
	Kind                                       string
	ToAssetID                                  *uuid.UUID
	ToSeq                                      *int32
	ToAssetNumber, ToAssetDescription, ToClass *string
	ValueDate, PostingDate                     time.Time
	FiscalYear, Period                         int16
	Currency, CurrencyGC                       string
	SharePercent                               *string
	ShareCost                                  *money.Minor
	FromCostCenterID, FromProfitCenterID       *uuid.UUID
	CostCenterID, ProfitCenterID               *uuid.UUID
	Text                                       *string
	Status                                     string
	Reversal                                   *Reversal
	Ledgers                                    []TransferValues
	Entries                                    []DocumentEntry
	Version                                    int32
	CreatedAt, UpdatedAt                       time.Time
	CreatedBy, UpdatedBy                       uuid.UUID
}

// TransferValues are what a transfer moved in one ledger: its catch-up
// (an expense debit positive) in local and group currency with the date
// and period it was posted in, and the cost (positive), accumulated
// depreciation (negative, the credit it is) and residual value an asset
// transfer took to the receiver, with the fraction of cost (Share), or the
// balances a center transfer moved to the new profit center.
type TransferValues struct {
	LedgerID                                 uuid.UUID
	Share                                    *string
	CatchUp, CatchUpGC                       money.Minor
	CatchUpOn                                time.Time
	CatchUpIn                                postingPeriod
	Cost, CostGC, Accumulated, AccumulatedGC money.Minor
	Residual                                 money.Minor
}

// profitCenterChange reports whether a center transfer moved the asset's
// balances to another profit center.
func (q Transfer) profitCenterChange() bool {
	return q.Kind == TransferCenter && !sameID(q.FromProfitCenterID, q.ProfitCenterID)
}

// writeTransfer writes the transfer with the asset's seq (and the
// receiver's), per ledger its movements and entries and the asset's
// booked_through, the assets' status derived again, and a center
// transfer's assignment row from its value date, taking the transfer's
// seq once derive has set it as the asset's last (asset_assignment_seq).
func writeTransfer(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, t *transferring) error {
	seq, toSeq, toAsset := t.asset.LastSeq+1, (*int32)(nil), (*uuid.UUID)(nil)
	var from, fromPC *uuid.UUID
	if t.in.Kind == TransferAsset {
		s := t.to.LastSeq + 1
		toSeq, toAsset = &s, &t.to.ID
	} else {
		from, fromPC = t.asset.CostCenterID, t.asset.ProfitCenterID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.transfer (tenant_id, id, legal_entity_id, asset_id, seq, kind, value_date, posting_date, fiscal_year,
			fiscal_period, currency, currency_gc, to_asset_id, to_seq, share_percent, share_cost, from_cost_center_id, from_profit_center_id, cost_center_id,
			profit_center_id, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::numeric, $16, $17, $18, $19, $20, $21, 'posted', $22, $22)`,
		p.TenantID, id, t.company.ID, t.asset.ID, seq, t.in.Kind, t.value, t.posting, t.period.FiscalYear, t.period.Number, t.company.Currency, t.group,
		toAsset, toSeq, percentOf(t.in.Percent), t.in.Cost, from, fromPC, t.costCenter, t.profitOrNil(), t.in.Text, p.ID); err != nil {
		return fmt.Errorf("assets: insert transfer of asset %s: %w", t.asset.Number, err)
	}
	for _, l := range t.ledgers {
		if err := writeTransferLedger(ctx, tx, p, id, seq, toSeq, t, l); err != nil {
			return err
		}
	}
	if err := derive(ctx, tx, p, t.asset.ID, seq); err != nil {
		return err
	}
	if toSeq != nil {
		if err := derive(ctx, tx, p, t.to.ID, *toSeq); err != nil {
			return err
		}
		return keepTotals(ctx, tx, t.asset.ID, t.to.ID)
	}
	if err := keepTotals(ctx, tx, t.asset.ID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO assets.asset_assignment (tenant_id, asset_id, seq, valid_from, cost_center_id, profit_center_id, transfer_id,
			created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.TenantID, t.asset.ID, seq, t.value, t.costCenter, t.profit, id, p.ID)
	return errs.Wrapf(err, "assets: assign asset %s from %s", t.asset.Number, t.value.Format(time.DateOnly))
}

// profitOrNil is a center transfer's new profit center, nil for an asset
// transfer.
func (t *transferring) profitOrNil() *uuid.UUID {
	if t.in.Kind != TransferCenter {
		return nil
	}
	return t.profit
}

// percentOf is a share in ten-thousandths of a percent as a plain
// decimal.
func percentOf(p *int64) *string {
	if p == nil {
		return nil
	}
	s := fmt.Sprintf("%d.%04d", *p/10_000, *p%10_000)
	return &s
}

// movementOf is a movement a transfer writes: its asset, seq and kind,
// the signed changes it records and, for the catch-up, the asset's
// booked_through before it.
type movementOf struct {
	asset                                    uuid.UUID
	seq                                      int32
	kind                                     string
	cost, costGC, accumulated, accumulatedGC money.Minor
	residual                                 money.Minor
	share                                    *string
	bookedYear, bookedPeriod                 *int16
	posting                                  time.Time
	year, period                             int16
	entry                                    *PostedEntry
}

// writeTransferLedger writes a transfer's movements and entries in one
// ledger and raises the asset's booked_through there to the period
// before V unless it is V (ADR-0057 §7): the catch-up's depreciation
// movement always, in its own entry's period when apart, and an asset
// transfer's transfer_out on the asset and transfer_in on the receiver.
func writeTransferLedger(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, seq int32, toSeq *int32, t *transferring, l *transferLedger) error {
	main := movementOf{posting: t.posting, year: t.period.FiscalYear, period: t.period.Number, entry: l.entry}
	if l.entry != nil {
		main.year, main.period = l.entry.FiscalYear, l.entry.Period
	}
	catchUp := main
	if l.apart {
		catchUp = movementOf{posting: l.catchUpOn, year: l.catchUpEntry.FiscalYear, period: l.catchUpEntry.Period, entry: l.catchUpEntry}
	}
	catchUp.asset, catchUp.seq, catchUp.kind = t.asset.ID, seq, kindDepreciation
	catchUp.accumulated, catchUp.accumulatedGC, catchUp.bookedYear, catchUp.bookedPeriod = -l.catchUp, -l.catchUpGC, l.terms.BookedYear, l.terms.BookedPeriod
	ms := []movementOf{catchUp}
	if toSeq != nil {
		share := l.share.FloatString(12)
		out, in := main, main
		out.asset, out.seq, out.kind, out.share = t.asset.ID, seq, "transfer_out", &share
		out.cost, out.costGC, out.accumulated, out.accumulatedGC, out.residual = -l.cost, -l.costGC, -l.accumulated, -l.accumulatedGC, -l.residual
		in.asset, in.seq, in.kind = t.to.ID, *toSeq, "transfer_in"
		in.cost, in.costGC, in.accumulated, in.accumulatedGC, in.residual = l.cost, l.costGC, l.accumulated, l.accumulatedGC, l.residual
		ms = append(ms, out, in)
	}
	for _, m := range ms {
		if err := insertMovement(ctx, tx, p, id, t, l.ledger, m); err != nil {
			return err
		}
	}
	for part, en := range map[string]*PostedEntry{partCatchUp: l.catchUpEntry, partMain: l.entry} {
		if err := insertDocumentEntry(ctx, tx, p, "transfer", id, l.ledger, part, en); err != nil {
			return err
		}
	}
	return bookThrough(ctx, tx, p, t.asset, l.ledger, t.before)
}

// insertMovement writes one of a transfer's movements in a ledger, with
// the class of its asset.
func insertMovement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, t *transferring, l LedgerRef, m movementOf) error {
	class := t.asset.Class
	if m.asset == t.to.ID {
		class = t.to.Class
	}
	var entryYear *int16
	var entry *uuid.UUID
	if m.entry != nil {
		entryYear, entry = &m.entry.FiscalYear, &m.entry.ID
	}
	_, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
			posting_date, fiscal_year, fiscal_period, cost, cost_gc, accumulated, accumulated_gc, residual, share, document_kind, document_id,
			journal_entry_year, journal_entry_id, booked_through_before_year, booked_through_before_period, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::numeric, 'transfer', $19, $20, $21, $22, $23, $24)`,
		p.TenantID, uuid.Must(uuid.NewV7()), m.asset, m.seq, t.company.ID, l.ID, class, m.kind, t.value, m.posting, m.year, m.period, m.cost, m.costGC,
		m.accumulated, m.accumulatedGC, m.residual, m.share, id, entryYear, entry, m.bookedYear, m.bookedPeriod, p.ID)
	return errs.Wrapf(err, "assets: %s movement of asset %s in ledger %s", m.kind, m.asset, l.Code)
}

// insertDocumentEntry records a document's entry of a part in a ledger,
// when there is one.
func insertDocumentEntry(ctx context.Context, tx pgx.Tx, p reqctx.Principal, kind string, id uuid.UUID, l LedgerRef, part string, en *PostedEntry) error {
	if en == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO assets.document_entry (tenant_id, document_kind, document_id, ledger_id, part, journal_entry_year, journal_entry_id,
			document_number) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.TenantID, kind, id, l.ID, part, en.FiscalYear, en.ID, en.DocumentNumber)
	return errs.Wrapf(err, "assets: %s entry of %s %s in ledger %s", part, kind, id, l.Code)
}

// bookThrough raises the asset's booked_through in the ledger to the
// period, never lowering it (ADR-0057 §7).
func bookThrough(ctx context.Context, tx pgx.Tx, p reqctx.Principal, a Asset, l LedgerRef, to postingPeriod) error {
	_, err := tx.Exec(ctx, `UPDATE assets.asset_terms SET booked_through_year = $3, booked_through_period = $4, version = version + 1, updated_by = $5
		WHERE asset_id = $1 AND ledger_id = $2 AND (booked_through_year IS NULL OR (booked_through_year, booked_through_period) < ($3, $4))`,
		a.ID, l.ID, to.year, to.period, p.ID)
	return errs.Wrapf(err, "assets: book asset %s through %d/%d in ledger %s", a.Number, to.year, to.period, l.Code)
}

// transferSelect reads transfers with their assets' and receivers'
// numbers and descriptions.
const transferSelect = `SELECT q.id, q.legal_entity_id, q.asset_id, a.asset_number, a.description, q.seq, q.kind, q.to_asset_id, q.to_seq, r.asset_number,
		r.description, r.class_code, q.value_date, q.posting_date, q.fiscal_year, q.fiscal_period, q.currency, q.currency_gc,
		trim_scale(q.share_percent)::text, q.share_cost, q.from_cost_center_id, q.from_profit_center_id, q.cost_center_id, q.profit_center_id, q.text,
		q.status, q.reversal_posting_date, q.reversal_reason, q.version, q.created_at, q.created_by, q.updated_at, q.updated_by
	FROM assets.transfer q JOIN assets.asset a ON a.tenant_id = q.tenant_id AND a.id = q.asset_id
	LEFT JOIN assets.asset r ON r.tenant_id = q.tenant_id AND r.id = q.to_asset_id`

func scanTransfer(r pgx.Row) (Transfer, error) {
	var q Transfer
	var reversed *time.Time
	var reason *string
	err := r.Scan(&q.ID, &q.LegalEntityID, &q.AssetID, &q.AssetNumber, &q.AssetDescription, &q.Seq, &q.Kind, &q.ToAssetID, &q.ToSeq, &q.ToAssetNumber,
		&q.ToAssetDescription, &q.ToClass, &q.ValueDate, &q.PostingDate, &q.FiscalYear, &q.Period, &q.Currency, &q.CurrencyGC, &q.SharePercent,
		&q.ShareCost, &q.FromCostCenterID, &q.FromProfitCenterID, &q.CostCenterID, &q.ProfitCenterID, &q.Text, &q.Status, &reversed, &reason,
		&q.Version, &q.CreatedAt, &q.CreatedBy, &q.UpdatedAt, &q.UpdatedBy)
	if err == nil && reversed != nil { // set together with the status (transfer_reversal)
		q.Reversal = &Reversal{PostingDate: *reversed, Reason: *reason}
	}
	return q, errs.Wrapf(err, "scan transfer")
}

// readTransfer reads a transfer of a legal entity the caller may act in,
// with what it moved per ledger and its entries; any other is not found.
func readTransfer(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Transfer, error) {
	return readDocument(ctx, tx, id, transferSelect, "transfer", scanTransfer, func(q Transfer) uuid.UUID { return q.LegalEntityID }, transferDetails)
}

// readDocument reads an assets document by id with its select, of a
// legal entity the caller may act in, and its details; any other is not
// found.
func readDocument[D any](ctx context.Context, tx pgx.Tx, id uuid.UUID, query, kind string, scan func(pgx.Row) (D, error), company func(D) uuid.UUID,
	details func(context.Context, pgx.Tx, []D) error) (D, error) {
	var none D
	access, err := authz.Companies(ctx)
	if err != nil {
		return none, err
	}
	q, err := scan(tx.QueryRow(ctx, query+` WHERE q.id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(company(q))):
		return none, problem.NotFoundOf("asset " + kind)
	case err != nil:
		return none, fmt.Errorf("assets: %s %s: %w", kind, id, err)
	}
	qs := []D{q}
	err = details(ctx, tx, qs)
	return qs[0], err
}

// transferDetails reads what the transfers moved per ledger and their
// entries into them: from the asset's catch-up and transfer_out
// movements, and for a center transfer that changed the profit center,
// the asset's balances after its catch-up, which it moved (the sums of
// its movements up to the transfer's seq, reversals included, so a
// movement reversed before it counts for nothing).
func transferDetails(ctx context.Context, tx pgx.Tx, qs []Transfer) error {
	ids := make([]uuid.UUID, len(qs))
	for i, q := range qs {
		ids[i] = q.ID
	}
	entries, err := readEntries(ctx, tx, "transfer", ids)
	if err != nil {
		return err
	}
	rows, _ := tx.Query(ctx, `SELECT q.id, m.ledger_id, trim_scale(max(m.share) FILTER (WHERE m.document_id = q.id))::text,
			-coalesce(sum(m.accumulated) FILTER (WHERE m.document_id = q.id AND m.kind = 'depreciation'), 0),
			-coalesce(sum(m.accumulated_gc) FILTER (WHERE m.document_id = q.id AND m.kind = 'depreciation'), 0),
			max(m.posting_date) FILTER (WHERE m.document_id = q.id AND m.kind = 'depreciation'),
			max(m.fiscal_year) FILTER (WHERE m.document_id = q.id AND m.kind = 'depreciation'),
			max(m.fiscal_period) FILTER (WHERE m.document_id = q.id AND m.kind = 'depreciation'),
			CASE q.kind WHEN 'asset' THEN -sum(m.cost) FILTER (WHERE m.kind = 'transfer_out') ELSE sum(m.cost) END,
			CASE q.kind WHEN 'asset' THEN -sum(m.cost_gc) FILTER (WHERE m.kind = 'transfer_out') ELSE sum(m.cost_gc) END,
			CASE q.kind WHEN 'asset' THEN -sum(m.accumulated) FILTER (WHERE m.kind = 'transfer_out') ELSE sum(m.accumulated) END,
			CASE q.kind WHEN 'asset' THEN -sum(m.accumulated_gc) FILTER (WHERE m.kind = 'transfer_out') ELSE sum(m.accumulated_gc) END,
			coalesce(-sum(m.residual) FILTER (WHERE m.kind = 'transfer_out'), 0)
		FROM assets.transfer q JOIN assets.movement m ON m.tenant_id = q.tenant_id AND m.asset_id = q.asset_id
			AND ((m.document_id = q.id AND m.reverses_id IS NULL) OR (q.kind = 'center' AND m.seq <= q.seq))
		WHERE q.id = ANY($1) GROUP BY q.id, q.kind, m.ledger_id`, ids)
	values := map[uuid.UUID][]TransferValues{}
	var doc uuid.UUID
	var v TransferValues
	_, err = pgx.ForEachRow(rows, []any{&doc, &v.LedgerID, &v.Share, &v.CatchUp, &v.CatchUpGC, &v.CatchUpOn, &v.CatchUpIn.year, &v.CatchUpIn.period,
		&v.Cost, &v.CostGC, &v.Accumulated, &v.AccumulatedGC, &v.Residual}, func() error {
		values[doc] = append(values[doc], v)
		return nil
	})
	for i := range qs {
		qs[i].Entries, qs[i].Ledgers = entries[qs[i].ID], movedOf(qs[i], values[qs[i].ID])
	}
	return errs.Wrapf(err, "assets: values of %d transfers", len(ids))
}

// movedOf is what a transfer moved per ledger: a center transfer without
// a profit center change moved no balance.
func movedOf(q Transfer, vs []TransferValues) []TransferValues {
	if q.Kind != TransferCenter || q.profitCenterChange() {
		return vs
	}
	for i := range vs {
		vs[i].Cost, vs[i].CostGC, vs[i].Accumulated, vs[i].AccumulatedGC = 0, 0, 0, 0
	}
	return vs
}
