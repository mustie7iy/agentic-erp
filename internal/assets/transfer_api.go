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

// CreateAssetTransfer implements POST /v1/asset-transfers.
func (e *Endpoints) CreateAssetTransfer(ctx context.Context, req *gen.AssetTransferCreate, _ gen.CreateAssetTransferParams) (gen.CreateAssetTransferRes, error) {
	q, out, err := e.shownTransfer(ctx, func(tx pgx.Tx, p reqctx.Principal) (Transfer, error) { return e.createTransfer(ctx, tx, p, req) })
	if err != nil {
		return nil, err
	}
	return &gen.AssetTransferHeaders{ETag: transferTag(q), Location: gen.NewOptString("/v1/asset-transfers/" + q.ID.String()), Response: out}, nil
}

// createTransfer reads the request's transfer and posts it.
func (e *Endpoints) createTransfer(ctx context.Context, tx pgx.Tx, p reqctx.Principal, req *gen.AssetTransferCreate) (Transfer, error) {
	in, err := e.newTransfer(ctx, req)
	if err != nil {
		return Transfer{}, err
	}
	return e.postTransfer(ctx, tx, p, in)
}

// transferTag is a transfer's ETag.
func transferTag(q Transfer) gen.OptString { return gen.NewOptString(etag.Format(q.Version)) }

// shownTransfer runs a write or read of a transfer in the request's
// transaction and shows the transfer it returns.
func (e *Endpoints) shownTransfer(ctx context.Context, fn func(tx pgx.Tx, p reqctx.Principal) (Transfer, error)) (Transfer, gen.AssetTransfer, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Transfer{}, gen.AssetTransfer{}, err
	}
	q, err := fn(tx, p)
	if err != nil {
		return Transfer{}, gen.AssetTransfer{}, err
	}
	v, err := e.transferView(ctx, tx, []Transfer{q})
	if err != nil {
		return Transfer{}, gen.AssetTransfer{}, err
	}
	return q, v.transferAPI(q), nil
}

// newTransfer reads a transfer from its request (422
// request.validation_failed on the field otherwise): a center transfer
// with a cost or profit center and no receiver or share; an asset
// transfer with its receiver and no centers, its share as a retirement's.
func (e *Endpoints) newTransfer(ctx context.Context, req *gen.AssetTransferCreate) (NewTransfer, error) {
	in := NewTransfer{AssetID: req.AssetID, Kind: req.Kind, CostCenterID: apitypes.Ptr[uuid.UUID](req.CostCenterID),
		ProfitCenterID: apitypes.Ptr[uuid.UUID](req.ProfitCenterID), ToAssetID: apitypes.Ptr[uuid.UUID](req.ToAssetID),
		ValueDate: apitypes.Ptr[time.Time](req.ValueDate), PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Text: apitypes.Ptr[string](req.Text)}
	switch {
	case in.Kind != TransferCenter && in.Kind != TransferAsset:
		return NewTransfer{}, validation("kind", "center or asset")
	case in.Kind == TransferCenter && in.ToAssetID != nil:
		return NewTransfer{}, validation("to_asset_id", "none for a center transfer")
	case in.Kind == TransferCenter && req.Share.Set:
		return NewTransfer{}, validation("share", "none for a center transfer")
	case in.Kind == TransferAsset && in.ToAssetID == nil:
		return NewTransfer{}, validation("to_asset_id", "the asset the share goes to")
	case in.Kind == TransferAsset && (in.CostCenterID != nil || in.ProfitCenterID != nil):
		return NewTransfer{}, validation("cost_center_id", "none for an asset transfer: the receiver keeps its centers")
	}
	s, ok := req.Share.Get()
	if !ok {
		return in, nil
	}
	var r NewRetirement
	if err := e.shareOf(ctx, s, &r); err != nil {
		return NewTransfer{}, err
	}
	in.Percent, in.Cost, in.CostCurrency = r.Percent, r.Cost, r.CostCurrency
	return in, nil
}

