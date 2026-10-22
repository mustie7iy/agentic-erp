// Package cash is the cash management context (ADR-0051; 03-data-model
// §3.18): the daily cash position and the liquidity forecast, computed on
// read from the ledger, payments, payables and procurement, classified by
// source, certainty and the tenant's liquidity items. It is a leaf: it
// reads those contexts only through read-only ports they publish, run in
// the request's transaction under row-level security (ADR-0003), and no
// context imports it (internal/layout).
//
// T53b builds its configuration: the liquidity items and planning groups
// of the document, kept in the cash schema by Setup, and the derivation of
// a flow's liquidity item (Derive). The rules and each company's cash
// settings are read from the active document when a position or forecast
// is computed (T53c, T53e).
package cash

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Setup is the configuration section of cash management: it keeps the
// document's liquidity items and planning groups in cash.liquidity_item
// and cash.planning_group. One the document no longer has becomes
// inactive, never deleted, since memo records and snapshots name items
// (T53d, T53f); the partner context refuses taking away a planning group a
// supplier has (partner.PlanningGroupCodes).
type Setup struct{}

// Check has nothing to check against the tenant's data: the document's
// validation covers the items, groups and rules, and nothing the cash
// context stores names an item yet.
func (Setup) Check(context.Context, *config.Config, *config.Config) ([]config.Finding, error) {
	return nil, nil
}

// Apply makes the tenant's liquidity items and planning groups the
// document's.
func (Setup) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if err := applyItems(ctx, tx, p, doc.Cash.LiquidityItems); err != nil {
		return fmt.Errorf("cash: liquidity items: %w", err)
	}
	if err := applyGroups(ctx, tx, p, doc.Cash.PlanningGroups); err != nil {
		return fmt.Errorf("cash: planning groups: %w", err)
	}
	return nil
}

// applyItems deactivates the items not given, then inserts, updates or
// reactivates the given ones in one statement, so a child may come before
// its parent (the foreign key is checked at the statement's end).
func applyItems(ctx context.Context, tx pgx.Tx, p reqctx.Principal, items []config.LiquidityItem) error {
	codes, names, directions, parents := []string{}, []string{}, []string{}, []string{}
	for _, it := range items {
		codes, names, directions, parents = append(codes, it.Code), append(names, it.Name), append(directions, it.Direction), append(parents, it.Parent)
	}
	if _, err := tx.Exec(ctx, `UPDATE cash.liquidity_item SET is_active = false, version = version + 1, updated_by = $2
		WHERE is_active AND code <> ALL($1)`, codes, p.ID); err != nil {
		return fmt.Errorf("deactivate: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cash.liquidity_item AS i (tenant_id, code, name, direction, parent_code, created_by, updated_by)
		SELECT $1, code, name, direction, nullif(parent, ''), $2, $2 FROM unnest($3::text[], $4::text[], $5::text[], $6::text[]) AS x(code, name, direction, parent)
		ON CONFLICT (tenant_id, code) DO UPDATE SET name = EXCLUDED.name, direction = EXCLUDED.direction, parent_code = EXCLUDED.parent_code,
			is_active = true, version = i.version + 1, updated_by = $2
		WHERE (i.name, i.direction, i.parent_code, i.is_active) IS DISTINCT FROM (EXCLUDED.name, EXCLUDED.direction, EXCLUDED.parent_code, true)`,
		p.TenantID, p.ID, codes, names, directions, parents); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// applyGroups deactivates the groups not given, then inserts, renames or
// reactivates the given ones.
func applyGroups(ctx context.Context, tx pgx.Tx, p reqctx.Principal, groups []config.PlanningGroup) error {
	codes, names := []string{}, []string{}
	for _, g := range groups {
		codes, names = append(codes, g.Code), append(names, g.Name)
	}
	if _, err := tx.Exec(ctx, `UPDATE cash.planning_group SET is_active = false, version = version + 1, updated_by = $2
		WHERE is_active AND code <> ALL($1)`, codes, p.ID); err != nil {
		return fmt.Errorf("deactivate: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cash.planning_group AS g (tenant_id, code, name, created_by, updated_by)
		SELECT $1, code, name, $2, $2 FROM unnest($3::text[], $4::text[]) AS x(code, name)
		ON CONFLICT (tenant_id, code) DO UPDATE SET name = EXCLUDED.name, is_active = true, version = g.version + 1, updated_by = $2
		WHERE (g.name, g.is_active) IS DISTINCT FROM (EXCLUDED.name, true)`, p.TenantID, p.ID, codes, names); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// PlanningGroups answers the partner context's question whether a code is
// one of the tenant's planning groups (partner.PlanningGroups), in the
// request's transaction. The composition root wires it, so the partner
// context does not import this one.
type PlanningGroups struct{}

// IsPlanningGroup reports whether the code is an active planning group.
func (PlanningGroups) IsPlanningGroup(ctx context.Context, code string) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM cash.planning_group WHERE code = $1 AND is_active)`, code).Scan(&ok); err != nil {
		return false, fmt.Errorf("cash: planning group %s: %w", code, err)
	}
	return ok, nil
}
