package assets

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// sheetValues are a history sheet row's sums (ADR-0057 §9): the opening
// cost and accumulated depreciation, the year's columns, and whether
// anything was posted in the year. Accumulated depreciation is negative,
// as the movements record it.
type sheetValues struct {
	OpeningCost, Acquisitions, Retirements, Transfers                              money.Minor
	OpeningAccumulated, Depreciation, RetirementsAccumulated, TransfersAccumulated money.Minor
	Moved                                                                          bool
}

func (v sheetValues) closingCost() money.Minor {
	return v.OpeningCost + v.Acquisitions + v.Retirements + v.Transfers
}

func (v sheetValues) closingAccumulated() money.Minor {
	return v.OpeningAccumulated + v.Depreciation + v.RetirementsAccumulated + v.TransfersAccumulated
}

// shown says whether the row is on the sheet: something was posted in the
// year, or it opens with a value.
func (v sheetValues) shown() bool { return v.Moved || v.OpeningCost != 0 || v.OpeningAccumulated != 0 }

func (v sheetValues) plus(o sheetValues) sheetValues {
	return sheetValues{OpeningCost: v.OpeningCost + o.OpeningCost, Acquisitions: v.Acquisitions + o.Acquisitions,
		Retirements: v.Retirements + o.Retirements, Transfers: v.Transfers + o.Transfers, OpeningAccumulated: v.OpeningAccumulated + o.OpeningAccumulated,
		Depreciation: v.Depreciation + o.Depreciation, RetirementsAccumulated: v.RetirementsAccumulated + o.RetirementsAccumulated,
		TransfersAccumulated: v.TransfersAccumulated + o.TransfersAccumulated, Moved: v.Moved || o.Moved}
}

// sheetSums reads a totals table's rows up to the year $3 as a row's
// sums: the years before summed into the opening values, the year's own
// by column.
const sheetSums = `coalesce(sum(acquisitions + retired_cost + transferred_cost) FILTER (WHERE fiscal_year < $3), 0) AS opening_cost,
		coalesce(sum(acquisitions) FILTER (WHERE fiscal_year = $3), 0) AS acquisitions,
		coalesce(sum(retired_cost) FILTER (WHERE fiscal_year = $3), 0) AS retirements,
		coalesce(sum(transferred_cost) FILTER (WHERE fiscal_year = $3), 0) AS transfers,
		coalesce(sum(depreciation + retired_accumulated + transferred_accumulated) FILTER (WHERE fiscal_year < $3), 0) AS opening_accumulated,
		coalesce(sum(depreciation) FILTER (WHERE fiscal_year = $3), 0) AS depreciation,
		coalesce(sum(retired_accumulated) FILTER (WHERE fiscal_year = $3), 0) AS retirements_accumulated,
		coalesce(sum(transferred_accumulated) FILTER (WHERE fiscal_year = $3), 0) AS transfers_accumulated,
		coalesce(bool_or(fiscal_year = $3), false) AS moved`

// sheetRow is a class's or an asset's row of the sheet.
type sheetRow struct {
	Class string
	sheetValues
	Asset                       uuid.UUID // by asset
	Number, Description, Status string    // by asset
}

// sheetTarget is the sheet's company, ledger, fiscal year and class
// filter ("" for every class).
type sheetTarget struct {
	co      Company
	ledger  LedgerRef
	periods []Period
	class   string
}

// The history sheet's groupings (group_by).
const (
	sheetByClass = "class"
	sheetByAsset = "asset"
)

// GetAssetHistorySheet implements GET /v1/asset-history-sheet (ADR-0057
// §9): the reconciliation of a company's asset values over a fiscal year
// in a ledger, by class or by asset, read from the year totals the
// postings keep (keepTotals), with the totals of every row selected.
func (e *Endpoints) GetAssetHistorySheet(ctx context.Context, params gen.GetAssetHistorySheetParams) (gen.GetAssetHistorySheetRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	groupBy := params.GroupBy.Or(sheetByClass)
	if groupBy != sheetByClass && groupBy != sheetByAsset {
		return nil, problem.BadRequest("request.invalid_parameter", "Parameter group_by is class or asset.",
			problem.FieldError{Field: "group_by", Code: "invalid", Message: "class or asset"})
	}
	ledgerID := apitypes.Ptr[uuid.UUID](params.LedgerID)
	scope := fmt.Sprintf("getAssetHistorySheet|%s|%s|%d|%s|%s", params.LegalEntityID, ledgerID, params.FiscalYear, groupBy, params.Class.Value)
	size, after, err := e.sheetPage(params, scope)
	if err != nil {
		return nil, err
	}
	s, err := e.sheetTarget(ctx, params, ledgerID)
	if err != nil {
		return nil, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	classes, err := classSums(ctx, tx, s)
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, s.co.Currency, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: currency %s: %w", s.co.Currency, err)
	}
	out := s.sheetOf(groupBy, classes, exp)
	if groupBy == sheetByClass {
		out.Items = s.rowsOf(doc, classes, exp, func(r sheetRow) bool { return r.shown() })
		return out, nil
	}
	assets, err := assetSums(ctx, tx, s, after, size+1)
	if err != nil {
		return nil, err
	}
	if len(assets) > size {
		assets = assets[:size]
		last := assets[size-1]
		tok, _ := e.Pages.Encode(scope, previewKey{Number: last.Number, ID: last.Asset}) // a string and a uuid always encode
		out.NextPageToken = gen.NewOptString(tok)
	}
	out.Items = s.rowsOf(doc, assets, exp, func(sheetRow) bool { return true })
	return out, nil
}