// ReverseAssetTransfer implements POST /v1/asset-transfers/{id}:reverse.
func (e *Endpoints) ReverseAssetTransfer(ctx context.Context, req *gen.AssetTransferReverse, params gen.ReverseAssetTransferParams) (gen.ReverseAssetTransferRes, error) {
	on, ifMatch := apitypes.Ptr[time.Time](req.PostingDate), etag.IfMatch(params.IfMatch)
	q, out, err := e.shownTransfer(ctx, func(tx pgx.Tx, p reqctx.Principal) (Transfer, error) {
		return e.reverseTransfer(ctx, tx, p, params.ID, ifMatch, on, req.Reason)
	})
	if err != nil {
		return nil, err
	}
	return &gen.ReverseAssetTransferOKHeaders{ETag: transferTag(q), Response: out}, nil
}

// GetAssetTransfer implements GET /v1/asset-transfers/{id}.
func (e *Endpoints) GetAssetTransfer(ctx context.Context, params gen.GetAssetTransferParams) (gen.GetAssetTransferRes, error) {
	q, out, err := e.shownTransfer(ctx, func(tx pgx.Tx, _ reqctx.Principal) (Transfer, error) { return readTransfer(ctx, tx, params.ID) })
	if err != nil {
		return nil, err
	}
	return &gen.GetAssetTransferOKHeaders{ETag: transferTag(q), Response: out}, nil
}

var transferSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "q.posting_date", Type: "date"}, {Name: "created_at", Column: "q.created_at", Type: "timestamptz"},
}}

