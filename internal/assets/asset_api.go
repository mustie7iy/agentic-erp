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
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// centerReadPermission reads cost and profit centers (the ledger's): an
// asset names its centers by code and name only to a caller who may read
// them, as an expansion would (04 §4.4, authz.MayUse).
const centerReadPermission = "ledger.cost_center.read"

// view is what assets are shown with: their classes' names, their
// companies with their currencies' minor-unit digits, the ledgers in
// order, and the names of their centers (nil when the caller may not
// read centers), and the tenant's group currency.
type view struct {
	classes   map[string]string
	companies map[uuid.UUID]Company
	exponents map[string]int
	ledgers   []LedgerRef
	centers   map[uuid.UUID]CenterName
	group     string
}

// viewOf reads what the assets are shown with.
func (e *Endpoints) viewOf(ctx context.Context, tx pgx.Tx, as []Asset) (view, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return view{}, fmt.Errorf("assets: configuration: %w", err)
	}
	v := view{classes: map[string]string{}, companies: map[uuid.UUID]Company{}, exponents: map[string]int{}}
	for _, c := range doc.AssetClasses {
		v.classes[c.Code] = c.Name
	}
	var centers []uuid.UUID
	for _, a := range as {
		if err := v.company(ctx, tx, e.Companies, a.LegalEntityID); err != nil {
			return view{}, err
		}
		for _, id := range []*uuid.UUID{a.CostCenterID, a.ProfitCenterID} {
			if id != nil && !slices.Contains(centers, *id) {
				centers = append(centers, *id)
			}
		}
	}
	if v.ledgers, err = e.Ledger.Ledgers(ctx, nil); err != nil {
		return view{}, fmt.Errorf("assets: ledgers: %w", err)
	}
	if err := v.groupCurrency(ctx, tx, e.Companies); err != nil {
		return view{}, err
	}
	may, err := authz.MayUse(ctx, centerReadPermission)
	if err != nil || !may || len(centers) == 0 {
		return v, err
	}
	if v.centers, err = e.Centers.Names(ctx, centers); err != nil {
		return view{}, fmt.Errorf("assets: names of %d centers: %w", len(centers), err)
	}
	return v, nil
}

// company adds a legal entity and its currency's digits to the view.
func (v *view) company(ctx context.Context, tx pgx.Tx, companies Companies, le uuid.UUID) error {
	if _, ok := v.companies[le]; ok {
		return nil
	}
	// An asset's legal entity was checked when the asset was created and
	// legal entities are never removed, so it is always found.
	co, _, err := companies.Company(ctx, le)
	if err != nil {
		return fmt.Errorf("assets: legal entity %s: %w", le, err)
	}
	v.companies[le] = co
	if _, ok := v.exponents[co.Currency]; !ok {
		exp, err := currency.Exponent(ctx, tx, co.Currency, nil)
		if err != nil {
			return fmt.Errorf("assets: currency %s: %w", co.Currency, err)
		}
		v.exponents[co.Currency] = exp
	}
	return nil
}

// groupCurrency adds the tenant's group currency and its digits to the
// view.
func (v *view) groupCurrency(ctx context.Context, tx pgx.Tx, companies Companies) error {
	group, err := companies.GroupCurrency(ctx)
	if err != nil {
		return fmt.Errorf("assets: group currency: %w", err)
	}
	exp, err := currency.Exponent(ctx, tx, group, nil)
	if err != nil {
		return fmt.Errorf("assets: currency %s: %w", group, err)
	}
	v.group, v.exponents[group] = group, exp
	return nil
}

// values shows the asset's values in each ledger it has terms or
// movements in, in ledger order: cost, accumulated depreciation and net
// book value in local and group currency.
func (v view) values(a Asset, co Company) []gen.AssetLedgerValues {
	out := []gen.AssetLedgerValues{}
	lc := func(m money.Minor) gen.Money { return apitypes.Money(m, co.Currency, v.exponents[co.Currency]) }
	gc := func(m money.Minor) gen.Money { return apitypes.Money(m, v.group, v.exponents[v.group]) }
	for _, l := range v.ledgers {
		x, moved := a.Values[l.ID]
		if !moved && !slices.ContainsFunc(a.Terms, func(t Terms) bool { return t.LedgerID == l.ID }) {
			continue
		}
		out = append(out, gen.AssetLedgerValues{LedgerID: l.ID, LedgerCode: l.Code, LedgerName: l.Name, Cost: lc(x.Cost), CostGc: gc(x.CostGC),
			Accumulated: lc(x.Accumulated), AccumulatedGc: gc(x.AccumulatedGC), NetBookValue: lc(x.Cost + x.Accumulated),
			NetBookValueGc: gc(x.CostGC + x.AccumulatedGC)})
	}
	return out
}

