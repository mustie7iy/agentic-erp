package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ImpairAsset implements POST /v1/assets/{id}:impair.
func (e *Endpoints) ImpairAsset(ctx context.Context, req *gen.AssetImpairmentCreate, params gen.ImpairAssetParams) (gen.ImpairAssetRes, error) {
	if req.Kind != ImpairmentLoss && req.Kind != ImpairmentReversal {
		return nil, validation("kind", "loss or reversal")
	}
	amount, err := e.amountOf(ctx, req.Amount, "amount")
	if err != nil {
		return nil, err
	}
	if amount <= 0 {
		return nil, validation("amount.amount", "above zero")
	}
	in := NewImpairment{LedgerID: apitypes.Ptr[uuid.UUID](req.LedgerID), Kind: req.Kind, Amount: amount, Currency: req.Amount.Currency,
		ValueDate: apitypes.Ptr[time.Time](req.ValueDate), PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Text: apitypes.Ptr[string](req.Text)}
	q, err := e.impairAsset(ctx, params.ID, etag.IfMatch(params.IfMatch), in)
	if err != nil {
		return nil, err
	}
	out, err := e.impairmentShown(ctx, q)
	if err != nil {
		return nil, err
	}
	return &gen.AssetImpairmentHeaders{Location: gen.NewOptString("/v1/asset-impairments/" + q.ID.String()), Response: out}, nil
}

// GetAssetImpairment implements GET /v1/asset-impairments/{id}.
func (e *Endpoints) GetAssetImpairment(ctx context.Context, params gen.GetAssetImpairmentParams) (gen.GetAssetImpairmentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	q, err := readImpairment(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.impairmentShown(ctx, q)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// impairmentShown shows one impairment.
func (e *Endpoints) impairmentShown(ctx context.Context, q Impairment) (gen.AssetImpairment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return gen.AssetImpairment{}, err
	}
	v, err := e.impairmentView(ctx, tx, []Impairment{q})
	if err != nil {
		return gen.AssetImpairment{}, err
	}
	return v.impairmentAPI(q), nil
}

var impairmentSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "q.posting_date", Type: "date"}, {Name: "created_at", Column: "q.created_at", Type: "timestamptz"},
}}

// ListAssetImpairments implements GET /v1/asset-impairments: the
// impairments of the legal entities the caller may act in, filtered by
// company, asset, ledger, kind and posting dates.
func (e *Endpoints) ListAssetImpairments(ctx context.Context, params gen.ListAssetImpairmentsParams) (gen.ListAssetImpairmentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, asset := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.AssetID)
	ledger := apitypes.Ptr[uuid.UUID](params.LedgerID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listAssetImpairments|%s|%s|%s|%s|%s|%s", le, asset, ledger, params.Kind.Value, scopeDate(from), scopeDate(to))
	ks, err := e.Pages.Keyset(impairmentSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// The keyset pages on (column, id): selected from a subquery, id is the
	// impairment's alone, not also its asset's.
	q, args := ks.SQL(`SELECT * FROM (`+impairmentSelect+`) q WHERE ($1 OR q.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR q.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR q.asset_id = $4) AND ($5::uuid IS NULL OR q.ledger_id = $5) AND ($6 = '' OR q.kind = $6)
		AND ($7::date IS NULL OR q.posting_date >= $7) AND ($8::date IS NULL OR q.posting_date <= $8)`,
		[]any{access.All, access.IDs, le, asset, ledger, params.Kind.Value, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	qs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Impairment, error) { return scanImpairment(r) })
	if err != nil {
		return nil, fmt.Errorf("assets: list impairments: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, qs, func(q Impairment) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return pagination.Instant(q.CreatedAt), q.ID
		}
		return q.PostingDate.Format(time.DateOnly), q.ID
	}, func(page []Impairment) (docView, error) {
		if err := impairmentEntries(ctx, tx, page); err != nil {
			return docView{}, err
		}
		return e.impairmentView(ctx, tx, page)
	}, func(q Impairment, v docView) gen.AssetImpairment { return v.impairmentAPI(q) })
	if err != nil {
		return nil, err
	}
	return &gen.AssetImpairmentPage{Items: items, NextPageToken: next}, nil
}

// impairmentView reads what impairments are shown with.
func (e *Endpoints) impairmentView(ctx context.Context, tx pgx.Tx, qs []Impairment) (docView, error) {
	var companies []uuid.UUID
	var currencies []string
	for _, q := range qs {
		companies, currencies = append(companies, q.LegalEntityID), append(currencies, q.Currency, q.CurrencyGC)
	}
	return e.docViewOf(ctx, tx, companies, currencies, nil)
}

// impairmentAPI shows an impairment with its ledger by code and name and
// the carrying amount after it.
func (v docView) impairmentAPI(q Impairment) gen.AssetImpairment {
	lc := func(m money.Minor) gen.Money { return v.money(m, q.Currency) }
	change, _ := q.change()
	out := gen.AssetImpairment{ID: q.ID, LegalEntityID: q.LegalEntityID, CompanyCode: v.companies[q.LegalEntityID].Code, AssetID: q.AssetID,
		AssetNumber: q.AssetNumber, AssetDescription: q.AssetDescription, LedgerID: q.LedgerID, Kind: q.Kind, ValueDate: q.ValueDate,
		PostingDate: q.PostingDate, FiscalYear: int32(q.FiscalYear), Period: int32(q.Period), Amount: lc(q.Amount), AmountGc: v.money(q.AmountGC, q.CurrencyGC),
		CarryingAmount: lc(q.Carrying), CarryingAmountAfter: lc(q.Carrying + change), UnimpairedAmount: lc(q.Unimpaired), Text: apitypes.OptString(q.Text),
		Entries: v.entries(q.Entries), CreatedAt: q.CreatedAt, CreatedBy: q.CreatedBy}
	// A ledger is the tenant's and never removed, so the view lists it.
	for _, l := range v.ledgers {
		if l.ID == q.LedgerID {
			out.LedgerCode, out.LedgerName = l.Code, l.Name
		}
	}
	return out
}