// ListAssetTransfers implements GET /v1/asset-transfers: the transfers of
// the legal entities the caller may act in, filtered by company, asset (as
// the asset transferred or the receiver), kind, posting dates and status.
func (e *Endpoints) ListAssetTransfers(ctx context.Context, params gen.ListAssetTransfersParams) (gen.ListAssetTransfersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, asset := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.AssetID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listAssetTransfers|%s|%s|%s|%s|%s|%s", le, asset, params.Kind.Value, scopeDate(from), scopeDate(to), params.Status.Value)
	ks, err := e.Pages.Keyset(transferSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// The keyset pages on (column, id): selected from a subquery, id is the
	// transfer's alone, not also its assets'.
	q, args := ks.SQL(`SELECT * FROM (`+transferSelect+`) q WHERE ($1 OR q.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR q.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR $4 IN (q.asset_id, q.to_asset_id)) AND ($5 = '' OR q.kind = $5) AND ($6::date IS NULL OR q.posting_date >= $6)
		AND ($7::date IS NULL OR q.posting_date <= $7) AND ($8 = '' OR q.status = $8)`,
		[]any{access.All, access.IDs, le, asset, params.Kind.Value, from, to, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	qs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Transfer, error) { return scanTransfer(r) })
	if err != nil {
		return nil, fmt.Errorf("assets: list transfers: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, qs, func(q Transfer) (string, uuid.UUID) {
		if ks.Field() == "created_at" {
			return q.CreatedAt.UTC().Format(time.RFC3339Nano), q.ID
		}
		return q.PostingDate.Format(time.DateOnly), q.ID
	}, func(page []Transfer) (transferView, error) {
		if err := transferDetails(ctx, tx, page); err != nil {
			return transferView{}, err
		}
		return e.transferView(ctx, tx, page)
	}, func(q Transfer, v transferView) gen.AssetTransfer { return v.transferAPI(q) })
	if err != nil {
		return nil, err
	}
	return &gen.AssetTransferPage{Items: items, NextPageToken: next}, nil
}

// transferView is what transfers are shown with: a document's view and
// the names of their centers (nil when the caller may not read centers).
type transferView struct {
	docView
	centers map[uuid.UUID]CenterName
}

// transferView reads what transfers are shown with.
func (e *Endpoints) transferView(ctx context.Context, tx pgx.Tx, qs []Transfer) (transferView, error) {
	var companies []uuid.UUID
	var currencies []string
	var centers []uuid.UUID
	for _, q := range qs {
		companies, currencies = append(companies, q.LegalEntityID), append(currencies, q.Currency, q.CurrencyGC)
		for _, id := range []*uuid.UUID{q.FromCostCenterID, q.FromProfitCenterID, q.CostCenterID, q.ProfitCenterID} {
			if id != nil && !slices.Contains(centers, *id) {
				centers = append(centers, *id)
			}
		}
	}
	d, err := e.docViewOf(ctx, tx, companies, currencies, nil)
	if err != nil {
		return transferView{}, err
	}
	v := transferView{docView: d}
	may, err := authz.MayUse(ctx, centerReadPermission)
	if err != nil || !may || len(centers) == 0 {
		return v, err
	}
	if v.centers, err = e.Centers.Names(ctx, centers); err != nil {
		return transferView{}, fmt.Errorf("assets: names of %d centers: %w", len(centers), err)
	}
	return v, nil
}

// centersAPI shows a center transfer's centers, named when the caller
// may read them.
func (v transferView) centersAPI(cc, pc *uuid.UUID) gen.OptAssetTransferCenters {
	out := gen.AssetTransferCenters{CostCenterID: apitypes.OptUUID(cc), ProfitCenterID: apitypes.OptUUID(pc)}
	if n, ok := v.centers[orNil(cc)]; ok {
		out.CostCenterCode, out.CostCenterName = gen.NewOptString(n.Code), gen.NewOptString(n.Name)
	}
	if n, ok := v.centers[orNil(pc)]; ok {
		out.ProfitCenterCode, out.ProfitCenterName = gen.NewOptString(n.Code), gen.NewOptString(n.Name)
	}
	return gen.NewOptAssetTransferCenters(out)
}

// transferAPI shows a transfer, what it moved per ledger in ledger order.
func (v transferView) transferAPI(q Transfer) gen.AssetTransfer {
	lc := func(m money.Minor) gen.Money { return v.money(m, q.Currency) }
	gc := func(m money.Minor) gen.Money { return v.money(m, q.CurrencyGC) }
	out := gen.AssetTransfer{ID: q.ID, Version: q.Version, CreatedAt: q.CreatedAt, CreatedBy: q.CreatedBy, UpdatedAt: q.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(q.UpdatedBy), LegalEntityID: q.LegalEntityID, CompanyCode: v.companies[q.LegalEntityID].Code, Kind: q.Kind,
		AssetID: q.AssetID, AssetNumber: q.AssetNumber, AssetDescription: q.AssetDescription, ToAssetID: apitypes.OptUUID(q.ToAssetID),
		ToAssetNumber: apitypes.OptString(q.ToAssetNumber), ToAssetDescription: apitypes.OptString(q.ToAssetDescription),
		ToClassCode: apitypes.OptString(q.ToClass), ValueDate: q.ValueDate, PostingDate: q.PostingDate, FiscalYear: int32(q.FiscalYear),
		Period: int32(q.Period), SharePercent: apitypes.OptString(q.SharePercent), Text: apitypes.OptString(q.Text), Status: q.Status,
		Ledgers: []gen.AssetTransferLedger{}, Entries: v.entries(q.Entries)}
	if c := q.ShareCost; c != nil {
		out.ShareCost = gen.NewOptMoney(lc(*c))
	}
	if q.Kind == TransferCenter {
		out.From, out.To = v.centersAPI(q.FromCostCenterID, q.FromProfitCenterID), v.centersAPI(q.CostCenterID, q.ProfitCenterID)
		out.ProfitCenterChange = gen.NewOptBool(q.profitCenterChange())
	}
	full := true
	for _, l := range v.ledgers {
		i := slices.IndexFunc(q.Ledgers, func(t TransferValues) bool { return t.LedgerID == l.ID })
		if i < 0 {
			continue
		}
		t := q.Ledgers[i]
		full = full && t.Share != nil && *t.Share == "1"
		out.Ledgers = append(out.Ledgers, gen.AssetTransferLedger{LedgerID: l.ID, LedgerCode: l.Code, LedgerName: l.Name, Share: apitypes.OptString(t.Share),
			CatchUp: lc(t.CatchUp), CatchUpGc: gc(t.CatchUpGC), CatchUpPostingDate: t.CatchUpOn, Cost: lc(t.Cost), CostGc: gc(t.CostGC),
			Accumulated: lc(t.Accumulated), AccumulatedGc: gc(t.AccumulatedGC), Residual: lc(t.Residual), NetBookValue: lc(t.Cost + t.Accumulated),
			NetBookValueGc: gc(t.CostGC + t.AccumulatedGC)})
	}
	if q.Kind == TransferAsset {
		out.Full = gen.NewOptBool(full)
	}
	if r := q.Reversal; r != nil {
		out.Reversal = gen.NewOptAssetTransferReversal(gen.AssetTransferReversal{PostingDate: r.PostingDate, Reason: r.Reason})
	}
	return out
}
