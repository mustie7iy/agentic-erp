package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// documentImpairment is an impairment's document kind on its movement and
// entry.
const documentImpairment = "impairment"

// Impairment is a stored impairment loss or reversal with its asset's
// number, description and class, the carrying amounts it was measured
// against and its journal entry.
type Impairment struct {
	ID, LegalEntityID, AssetID, LedgerID uuid.UUID
	AssetNumber, AssetDescription, Class string
	Seq                                  int32
	Kind                                 string
	ValueDate, PostingDate               time.Time
	FiscalYear, Period                   int16
	Currency, CurrencyGC                 string
	Amount, AmountGC                     money.Minor
	Carrying, Unimpaired                 money.Minor
	Text                                 *string
	Entries                              []DocumentEntry
	CreatedAt                            time.Time
	CreatedBy                            uuid.UUID
}

// change is the impairment's change of the asset's accumulated
// depreciation (and its group amount): a loss credits it (negative), a
// reversal debits it.
func (q Impairment) change() (money.Minor, money.Minor) {
	if q.Kind == ImpairmentLoss {
		return -q.Amount, -q.AmountGC
	}
	return q.Amount, q.AmountGC
}

// writeImpairment writes the impairment with the asset's next seq, its
// movement on the ledger's entry and the entry, derives the asset again
// (its last seq and version) and adds the movement to the year totals.
func writeImpairment(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, r *impairing, en PostedEntry) error {
	seq := r.asset.LastSeq + 1
	if _, err := tx.Exec(ctx, `INSERT INTO assets.impairment (tenant_id, id, legal_entity_id, asset_id, seq, ledger_id, kind, value_date, posting_date,
			fiscal_year, fiscal_period, currency, amount, currency_gc, amount_gc, carrying_amount, unimpaired_amount, text, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		p.TenantID, id, r.company.ID, r.asset.ID, seq, r.ledger.ID, r.in.Kind, r.value, r.posting, r.period.FiscalYear, r.period.Number,
		r.company.Currency, r.in.Amount, r.group, r.amountGC, r.carrying, r.unimpaired, r.in.Text, p.ID); err != nil {
		return fmt.Errorf("assets: insert impairment of asset %s: %w", r.asset.Number, err)
	}
	kind, accumulated, accumulatedGC := kindImpairment, -r.in.Amount, -r.amountGC
	if r.in.Kind == ImpairmentReversal {
		kind, accumulated, accumulatedGC = kindImpairmentReversal, r.in.Amount, r.amountGC
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
			posting_date, fiscal_year, fiscal_period, accumulated, accumulated_gc, document_kind, document_id, journal_entry_year, journal_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'impairment', $15, $16, $17, $18)`,
		p.TenantID, uuid.Must(uuid.NewV7()), r.asset.ID, seq, r.company.ID, r.ledger.ID, r.asset.Class, kind, r.value, r.posting, en.FiscalYear,
		en.Period, accumulated, accumulatedGC, id, en.FiscalYear, en.ID, p.ID); err != nil {
		return fmt.Errorf("assets: impairment movement of asset %s in ledger %s: %w", r.asset.Number, r.ledger.Code, err)
	}
	if err := insertDocumentEntry(ctx, tx, p, documentImpairment, id, r.ledger, partMain, &en); err != nil {
		return err
	}
	if err := derive(ctx, tx, p, r.asset.ID, seq); err != nil {
		return err
	}
	return keepTotals(ctx, tx, r.asset.ID)
}

// impairmentSelect reads impairments with their assets' numbers,
// descriptions and classes.
const impairmentSelect = `SELECT q.id, q.legal_entity_id, q.asset_id, a.asset_number, a.description, a.class_code, q.seq, q.ledger_id, q.kind, q.value_date,
		q.posting_date, q.fiscal_year, q.fiscal_period, q.currency, q.amount, q.currency_gc, q.amount_gc, q.carrying_amount, q.unimpaired_amount, q.text,
		q.created_at, q.created_by
	FROM assets.impairment q JOIN assets.asset a ON a.tenant_id = q.tenant_id AND a.id = q.asset_id`

