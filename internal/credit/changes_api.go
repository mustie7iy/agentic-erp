package credit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// changeShown is what changes are shown with: their limits, and what the
// limits are shown with (their companies and customers).
type changeShown struct {
	limits map[uuid.UUID]Limit
	shown
}

// changesShown reads the limits of the changes, each once, in one query,
// and what they are shown with.
func (e *Endpoints) changesShown(ctx context.Context, tx pgx.Tx, cs []Change) (changeShown, error) {
	ids := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		ids[i] = c.LimitID
	}
	rows, _ := tx.Query(ctx, limitSelect+` WHERE l.id = ANY($1) ORDER BY l.id`, ids)
	limits, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Limit, error) { return scanLimit(r) })
	if err != nil {
		return changeShown{}, fmt.Errorf("credit: limits of %d credit limit changes: %w", len(cs), err)
	}
	out := changeShown{limits: make(map[uuid.UUID]Limit, len(limits))}
	for _, l := range limits {
		out.limits[l.ID] = l
	}
	if out.shown, err = e.shown(ctx, limits); err != nil {
		return changeShown{}, err
	}
	return out, nil
}

// changeToAPI is a change as the API gives it, with its limit's company
// and customer, and its amounts in the limit's currency.
func changeToAPI(c Change, s changeShown) gen.CreditLimitChange {
	l := s.limits[c.LimitID]
	le := s.companies[l.LegalEntityID]
	out := gen.CreditLimitChange{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), CreditLimitID: l.ID, LegalEntityID: l.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name,
		CustomerID: l.CustomerID, CustomerNumber: l.CustomerNumber, CustomerName: s.customers[l.CustomerID].Name, Kind: c.Kind, Status: c.Status,
		Limit: apitypes.Money(c.Asked.Amount, l.Currency, l.Exponent), ValidFrom: apitypes.OptDate(c.Asked.ValidFrom),
		ValidTo: apitypes.OptDate(c.Asked.ValidTo), NextReviewOn: apitypes.OptDate(c.Asked.NextReview),
		WorkflowRequestID: apitypes.OptUUID(c.WorkflowRequestID), RequestedBy: c.RequestedBy, DecidedAt: apitypes.OptDateTime(c.DecidedAt)}
	if b := c.Before; b != nil {
		out.LimitBefore.SetTo(apitypes.Money(b.Amount, l.Currency, l.Exponent))
		out.ValidFromBefore, out.ValidToBefore, out.NextReviewOnBefore = apitypes.OptDate(b.ValidFrom), apitypes.OptDate(b.ValidTo), apitypes.OptDate(b.NextReview)
	}
	return out
}

// GetCreditLimitChange implements GET /v1/credit-limit-changes/{id}: one
// of a limit of a company the caller may act in; another's is not found.
func (e *Endpoints) GetCreditLimitChange(ctx context.Context, params gen.GetCreditLimitChangeParams) (gen.GetCreditLimitChangeRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getChange(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	l, err := limit(ctx, tx, c.LimitID, false)
	if err != nil {
		return nil, err
	}
	s, err := e.shown(ctx, []Limit{l})
	if err != nil {
		return nil, err
	}
	out := changeToAPI(c, changeShown{limits: map[uuid.UUID]Limit{l.ID: l}, shown: s})
	return &gen.CreditLimitChangeHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

var changeSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{{Name: "created_at", Column: "c.created_at", Type: "timestamptz"}}}

// ListCreditLimitChanges implements GET /v1/credit-limit-changes: the
// changes of the limits of the companies the caller may act in, newest
// first, filtered by limit and status.
func (e *Endpoints) ListCreditLimitChanges(ctx context.Context, params gen.ListCreditLimitChangesParams) (gen.ListCreditLimitChangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	of := apitypes.Ptr[uuid.UUID](params.CreditLimitID)
	scope := fmt.Sprintf("listCreditLimitChanges|%s|%s", of, params.Status.Value)
	ks, err := e.Pages.Keyset(changeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("credit: page of credit limit changes: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("credit: company access for credit limit changes: %w", err)
	}
	q, args := ks.SQL(`SELECT `+changeColumns+` FROM credit.credit_limit_change c
		WHERE EXISTS (SELECT FROM credit.credit_limit l WHERE l.tenant_id = c.tenant_id AND l.id = c.credit_limit_id
			AND ($1 OR l.legal_entity_id = ANY($2))) AND ($3::uuid IS NULL OR c.credit_limit_id = $3) AND ($4 = '' OR c.status = $4)`,
		[]any{access.All, access.IDs, of, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Change, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("credit: list credit limit changes: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, cs, func(c Change) (string, uuid.UUID) { return pagination.Instant(c.CreatedAt), c.ID },
		func(page []Change) (changeShown, error) { return e.changesShown(ctx, tx, page) }, changeToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CreditLimitChangePage{Items: items, NextPageToken: next}, nil
}
