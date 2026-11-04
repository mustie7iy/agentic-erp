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
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CreateAssetRetirement implements POST /v1/asset-retirements.
func (e *Endpoints) CreateAssetRetirement(ctx context.Context, req *gen.AssetRetirementCreate, _ gen.CreateAssetRetirementParams) (gen.CreateAssetRetirementRes, error) {
	q, out, err := e.shown(ctx, func(tx pgx.Tx, p reqctx.Principal) (Retirement, error) {
		in, err := e.newRetirement(ctx, req)
		if err != nil {
			return Retirement{}, err
		}
		return e.postRetirement(ctx, tx, p, in)
	})
	if err != nil {
		return nil, err
	}
	return &gen.AssetRetirementHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Location: gen.NewOptString("/v1/asset-retirements/" + q.ID.String()),
		Response: out}, nil
}

// shown runs a write or read of a retirement in the request's
// transaction and shows the retirement it returns.
func (e *Endpoints) shown(ctx context.Context, fn func(tx pgx.Tx, p reqctx.Principal) (Retirement, error)) (Retirement, gen.AssetRetirement, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Retirement{}, gen.AssetRetirement{}, err
	}
	q, err := fn(tx, p)
	if err != nil {
		return Retirement{}, gen.AssetRetirement{}, err
	}
	v, err := e.retirementView(ctx, tx, []Retirement{q})
	if err != nil {
		return Retirement{}, gen.AssetRetirement{}, err
	}
	return q, v.retirementAPI(q), nil
}

// newRetirement reads a retirement from its request (422
// request.validation_failed on the field otherwise): a sale with its
// proceeds, zero or more, or a scrapping without; a share of a percent
// above 0 and at most 100 with up to four decimals, or of a cost above
// zero, not both.
func (e *Endpoints) newRetirement(ctx context.Context, req *gen.AssetRetirementCreate) (NewRetirement, error) {
	in := NewRetirement{AssetID: req.AssetID, Kind: req.Kind, ValueDate: apitypes.Ptr[time.Time](req.ValueDate),
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Text: apitypes.Ptr[string](req.Text)}
	m, given := req.Proceeds.Get()
	switch {
	case in.Kind != RetirementSale && in.Kind != RetirementScrapping:
		return NewRetirement{}, validation("kind", "sale or scrapping")
	case in.Kind == RetirementSale && !given:
		return NewRetirement{}, validation("proceeds", "the proceeds of a sale, zero or more")
	case in.Kind == RetirementScrapping && given:
		return NewRetirement{}, validation("proceeds", "none for a scrapping")
	}
	if given {
		amount, err := e.amountOf(ctx, m, "proceeds")
		if err != nil {
			return NewRetirement{}, err
		}
		if amount < 0 {
			return NewRetirement{}, validation("proceeds.amount", "zero or more")
		}
		in.Proceeds, in.ProceedsCurrency = amount, m.Currency
	}
	if s, ok := req.Share.Get(); ok {
		return in, e.shareOf(ctx, s, &in)
	}
	return in, nil
}

// shareOf reads a retirement's share: a percent or a cost, one of the two.
func (e *Endpoints) shareOf(ctx context.Context, s gen.AssetRetirementShare, in *NewRetirement) error {
	if s.Percent.Set == s.Cost.Set {
		return validation("share", "a percent or a cost, one of the two; no share for all of it")
	}
	if p, ok := s.Percent.Get(); ok {
		v, err := money.ParseDecimal(p, 4) // in ten-thousandths of a percent
		if err != nil || v <= 0 || v > hundredPercent {
			return validation("share.percent", "a plain decimal above 0 and at most 100, with up to four decimals")
		}
		in.Percent = (*int64)(&v)
		return nil
	}
	cost, err := e.amountOf(ctx, s.Cost.Value, "share.cost")
	if err != nil {
		return err
	}
	if cost <= 0 {
		return validation("share.cost.amount", "above zero")
	}
	in.Cost, in.CostCurrency = &cost, s.Cost.Value.Currency
	return nil
}

// ReverseAssetRetirement implements POST /v1/asset-retirements/{id}:reverse.
func (e *Endpoints) ReverseAssetRetirement(ctx context.Context, req *gen.AssetRetirementReverse, params gen.ReverseAssetRetirementParams) (gen.ReverseAssetRetirementRes, error) {
	q, out, err := e.shown(ctx, func(tx pgx.Tx, p reqctx.Principal) (Retirement, error) {
		return e.reverseRetirement(ctx, tx, p, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[time.Time](req.PostingDate), req.Reason)
	})
	if err != nil {
		return nil, err
	}
	return &gen.ReverseAssetRetirementOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

// GetAssetRetirement implements GET /v1/asset-retirements/{id}.
func (e *Endpoints) GetAssetRetirement(ctx context.Context, params gen.GetAssetRetirementParams) (gen.GetAssetRetirementRes, error) {
	q, out, err := e.shown(ctx, func(tx pgx.Tx, _ reqctx.Principal) (Retirement, error) { return readRetirement(ctx, tx, params.ID) })
	if err != nil {
		return nil, err
	}
	return &gen.GetAssetRetirementOKHeaders{ETag: gen.NewOptString(etag.Format(q.Version)), Response: out}, nil
}

var retirementSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "q.posting_date", Type: "date"}, {Name: "created_at", Column: "q.created_at", Type: "timestamptz"},
}}

