package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

var ledgerCodeSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

// ListLedgers implements GET /v1/ledgers: the ledgers with the legal
// entities the caller may see that keep each (ADR-0045).
func (e *Endpoints) ListLedgers(ctx context.Context, params gen.ListLedgersParams) (gen.ListLedgersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	entity := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	ks, err := e.Pages.Keyset(ledgerCodeSort, fmt.Sprintf("listLedgers|%s", entity), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (SELECT l.id, l.code, l.name, l.accounting_principle, l.is_leading,
			coalesce((SELECT array_agg(c.legal_entity_id ORDER BY c.legal_entity_id) FROM ledger.company_ledger c
				WHERE c.tenant_id = l.tenant_id AND c.ledger_id = l.id AND ($1 OR c.legal_entity_id = ANY($2))), '{}') AS legal_entity_ids,
			l.version, l.created_at, l.created_by
		FROM ledger.ledger l WHERE $3::uuid IS NULL OR EXISTS (SELECT FROM ledger.company_ledger c WHERE c.tenant_id = l.tenant_id
			AND c.ledger_id = l.id AND c.legal_entity_id = $3 AND ($1 OR $3 = ANY($2)))) l WHERE true`,
		[]any{access.All, access.IDs, entity})
	rows, _ := tx.Query(ctx, q, args...)
	ledgers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[gen.Ledger])
	if err != nil {
		return nil, fmt.Errorf("ledger: list ledgers: %w", err)
	}
	ledgers, next := pagination.Page(ks, ledgers, func(l gen.Ledger) (string, uuid.UUID) { return l.Code, l.ID })
	page := gen.LedgerPage{Items: ledgers}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &page, nil
}

// ListLedgerGroups implements GET /v1/ledger-groups (ADR-0045).
func (e *Endpoints) ListLedgerGroups(ctx context.Context, params gen.ListLedgerGroupsParams) (gen.ListLedgerGroupsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ledger := apitypes.Ptr[uuid.UUID](params.LedgerID)
	ks, err := e.Pages.Keyset(ledgerCodeSort, fmt.Sprintf("listLedgerGroups|%s", ledger), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT * FROM (SELECT g.id, g.code, g.name, g.representative_ledger_id,
			(SELECT array_agg(m.ledger_id ORDER BY m.ledger_id) FROM ledger.ledger_group_member m WHERE m.tenant_id = g.tenant_id AND m.group_id = g.id) AS ledger_ids,
			g.version, g.created_at, g.created_by, g.updated_at, g.updated_by
		FROM ledger.ledger_group g) g WHERE ($1::uuid IS NULL OR $1 = ANY(ledger_ids))`, []any{ledger})
	rows, _ := tx.Query(ctx, q, args...)
	groups, err := pgx.CollectRows(rows, pgx.RowToStructByPos[gen.LedgerGroup])
	if err != nil {
		return nil, fmt.Errorf("ledger: list ledger groups: %w", err)
	}
	groups, next := pagination.Page(ks, groups, func(g gen.LedgerGroup) (string, uuid.UUID) { return g.Code, g.ID })
	page := gen.LedgerGroupPage{Items: groups}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &page, nil
}