// sheetPage reads the page size and the asset a page starts after (none:
// the first page). A token is issued by asset only, so by class one is
// refused (400 request.invalid_page_token), its scope naming the grouping.
func (e *Endpoints) sheetPage(params gen.GetAssetHistorySheetParams, scope string) (int, *previewKey, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil || !params.PageToken.Set {
		return size, nil, err
	}
	after := &previewKey{}
	return size, after, e.Pages.Decode(scope, params.PageToken.Value, after)
}

// sheetTarget reads the sheet's company (404 for one the caller may not
// act in), ledger (the leading by default; 422 ledger.ledger_not_kept for
// one the company does not keep) and the fiscal year's periods (422
// ledger.no_fiscal_period for a year not set up).
func (e *Endpoints) sheetTarget(ctx context.Context, params gen.GetAssetHistorySheetParams, ledgerID *uuid.UUID) (sheetTarget, error) {
	co, err := e.company(ctx, params.LegalEntityID)
	if err != nil {
		return sheetTarget{}, err
	}
	l, ok, err := e.ledgerOf(ctx, co.ID, ledgerID)
	if err != nil {
		return sheetTarget{}, err
	}
	if !ok {
		return sheetTarget{}, invalid("ledger.ledger_not_kept", fmt.Sprintf("Company %s does not keep this ledger.", co.Code), "ledger_id",
			"a ledger the company keeps")
	}
	periods, err := e.Ledger.Periods(ctx, co.ID, int16(params.FiscalYear))
	if err != nil {
		return sheetTarget{}, fmt.Errorf("assets: periods of %s in %d: %w", co.Code, params.FiscalYear, err)
	}
	return sheetTarget{co: co, ledger: l, periods: periods, class: params.Class.Value}, nil
}

// classSums reads each class's sums from the company's class year totals
// in the ledger up to the year: at most classes × years rows.
func classSums(ctx context.Context, tx pgx.Tx, s sheetTarget) ([]sheetRow, error) {
	rows, _ := tx.Query(ctx, `SELECT class_code, `+sheetSums+` FROM assets.class_year_total
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year <= $3 AND ($4 = '' OR class_code = $4)
		GROUP BY class_code ORDER BY class_code`, s.co.ID, s.ledger.ID, s.year(), s.class)
	var out []sheetRow
	var v sheetRow
	_, err := pgx.ForEachRow(rows, append([]any{&v.Class}, v.sums()...), func() error { out = append(out, v); return nil })
	return out, errs.Wrapf(err, "assets: history sheet by class of %s in %d", s.co.Code, s.year())
}

// assetSums reads up to limit of the company's assets on the sheet after
// the one given (none: from the first), by asset number, each with its
// sums from its year totals in the ledger up to the year. An asset's years
// are one range of the year totals' key, and the assets are read in the
// order of their number's index, so a page reads its own assets alone.
func assetSums(ctx context.Context, tx pgx.Tx, s sheetTarget, after *previewKey, limit int) ([]sheetRow, error) {
	var number *string
	var id *uuid.UUID
	if after != nil {
		number, id = &after.Number, &after.ID
	}
	rows, _ := tx.Query(ctx, `SELECT * FROM (SELECT a.id, a.asset_number, a.description, a.status, a.class_code, y.*
			FROM assets.asset a CROSS JOIN LATERAL (SELECT `+sheetSums+` FROM assets.year_total t
				WHERE t.tenant_id = a.tenant_id AND t.ledger_id = $2 AND t.asset_id = a.id AND t.fiscal_year <= $3) y
			WHERE a.legal_entity_id = $1 AND ($4 = '' OR a.class_code = $4) AND ($5::text IS NULL OR (a.asset_number, a.id) > ($5, $6::uuid))) x
		WHERE (moved OR opening_cost <> 0 OR opening_accumulated <> 0)
		ORDER BY asset_number, id LIMIT $7`, pagination.PlanPerCall, s.co.ID, s.ledger.ID, s.year(), s.class, number, id, limit)
	var out []sheetRow
	var v sheetRow
	_, err := pgx.ForEachRow(rows, append([]any{&v.Asset, &v.Number, &v.Description, &v.Status, &v.Class}, v.sums()...),
		func() error { out = append(out, v); return nil })
	return out, errs.Wrapf(err, "assets: history sheet by asset of %s in %d", s.co.Code, s.year())
}