// toAPI shows an asset with its class's name, its company's code, its
// centers named (when the caller may read them) and its terms in ledger
// order.
func (v view) toAPI(a Asset) gen.Asset {
	co := v.companies[a.LegalEntityID]
	out := gen.Asset{ID: a.ID, Version: a.Version, CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy, UpdatedAt: a.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(a.UpdatedBy), LegalEntityID: a.LegalEntityID, CompanyCode: co.Code, AssetNumber: a.Number,
		ClassCode: a.Class, ClassName: v.classes[a.Class], Description: a.Description, Status: a.Status, CapitalisedOn: apitypes.OptDate(a.CapitalisedOn),
		CostCenterID: apitypes.OptUUID(a.CostCenterID), ProfitCenterID: apitypes.OptUUID(a.ProfitCenterID), Terms: []gen.AssetTerms{}}
	if n, ok := v.name(a.CostCenterID); ok {
		out.CostCenterCode, out.CostCenterName = gen.NewOptString(n.Code), gen.NewOptString(n.Name)
	}
	if n, ok := v.name(a.ProfitCenterID); ok {
		out.ProfitCenterCode, out.ProfitCenterName = gen.NewOptString(n.Code), gen.NewOptString(n.Name)
	}
	for _, l := range v.ledgers {
		i := slices.IndexFunc(a.Terms, func(t Terms) bool { return t.LedgerID == l.ID })
		if i < 0 {
			continue
		}
		t := a.Terms[i]
		at := gen.AssetTerms{LedgerID: l.ID, LedgerCode: l.Code, LedgerName: l.Name, Method: t.Method, UsefulLifeMonths: t.UsefulLifeMonths,
			DecliningFactor: apitypes.OptString(t.DecliningFactor), ResidualValue: apitypes.Money(t.Residual, co.Currency, v.exponents[co.Currency]),
			DepreciationStart: apitypes.OptDate(t.DepreciationStart)}
		if t.booked() {
			at.BookedThroughYear, at.BookedThroughPeriod = gen.NewOptInt32(int32(*t.BookedYear)), gen.NewOptInt32(int32(*t.BookedPeriod))
		}
		out.Terms = append(out.Terms, at)
	}
	out.Values = v.values(a, co)
	return out
}

// name is a center's code and name, when the view has it.
func (v view) name(id *uuid.UUID) (CenterName, bool) {
	if id == nil {
		return CenterName{}, false
	}
	n, ok := v.centers[*id]
	return n, ok
}

// assetOut is an asset as a single read or write shows it, after the read
// or write in the request's transaction.
func (e *Endpoints) assetOut(ctx context.Context, a Asset) (gen.Asset, error) {
	tx, _ := reqctx.TxFrom(ctx) // the read or write before it found it
	v, err := e.viewOf(ctx, tx, []Asset{a})
	if err != nil {
		return gen.Asset{}, err
	}
	return v.toAPI(a), nil
}

func (e *Endpoints) changed(ctx context.Context, a Asset) (*gen.UpdateAssetOKHeaders, error) {
	out, err := e.assetOut(ctx, a)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateAssetOKHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Response: out}, nil
}

// CreateAsset implements POST /v1/assets.
func (e *Endpoints) CreateAsset(ctx context.Context, req *gen.AssetCreate, _ gen.CreateAssetParams) (gen.CreateAssetRes, error) {
	a, err := e.createAsset(ctx, NewAsset{LegalEntityID: req.LegalEntityID, Class: req.ClassCode, Description: req.Description,
		CostCenterID: apitypes.Ptr[uuid.UUID](req.CostCenterID), ProfitCenterID: apitypes.Ptr[uuid.UUID](req.ProfitCenterID)})
	if err != nil {
		return nil, err
	}
	out, err := e.assetOut(ctx, a)
	if err != nil {
		return nil, err
	}
	return &gen.AssetHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Location: gen.NewOptString("/v1/assets/" + a.ID.String()), Response: out}, nil
}

