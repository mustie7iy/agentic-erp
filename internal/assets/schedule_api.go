package assets

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// termsMissing refuses a ledger the asset has no terms in.
func termsMissing(number string) error {
	return invalid("assets.ledger_terms_missing", fmt.Sprintf("Asset %s has no depreciation terms in this ledger; give it terms there with PATCH "+
		"/v1/assets/{id} (a ledger its company keeps).", number), "ledger_id", "a ledger the asset has terms in")
}

// GetAssetSchedule implements GET /v1/assets/{id}/schedule (ADR-0057 §7):
// the asset's planned and booked depreciation per regular period of a
// fiscal year in one ledger, computed from its terms and movements.
func (e *Endpoints) GetAssetSchedule(ctx context.Context, params gen.GetAssetScheduleParams) (gen.GetAssetScheduleRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	a, err := readAsset(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	l, ok, err := e.ledgerOf(ctx, a.LegalEntityID, apitypes.Ptr[uuid.UUID](params.LedgerID))
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(a.Terms, func(t Terms) bool { return t.LedgerID == l.ID })
	if !ok || i < 0 {
		return nil, termsMissing(a.Number)
	}
	periods, err := e.Ledger.Periods(ctx, a.LegalEntityID, int16(params.FiscalYear))
	if err != nil {
		return nil, fmt.Errorf("assets: periods of %s in %d: %w", a.LegalEntityID, params.FiscalYear, err)
	}
	d := &depreciable{asset: a.ID, terms: a.Terms[i], capitalised: a.CapitalisedOn, bookedIn: map[postingPeriod]money.Minor{}}
	if err := readMovements(ctx, tx, l.ID, map[uuid.UUID]*depreciable{a.ID: d}); err != nil {
		return nil, err
	}
	steps, err := d.schedule(MonthOf(periods[len(periods)-1].Start))
	if err != nil {
		return nil, err
	}
	v, err := e.docViewOf(ctx, tx, []uuid.UUID{a.LegalEntityID}, nil, nil)
	if err != nil {
		return nil, err
	}
	co := v.companies[a.LegalEntityID]
	if err := v.exponent(ctx, tx, co.Currency); err != nil {
		return nil, err
	}
	return scheduleOf(v, a, co, l, d, periods, steps), nil
}

// scheduleOf shows the asset's schedule for the year's periods.
func scheduleOf(v docView, a Asset, co Company, l LedgerRef, d *depreciable, periods []Period, steps []Step) *gen.AssetSchedule {
	lc := func(m money.Minor) gen.Money { return v.money(m, co.Currency) }
	in := d.terms.inForce(periods[len(periods)-1].Start)
	out := &gen.AssetSchedule{AssetID: a.ID, AssetNumber: a.Number, Description: a.Description, LegalEntityID: a.LegalEntityID, CompanyCode: co.Code,
		LedgerID: l.ID, LedgerCode: l.Code, LedgerName: l.Name, FiscalYear: int32(periods[0].FiscalYear), Currency: co.Currency, Method: in.Method,
		UsefulLifeMonths: in.UsefulLifeMonths, DecliningFactor: apitypes.OptString(in.DecliningFactor), ResidualValue: lc(in.Residual),
		DepreciationStart: apitypes.OptDate(d.start()), EstimateChanges: changesAPI(d.terms.Changes, lc), HeldForSale: heldForSaleAPI(d.terms.HeldForSale),
		Periods: make([]gen.AssetSchedulePeriod, len(periods))}
	var planned, booked money.Minor
	for i, p := range periods {
		// A period before the walk's first step holds no value: no movement
		// was valued by then and depreciation had not started.
		var step Step
		if j := slices.IndexFunc(steps, func(s Step) bool { return s.Month == MonthOf(p.Start) }); j >= 0 {
			step = steps[j]
		}
		b := d.bookedIn[postingPeriod{p.FiscalYear, p.Number}]
		planned, booked = planned+step.Depreciation, booked+b
		out.Periods[i] = gen.AssetSchedulePeriod{FiscalYear: int32(p.FiscalYear), Period: int32(p.Number), StartDate: p.Start, EndDate: p.End,
			Planned: lc(step.Depreciation), Booked: lc(b), NetBookValue: lc(step.BookValue)}
	}
	out.Totals = gen.AssetScheduleTotals{Planned: lc(planned), Booked: lc(booked)}
	return out
}

var movementSort = pagination.Sort{Default: "seq", Fields: []pagination.Field{{Name: "seq", Column: "seq", Type: "integer"}}}

// movementRow is a movement as the list shows it: with its entry's
// document number (an acquisition's, retirement's or transfer's entry, or
// a depreciation run's batch's)
// and the movement reversing it.
type movementRow struct {
	ID, LedgerID                                                 uuid.UUID
	Seq                                                          int32
	Kind                                                         string
	ValueDate, PostingDate                                       time.Time
	FiscalYear, FiscalPeriod                                     int16
	Cost, Accumulated, CostGC, AccumulatedGC, Residual, Proceeds money.Minor
	Share                                                        *string
	DocumentKind                                                 string
	DocumentID                                                   uuid.UUID
	EntryYear                                                    *int16
	EntryID                                                      *uuid.UUID
	DocumentNumber                                               *string
	Reverses, ReversedBy                                         *uuid.UUID
	AssetID                                                      uuid.UUID // the filter's
}

// movementSelect reads movements with the document number of their
// entry, kept on the document's entry per ledger (or on its reversal), or
// for a depreciation run's on its batch's (T57f), and the movement
// reversing each; the keyset pages on its seq and id.
const movementSelect = `SELECT * FROM (SELECT m.id, m.ledger_id, m.seq, m.kind, m.value_date, m.posting_date, m.fiscal_year, m.fiscal_period, m.cost,
		m.accumulated, m.cost_gc, m.accumulated_gc, m.residual, m.proceeds, trim_scale(m.share)::text AS share, m.document_kind, m.document_id,
		m.journal_entry_year, m.journal_entry_id,
		coalesce(b.document_number, CASE WHEN de.journal_entry_id = m.journal_entry_id THEN de.document_number ELSE de.reversal_document_number END)
			AS document_number,
		m.reverses_id, r.id AS reversed_by_id, m.asset_id
	FROM assets.movement m
	LEFT JOIN assets.document_entry de ON de.tenant_id = m.tenant_id AND de.document_kind = m.document_kind AND de.document_id = m.document_id
		AND de.ledger_id = m.ledger_id AND m.journal_entry_id IN (de.journal_entry_id, de.reversal_entry_id)
	LEFT JOIN assets.depreciation_batch b ON b.tenant_id = m.tenant_id AND m.document_kind = 'depreciation_run' AND b.run_id = m.document_id
		AND b.journal_entry_id = m.journal_entry_id
	LEFT JOIN assets.movement r ON r.tenant_id = m.tenant_id AND r.reverses_id = m.id) mv`

// ListAssetMovements implements GET /v1/assets/{id}/movements (ADR-0057
// §5): the asset's movements in posting order, filtered by ledger, fiscal
// year and kind.
func (e *Endpoints) ListAssetMovements(ctx context.Context, params gen.ListAssetMovementsParams) (gen.ListAssetMovementsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	a, err := readAsset(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	ledger, year := apitypes.Ptr[uuid.UUID](params.LedgerID), apitypes.Ptr[int32](params.FiscalYear)
	scope := fmt.Sprintf("listAssetMovements|%s|%s|%t %d|%s", a.ID, ledger, params.FiscalYear.Set, params.FiscalYear.Value, params.Kind.Value)
	ks, err := e.Pages.Keyset(movementSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(movementSelect+` WHERE asset_id = $1 AND ($2::uuid IS NULL OR ledger_id = $2) AND ($3::smallint IS NULL OR fiscal_year = $3)
		AND ($4 = '' OR kind = $4)`, []any{a.ID, ledger, year, params.Kind.Value})
	rows, _ := tx.Query(ctx, q, args...)
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[movementRow])
	if err != nil {
		return nil, fmt.Errorf("assets: movements of asset %s: %w", a.ID, err)
	}
	group, err := e.Companies.GroupCurrency(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: group currency: %w", err)
	}
	v, err := e.docViewOf(ctx, tx, []uuid.UUID{a.LegalEntityID}, nil, nil)
	if err != nil {
		return nil, err
	}
	co := v.companies[a.LegalEntityID]
	for _, c := range []string{co.Currency, group} {
		if err := v.exponent(ctx, tx, c); err != nil {
			return nil, err
		}
	}
	items, next := apitypes.PageOf(ks, ms, func(m movementRow) (string, uuid.UUID) { return fmt.Sprint(m.Seq), m.ID },
		func(m movementRow) gen.AssetMovement { return v.movementAPI(m, co.Currency, group) })
	return &gen.AssetMovementPage{Items: items, NextPageToken: next}, nil
}

// movementAPI shows a movement, its amounts in local and group currency.
func (v docView) movementAPI(m movementRow, lc, gc string) gen.AssetMovement {
	out := gen.AssetMovement{ID: m.ID, Seq: m.Seq, LedgerID: m.LedgerID, Kind: m.Kind, ValueDate: m.ValueDate, PostingDate: m.PostingDate,
		FiscalYear: int32(m.FiscalYear), FiscalPeriod: int32(m.FiscalPeriod), Cost: v.money(m.Cost, lc), Accumulated: v.money(m.Accumulated, lc),
		CostGc: v.money(m.CostGC, gc), AccumulatedGc: v.money(m.AccumulatedGC, gc), Residual: v.money(m.Residual, lc),
		Proceeds: v.money(m.Proceeds, lc), Share: apitypes.OptString(m.Share), DocumentKind: m.DocumentKind, DocumentID: m.DocumentID,
		JournalEntryID: apitypes.OptUUID(m.EntryID), DocumentNumber: apitypes.OptString(m.DocumentNumber), ReversesID: apitypes.OptUUID(m.Reverses),
		ReversedByID: apitypes.OptUUID(m.ReversedBy)}
	if m.EntryYear != nil {
		out.JournalEntryFiscalYear = gen.NewOptInt32(int32(*m.EntryYear))
	}
	v.nameLedger(m.LedgerID, &out.LedgerCode, &out.LedgerName)
	return out
}

// nameLedger sets code and name to a ledger's, when the view has it.
func (v docView) nameLedger(id uuid.UUID, code, name *string) {
	if i := slices.IndexFunc(v.ledgers, func(l LedgerRef) bool { return l.ID == id }); i >= 0 {
		*code, *name = v.ledgers[i].Code, v.ledgers[i].Name
	}
}