// sums are the row's sums as sheetSums reads them.
func (v *sheetRow) sums() []any {
	return []any{&v.OpeningCost, &v.Acquisitions, &v.Retirements, &v.Transfers, &v.OpeningAccumulated, &v.Depreciation, &v.RetirementsAccumulated,
		&v.TransfersAccumulated, &v.Moved}
}

func (s sheetTarget) year() int16 { return s.periods[0].FiscalYear }

// sheetOf is the sheet's head and the totals of every class selected: the
// totals of all assets' rows too, since a class's year totals are the
// sums of its assets'.
func (s sheetTarget) sheetOf(groupBy string, classes []sheetRow, exp int) *gen.AssetHistorySheet {
	var totals sheetValues
	for _, c := range classes {
		totals = totals.plus(c.sheetValues)
	}
	return &gen.AssetHistorySheet{LegalEntityID: s.co.ID, CompanyCode: s.co.Code, CompanyName: s.co.Name, LedgerID: s.ledger.ID,
		LedgerCode: s.ledger.Code, LedgerName: s.ledger.Name, FiscalYear: int32(s.year()), StartDate: s.periods[0].Start,
		EndDate: s.periods[len(s.periods)-1].End, Currency: s.co.Currency, GroupBy: groupBy, Items: []gen.AssetHistorySheetRow{},
		Totals: s.valuesOf(totals, exp)}
}

// rowsOf shows the rows keep keeps, each with its class's name from the
// configuration (none when it no longer has the class).
func (s sheetTarget) rowsOf(doc *config.Config, rows []sheetRow, exp int, keep func(sheetRow) bool) []gen.AssetHistorySheetRow {
	out := []gen.AssetHistorySheetRow{}
	for _, r := range rows {
		if !keep(r) {
			continue
		}
		v := s.valuesOf(r.sheetValues, exp)
		row := gen.AssetHistorySheetRow{ClassCode: r.Class, OpeningCost: v.OpeningCost, Acquisitions: v.Acquisitions, Retirements: v.Retirements,
			Transfers: v.Transfers, ClosingCost: v.ClosingCost, OpeningAccumulated: v.OpeningAccumulated, Depreciation: v.Depreciation,
			RetirementsAccumulated: v.RetirementsAccumulated, TransfersAccumulated: v.TransfersAccumulated, ClosingAccumulated: v.ClosingAccumulated,
			OpeningNetBookValue: v.OpeningNetBookValue, ClosingNetBookValue: v.ClosingNetBookValue}
		if c := doc.AssetClass(r.Class); c != nil {
			row.ClassName = gen.NewOptString(c.Name)
		}
		if r.Asset != uuid.Nil {
			row.AssetID, row.AssetNumber = gen.NewOptUUID(r.Asset), gen.NewOptString(r.Number)
			row.Description, row.Status = gen.NewOptString(r.Description), gen.NewOptString(r.Status)
		}
		out = append(out, row)
	}
	return out
}

// valuesOf shows sums as the sheet's columns, with the closing values and
// net book values they give.
func (s sheetTarget) valuesOf(v sheetValues, exp int) gen.AssetHistorySheetValues {
	m := func(a money.Minor) gen.Money { return apitypes.Money(a, s.co.Currency, exp) }
	return gen.AssetHistorySheetValues{OpeningCost: m(v.OpeningCost), Acquisitions: m(v.Acquisitions), Retirements: m(v.Retirements),
		Transfers: m(v.Transfers), ClosingCost: m(v.closingCost()), OpeningAccumulated: m(v.OpeningAccumulated), Depreciation: m(v.Depreciation),
		RetirementsAccumulated: m(v.RetirementsAccumulated), TransfersAccumulated: m(v.TransfersAccumulated), ClosingAccumulated: m(v.closingAccumulated()),
		OpeningNetBookValue: m(v.OpeningCost + v.OpeningAccumulated), ClosingNetBookValue: m(v.closingCost() + v.closingAccumulated())}
}
