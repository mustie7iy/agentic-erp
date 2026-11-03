package closing

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
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

var commentSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "v.created_at", Type: "timestamptz"}}}

// GetVarianceComment implements GET /v1/variance-comments/{id}.
func (e *Endpoints) GetVarianceComment(ctx context.Context, params gen.GetVarianceCommentParams) (gen.GetVarianceCommentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := readComment(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.commentOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetVarianceCommentOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// ListVarianceComments implements GET /v1/variance-comments: the comments
// of the closes of the legal entities the caller may act in, oldest
// first.
func (e *Endpoints) ListVarianceComments(ctx context.Context, params gen.ListVarianceCommentsParams) (gen.ListVarianceCommentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	closeID, account, session := apitypes.Ptr[uuid.UUID](params.CloseID), apitypes.Ptr[uuid.UUID](params.AccountID), apitypes.Ptr[uuid.UUID](params.SessionID)
	scope := fmt.Sprintf("listVarianceComments|%v|%s|%v|%s|%v", closeID, params.Basis.Value, account, params.Status.Value, session)
	ks, err := e.Pages.Keyset(commentSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// A subquery, so that the keyset's id is the comment's alone.
	q, args := ks.SQL(`SELECT * FROM (`+commentSelect+`) v WHERE ($1 OR v.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR v.close_id = $3) AND ($4 = '' OR v.basis = $4)
		AND ($5::uuid IS NULL OR v.account_id = $5) AND ($6 = '' OR v.status = $6) AND ($7::uuid IS NULL OR v.session_id = $7)`,
		[]any{access.All, access.IDs, closeID, params.Basis.Value, account, params.Status.Value, session})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (comment, error) { return scanComment(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: list variance comments: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, cs, func(c comment) (string, uuid.UUID) { return c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ID },
		func(page []comment) (commentView, error) { return e.commentView(ctx, tx, page) }, func(c comment, v commentView) gen.VarianceComment { return v.toAPI(c) })
	if err != nil {
		return nil, err
	}
	return &gen.VarianceCommentPage{Items: items, NextPageToken: next}, nil
}

// figures are a report's rows' actual and comparison by account.
type figures map[uuid.UUID][2]money.Minor

// rowKey names a close's report against a basis.
type rowKey struct {
	close uuid.UUID
	basis string
}

// commentView is what comments are shown with: their companies and
// people, their currencies' digits, and the rows' figures now of each
// close and basis the company still compares against.
type commentView struct {
	view
	exps    map[string]int
	current map[rowKey]figures
}

// commentView reads what the comments are shown with.
func (e *Endpoints) commentView(ctx context.Context, tx pgx.Tx, cs []comment) (commentView, error) {
	ids := append(idref.Of(cs, func(c comment) *uuid.UUID { return &c.AuthorID }), idref.Of(cs, func(c comment) *uuid.UUID { return c.DecidedBy })...)
	v, err := e.view(ctx, idref.Of(cs, func(c comment) *uuid.UUID { return &c.LegalEntityID }), nil, ids...)
	if err != nil {
		return commentView{}, err
	}
	out := commentView{view: v, exps: map[string]int{}, current: map[rowKey]figures{}}
	for _, c := range cs {
		if _, ok := out.exps[c.Currency]; !ok {
			if out.exps[c.Currency], err = currency.Exponent(ctx, tx, c.Currency, nil); err != nil {
				return commentView{}, fmt.Errorf("closing: currency of variance comment %s: %w", c.ID, err)
			}
		}
		k := rowKey{c.CloseID, c.Basis}
		if _, ok := out.current[k]; ok {
			continue
		}
		if out.current[k], err = e.figuresNow(ctx, tx, k); err != nil {
			return commentView{}, err
		}
	}
	return out, nil
}

// figuresNow reads the rows' figures of a close's report against a basis,
// nil when the company no longer compares against it.
func (e *Endpoints) figuresNow(ctx context.Context, tx pgx.Tx, k rowKey) (figures, error) {
	r, err := e.report(ctx, tx, k.close, k.basis)
	if err != nil || r.setting == nil {
		return nil, err
	}
	out := figures{}
	for _, row := range r.rows {
		out[row.AccountID] = [2]money.Minor{row.Net, row.comparison}
	}
	return out, nil
}

// commentOut is one comment as the API shows it.
func (e *Endpoints) commentOut(ctx context.Context, tx pgx.Tx, c comment) (gen.VarianceComment, error) {
	v, err := e.commentView(ctx, tx, []comment{c})
	if err != nil {
		return gen.VarianceComment{}, err
	}
	return v.toAPI(c), nil
}

// toAPI shows a comment with its company, its author and decider named,
// its figures with their currency and whether they are still its row's.
func (v commentView) toAPI(c comment) gen.VarianceComment {
	co, exp := v.companies[c.LegalEntityID], v.exps[c.Currency]
	out := gen.VarianceComment{ID: c.ID, CloseID: c.CloseID, LegalEntityID: c.LegalEntityID, CompanyCode: co.Code, CompanyName: co.Name,
		FiscalYear: int32(c.FiscalYear), FiscalPeriod: int32(c.FiscalPeriod), Basis: c.Basis, AccountID: c.AccountID, AccountNumber: c.AccountNumber,
		AccountName: c.AccountName, Text: c.Text, Edited: c.ProposedText != nil, ProposedText: apitypes.OptString(c.ProposedText), AuthorID: c.AuthorID,
		AuthorKind: c.AuthorKind, AuthorName: v.people[c.AuthorID].Name, SessionID: apitypes.OptUUID(c.SessionID),
		Actual: apitypes.Money(c.Actual, c.Currency, exp), Comparison: apitypes.Money(c.Comparison, c.Currency, exp),
		Citations: make([]gen.VarianceCitation, len(c.Citations)), Status: c.Status, DecidedBy: apitypes.OptUUID(c.DecidedBy),
		DecisionReason: apitypes.OptString(c.Reason), Version: c.Version, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	// The variance of figures the database holds as bigint minor units of
	// one row, which never overflow their difference (the report computed it).
	out.Variance = apitypes.Money(c.Actual-c.Comparison, c.Currency, exp)
	if now := v.current[rowKey{c.CloseID, c.Basis}]; now != nil {
		out.Stale.SetTo(now[c.AccountID] != [2]money.Minor{c.Actual, c.Comparison})
	}
	for i, ci := range c.Citations {
		day, _ := time.Parse(time.DateOnly, ci.PostingDate) // the database writes a date as YYYY-MM-DD
		out.Citations[i] = gen.VarianceCitation{JournalEntryID: ci.EntryID, FiscalYear: int32(ci.FiscalYear), DocumentNumber: ci.Number, PostingDate: day}
	}
	if c.DecidedBy != nil {
		out.DecidedByName.SetTo(v.people[*c.DecidedBy].Name)
		out.DecidedAt.SetTo(*c.DecidedAt) // set with who decided (variance_comment_decided)
	}
	return out
}