func scanImpairment(r pgx.Row) (Impairment, error) {
	var q Impairment
	err := r.Scan(&q.ID, &q.LegalEntityID, &q.AssetID, &q.AssetNumber, &q.AssetDescription, &q.Class, &q.Seq, &q.LedgerID, &q.Kind, &q.ValueDate,
		&q.PostingDate, &q.FiscalYear, &q.Period, &q.Currency, &q.Amount, &q.CurrencyGC, &q.AmountGC, &q.Carrying, &q.Unimpaired, &q.Text,
		&q.CreatedAt, &q.CreatedBy)
	return q, errs.Wrapf(err, "scan impairment")
}

// readImpairment reads an impairment of a legal entity the caller may act
// in, with its entry; any other is not found.
func readImpairment(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Impairment, error) {
	return readDocument(ctx, tx, id, impairmentSelect, documentImpairment, scanImpairment, func(q Impairment) uuid.UUID { return q.LegalEntityID },
		impairmentEntries)
}

// impairmentEntries reads the impairments' entries into them.
func impairmentEntries(ctx context.Context, tx pgx.Tx, qs []Impairment) error {
	ids := make([]uuid.UUID, len(qs))
	for i, q := range qs {
		ids[i] = q.ID
	}
	entries, err := readEntries(ctx, tx, documentImpairment, ids)
	for i := range qs {
		qs[i].Entries = entries[qs[i].ID]
	}
	return err
}

// recordImpairment audits the impairment and publishes it as
// AssetMovementPosted (document kind impairment), with the asset's status
// and version after it and its one ledger's change of accumulated
// depreciation with the entry. Its text is not recorded (ADR-0079).
func (e *Endpoints) recordImpairment(ctx context.Context, tx pgx.Tx, q Impairment) error {
	expLC, err := currency.Exponent(ctx, tx, q.Currency, nil)
	if err != nil {
		return fmt.Errorf("assets: currency %s: %w", q.Currency, err)
	}
	expGC, err := currency.Exponent(ctx, tx, q.CurrencyGC, nil)
	if err != nil {
		return fmt.Errorf("assets: currency %s: %w", q.CurrencyGC, err)
	}
	asset, err := readAsset(ctx, tx, q.AssetID, false)
	if err != nil {
		return err
	}
	en := q.Entries[0] // an impairment posts one entry
	lc, gc := q.change()
	ledger := map[string]any{"ledger_id": q.LedgerID, "cost": money.EventAmount(0, q.Currency, expLC), "cost_gc": money.EventAmount(0, q.CurrencyGC, expGC),
		"accumulated": money.EventAmount(lc, q.Currency, expLC), "accumulated_gc": money.EventAmount(gc, q.CurrencyGC, expGC),
		"journal_entry_id": en.EntryID, "fiscal_year": en.FiscalYear, "document_number": en.DocumentNumber}
	value, posting := q.ValueDate.Format(time.DateOnly), q.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": q.Seq, "kind": q.Kind, "ledger_id": q.LedgerID,
		"value_date": value, "posting_date": posting, "amount": int64(q.Amount), "amount_gc": int64(q.AmountGC), "carrying_amount": int64(q.Carrying),
		"unimpaired_amount": int64(q.Unimpaired), "text_given": q.Text != nil, "journal_entry_id": en.EntryID, "document_number": en.DocumentNumber}
	return audit.Record(ctx, "assets.impairment.post", "assets.impairment", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_posted", AggregateType: "assets.impairment", AggregateID: q.ID,
			Payload: map[string]any{"document_kind": documentImpairment, "document_id": q.ID, "impairment_kind": q.Kind, "asset_id": q.AssetID,
				"legal_entity_id": q.LegalEntityID, "asset_number": q.AssetNumber, "class_code": q.Class, "value_date": value, "posting_date": posting,
				"currency_lc": q.Currency, "currency_gc": q.CurrencyGC, "ledgers": []map[string]any{ledger}, "asset_status": asset.Status,
				"asset_version": asset.Version}})
}
