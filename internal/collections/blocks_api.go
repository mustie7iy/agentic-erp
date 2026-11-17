package collections

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// blockRefs reads what the blocks name by id, once each: their
// companies, customers and the people who released them.
func (e *Endpoints) blockRefs(ctx context.Context, blocks []Block) (caseRefs, error) {
	les, customers := make([]uuid.UUID, len(blocks)), make([]uuid.UUID, len(blocks))
	for i, b := range blocks {
		les[i], customers[i] = b.LegalEntityID, b.CustomerID
	}
	return e.named(ctx, "dunning blocks", les, customers, idref.Of(blocks, func(b Block) *uuid.UUID { return b.ReleasedBy }))
}

// blockToAPI is a block as the API gives it: its company by code and
// name, its customer by number and name, who released it by name.
func blockToAPI(b Block, r caseRefs) gen.DunningBlock {
	le := r.companies[b.LegalEntityID]
	out := gen.DunningBlock{ID: b.ID, Version: b.Version, CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy, UpdatedAt: b.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(b.UpdatedBy), LegalEntityID: b.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, CustomerID: b.CustomerID,
		CustomerNumber: b.CustomerNumber, CustomerName: r.customers[b.CustomerID].Name, DocumentNumber: apitypes.OptString(b.DocumentNumber),
		Reason: b.Reason, ValidTo: b.ValidTo, Status: b.Status(), ReleasedAt: apitypes.OptDateTime(b.ReleasedAt), ReleasedBy: apitypes.OptUUID(b.ReleasedBy)}
	if b.Item != nil {
		out.FiscalYear, out.JournalEntryID, out.LineNo = gen.NewOptInt32(int32(b.Item.FiscalYear)), gen.NewOptUUID(b.Item.EntryID), gen.NewOptInt32(b.Item.LineNo)
	}
	if b.ReleasedBy != nil {
		out.ReleasedByName.SetTo(r.people[*b.ReleasedBy])
	}
	return out
}

// shownBlock is a block as the API gives it, with what it names.
func (e *Endpoints) shownBlock(ctx context.Context, b Block) (gen.DunningBlock, error) {
	r, err := e.blockRefs(ctx, []Block{b})
	if err != nil {
		return gen.DunningBlock{}, err
	}
	return blockToAPI(b, r), nil
}

// blockItem is the journal line a request names, nil for none; a line
// named in part is refused.
func blockItem(req *gen.DunningBlockCreate) (*ledger.ItemRef, error) {
	if !req.FiscalYear.Set && !req.JournalEntryID.Set && !req.LineNo.Set {
		return nil, nil
	}
	if !req.FiscalYear.Set || !req.JournalEntryID.Set || !req.LineNo.Set {
		return nil, problem.Unprocessable("request.validation_failed", "The request is not valid: an item is named by fiscal_year, journal_entry_id "+
			"and line_no together, and a block of the whole customer names none of them.",
			problem.FieldError{Field: "journal_entry_id", Code: "invalid", Message: "with fiscal_year and line_no, or none of them"})
	}
	// The contract bounds the year to 1900-2100.
	return &ledger.ItemRef{FiscalYear: int16(req.FiscalYear.Value), EntryID: req.JournalEntryID.Value, LineNo: req.LineNo.Value}, nil
}

// CreateDunningBlock implements POST /v1/dunning-blocks.
func (e *Endpoints) CreateDunningBlock(ctx context.Context, req *gen.DunningBlockCreate, _ gen.CreateDunningBlockParams) (gen.CreateDunningBlockRes, error) {
	item, err := blockItem(req)
	if err != nil {
		return nil, err
	}
	b, err := e.PlaceBlock(ctx, BlockInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, Item: item, Reason: req.Reason, ValidTo: req.ValidTo})
	if err != nil {
		return nil, err
	}
	out, err := e.shownBlock(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.DunningBlockHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Location: gen.NewOptString("/v1/dunning-blocks/" + b.ID.String()),
		Response: out}, nil
}

// GetDunningBlock implements GET /v1/dunning-blocks/{id}.
func (e *Endpoints) GetDunningBlock(ctx context.Context, params gen.GetDunningBlockParams) (gen.GetDunningBlockRes, error) {
	b, err := GetBlock(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.shownBlock(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.GetDunningBlockOKHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Response: out}, nil
}

// ReleaseDunningBlock implements POST /v1/dunning-blocks/{id}:release.
func (e *Endpoints) ReleaseDunningBlock(ctx context.Context, params gen.ReleaseDunningBlockParams) (gen.ReleaseDunningBlockRes, error) {
	b, err := e.ReleaseBlock(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	out, err := e.shownBlock(ctx, b)
	if err != nil {
		return nil, err
	}
	return &gen.ReleaseDunningBlockOKHeaders{ETag: gen.NewOptString(etag.Format(b.Version)), Response: out}, nil
}

var blockSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "b.created_at", Type: "timestamptz"},
}}

// ListDunningBlocks implements GET /v1/dunning-blocks: the blocks of the
// companies the caller may act in, the latest first.
func (e *Endpoints) ListDunningBlocks(ctx context.Context, params gen.ListDunningBlocksParams) (gen.ListDunningBlocksRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	scope := fmt.Sprintf("listDunningBlocks|%s|%s|%s", le, customer, params.Status.Value)
	ks, err := e.Pages.Keyset(blockSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("collections: page of dunning blocks: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: company access for dunning blocks: %w", err)
	}
	q, args := ks.SQL(blockSelect+` WHERE ($1 OR b.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR b.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR b.customer_id = $4) AND ($5 = '' OR ($5 = 'active' AND b.released_at IS NULL) OR ($5 = 'released' AND b.released_at IS NOT NULL))`,
		[]any{access.All, access.IDs, le, customer, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	blocks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Block, error) { return scanBlock(r) })
	if err != nil {
		return nil, fmt.Errorf("collections: list dunning blocks: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, blocks, func(b Block) (string, uuid.UUID) { return pagination.Instant(b.CreatedAt), b.ID },
		func(page []Block) (caseRefs, error) { return e.blockRefs(ctx, page) }, blockToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.DunningBlockPage{Items: items, NextPageToken: next}, nil
}