// GetAsset implements GET /v1/assets/{id}.
func (e *Endpoints) GetAsset(ctx context.Context, params gen.GetAssetParams) (gen.GetAssetRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	a, err := readAsset(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	out, err := e.assetOut(ctx, a)
	if err != nil {
		return nil, err
	}
	return &gen.GetAssetOKHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Response: out}, nil
}

// UpdateAsset implements PATCH /v1/assets/{id}.
func (e *Endpoints) UpdateAsset(ctx context.Context, req *gen.AssetUpdate, params gen.UpdateAssetParams) (gen.UpdateAssetRes, error) {
	ch := Patch{Description: apitypes.Ptr[string](req.Description), DepreciationStart: nilDate(req.DepreciationStart)}
	for _, t := range req.Terms {
		tp := TermsPatch{LedgerID: t.LedgerID, Method: apitypes.Ptr[string](t.Method), UsefulLifeMonths: apitypes.Ptr[int32](t.UsefulLifeMonths),
			DecliningFactor: apitypes.Ptr[string](t.DecliningFactor), DepreciationStart: nilDate(t.DepreciationStart)}
		if r, ok := t.ResidualValue.Get(); ok {
			tp.Residual, tp.ResidualCurrency = &r.Amount, &r.Currency
		}
		ch.Terms = append(ch.Terms, tp)
	}
	a, err := e.changeAsset(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return e.changed(ctx, a)
}

// nilDate is a merge patch's date: nil when absent, a pointer to nil when
// null.
func nilDate(d gen.OptNilDate) **time.Time {
	if !d.Set {
		return nil
	}
	var v *time.Time
	if !d.Null {
		v = &d.Value
	}
	return &v
}

// CancelAsset implements POST /v1/assets/{id}:cancel.
func (e *Endpoints) CancelAsset(ctx context.Context, params gen.CancelAssetParams) (gen.CancelAssetRes, error) {
	a, err := e.cancelAsset(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return e.changed(ctx, a)
}

var assetSort = pagination.Sort{Default: "asset_number", Fields: []pagination.Field{
	{Name: "asset_number", Column: "a.asset_number", Type: "text"}, {Name: "created_at", Column: "a.created_at", Type: "timestamptz"},
}}

// ListAssets implements GET /v1/assets: the assets of the legal entities
// the caller may act in, filtered by company, class, status, current cost
// center and number.
func (e *Endpoints) ListAssets(ctx context.Context, params gen.ListAssetsParams) (gen.ListAssetsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, cc := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CostCenterID)
	scope := fmt.Sprintf("listAssets|%s|%s|%s|%s|%s", le, params.ClassCode.Value, params.Status.Value, cc, params.AssetNumber.Value)
	ks, err := e.Pages.Keyset(assetSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(assetSelect+` WHERE ($1 OR a.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR a.legal_entity_id = $3)
		AND ($4 = '' OR a.class_code = $4) AND ($5 = '' OR a.status = $5) AND ($6::uuid IS NULL OR c.cost_center_id = $6)
		AND ($7 = '' OR a.asset_number = $7)`,
		[]any{access.All, access.IDs, le, params.ClassCode.Value, params.Status.Value, cc, params.AssetNumber.Value})
	rows, _ := tx.Query(ctx, q, args...)
	as, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Asset, error) { return scanAsset(r) })
	if err != nil {
		return nil, fmt.Errorf("assets: list assets: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, as, func(a Asset) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return a.CreatedAt.UTC().Format(time.RFC3339Nano), a.ID
		}
		return a.Number, a.ID
	}, func(page []Asset) (view, error) { return e.pageView(ctx, tx, page) }, func(a Asset, v view) gen.Asset { return v.toAPI(a) })
	if err != nil {
		return nil, err
	}
	return &gen.AssetPage{Items: items, NextPageToken: next}, nil
}

// pageView reads the terms of a page's assets into them and what they are
// shown with.
func (e *Endpoints) pageView(ctx context.Context, tx pgx.Tx, page []Asset) (view, error) {
	ids := make([]uuid.UUID, len(page))
	for i, a := range page {
		ids[i] = a.ID
	}
	terms, err := readTerms(ctx, tx, ids)
	if err != nil {
		return view{}, err
	}
	values, err := readValues(ctx, tx, ids)
	if err != nil {
		return view{}, err
	}
	for i := range page {
		page[i].Terms, page[i].Values = terms[page[i].ID], values[page[i].ID]
	}
	return e.viewOf(ctx, tx, page)
}