// ListAssetRetirements implements GET /v1/asset-retirements: the
// retirements of the legal entities the caller may act in, filtered by
// company, asset, kind, posting dates and status.
func (e *Endpoints) ListAssetRetirements(ctx context.Context, params gen.ListAssetRetirementsParams) (gen.ListAssetRetirementsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, asset := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.AssetID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listAssetRetirements|%s|%s|%s|%s|%s|%s", le, asset, params.Kind.Value, scopeDate(from), scopeDate(to), params.Status.Value)
	ks, err := e.Pages.Keyset(retirementSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// The keyset pages on (column, id): selected from a subquery, id is the
	// retirement's alone, not also its asset's.
	q, args := ks.SQL(`SELECT * FROM (`+retirementSelect+`) q WHERE ($1 OR q.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR q.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR q.asset_id = $4) AND ($5 = '' OR q.kind = $5) AND ($6::date IS NULL OR q.posting_date >= $6)
		AND ($7::date IS NULL OR q.posting_date <= $7) AND ($8 = '' OR q.status = $8)`,
		[]any{access.All, access.IDs, le, asset, params.Kind.Value, from, to, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	qs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Retirement, error) { return scanRetirement(r) })
	if err != nil {
		return nil, fmt.Errorf("assets: list retirements: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, qs, func(q Retirement) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return q.CreatedAt.UTC().Format(time.RFC3339Nano), q.ID
		}
		return q.PostingDate.Format(time.DateOnly), q.ID
	}, func(page []Retirement) (docView, error) { return e.retirementPage(ctx, tx, page) }, func(q Retirement, v docView) gen.AssetRetirement {
		return v.retirementAPI(q)
	})
	if err != nil {
		return nil, err
	}
	return &gen.AssetRetirementPage{Items: items, NextPageToken: next}, nil
}

// retirementPage reads what a page's retirements took and their entries
// into them, and what they are shown with.
func (e *Endpoints) retirementPage(ctx context.Context, tx pgx.Tx, page []Retirement) (docView, error) {
	if err := retirementDetails(ctx, tx, page); err != nil {
		return docView{}, err
	}
	return e.retirementView(ctx, tx, page)
}

// retirementView reads what retirements are shown with.
func (e *Endpoints) retirementView(ctx context.Context, tx pgx.Tx, qs []Retirement) (docView, error) {
	var companies []uuid.UUID
	var currencies []string
	for _, q := range qs {
		companies, currencies = append(companies, q.LegalEntityID), append(currencies, q.Currency, q.CurrencyGC)
	}
	return e.docViewOf(ctx, tx, companies, currencies, nil)
}

// retirementAPI shows a retirement, what it took per ledger in ledger
// order.
func (v docView) retirementAPI(q Retirement) gen.AssetRetirement {
	lc := func(m money.Minor) gen.Money { return v.money(m, q.Currency) }
	gc := func(m money.Minor) gen.Money { return v.money(m, q.CurrencyGC) }
	out := gen.AssetRetirement{ID: q.ID, Version: q.Version, CreatedAt: q.CreatedAt, CreatedBy: q.CreatedBy, UpdatedAt: q.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(q.UpdatedBy), LegalEntityID: q.LegalEntityID, CompanyCode: v.companies[q.LegalEntityID].Code, AssetID: q.AssetID,
		AssetNumber: q.AssetNumber, AssetDescription: q.AssetDescription, Kind: q.Kind, ValueDate: q.ValueDate, PostingDate: q.PostingDate,
		FiscalYear: int32(q.FiscalYear), Period: int32(q.Period), Full: true, SharePercent: apitypes.OptString(q.SharePercent), Proceeds: lc(q.Proceeds),
		ProceedsGc: gc(q.ProceedsGC), Text: apitypes.OptString(q.Text), Status: q.Status, Ledgers: []gen.AssetRetirementLedger{}, Entries: v.entries(q.Entries)}
	if c := q.ShareCost; c != nil {
		out.ShareCost = gen.NewOptMoney(lc(*c))
	}
	for _, l := range v.ledgers {
		i := slices.IndexFunc(q.Ledgers, func(r RetiredValues) bool { return r.LedgerID == l.ID })
		if i < 0 {
			continue
		}
		r := q.Ledgers[i]
		gain, gainGC := q.gain(r)
		out.Full = out.Full && r.Share == "1"
		out.Ledgers = append(out.Ledgers, gen.AssetRetirementLedger{LedgerID: l.ID, LedgerCode: l.Code, LedgerName: l.Name, Share: r.Share,
			CatchUp: lc(r.CatchUp), CatchUpGc: gc(r.CatchUpGC), Cost: lc(r.Cost), CostGc: gc(r.CostGC), Accumulated: lc(r.Accumulated),
			AccumulatedGc: gc(r.AccumulatedGC), Residual: lc(r.Residual), NetBookValue: lc(r.Cost + r.Accumulated),
			NetBookValueGc: gc(r.CostGC + r.AccumulatedGC), Gain: lc(gain), GainGc: gc(gainGC)})
	}
	if r := q.Reversal; r != nil {
		out.Reversal = gen.NewOptAssetRetirementReversal(gen.AssetRetirementReversal{PostingDate: r.PostingDate, Reason: r.Reason})
	}
	return out
}
