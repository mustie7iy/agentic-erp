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
// is computed (T53c, T53e). T53d adds memo records (ADR-0051 §7): expected
// receipts and payments entered, uploaded by batch or proposed by an agent
// for a person to accept, one-off or a series expanded at read time, which
// count in the position on their house bank until they expire.
package cash

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Setup is the configuration section of cash management: it keeps the
// document's liquidity items and planning groups in cash.liquidity_item
// and cash.planning_group. One the document no longer has becomes
// inactive, never deleted, since memo records and snapshots name items
// (T53d, T53f); an item an open memo record names is not taken away, and
// the partner context refuses taking away a planning group a supplier has
// (partner.PlanningGroupCodes).
type Setup struct{}

// Check reports each liquidity item the proposed document no longer has
// that an open memo record (proposed or active, expired or not) names.
func (Setup) Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	if base == nil {
		return nil, nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	used, err := itemsInUse(ctx, tx, proposed.Cash.LiquidityItems)
	if err != nil {
		return nil, err
	}
	out := make([]config.Finding, len(used))
	for i, code := range used {
		out[i] = config.Finding{Severity: config.SevError, Path: "cash.liquidity_items[" + code + "]",
			Message: "open memo records name this liquidity item; move them to another item, or cancel them, first"}
	}
	return out, nil
}

// Apply makes the tenant's liquidity items and planning groups the
// document's, refusing a document that takes away an item an open memo
// record names (409 cash.liquidity_item_in_use): the activation rolls
// back. The items are deactivated first, so a memo record written at the
// same time waits for the activation (cash.check_memo_item locks the
// item) and then finds it inactive.
func (Setup) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if err := applyItems(ctx, tx, p, doc.Cash.LiquidityItems); err != nil {
		return fmt.Errorf("cash: liquidity items: %w", err)
	}
	used, err := itemsInUse(ctx, tx, doc.Cash.LiquidityItems)
	if err != nil {
		return err
	}
	if len(used) > 0 {
		return problem.Conflict("cash.liquidity_item_in_use", "Open memo records name liquidity item "+used[0]+
			", which the configuration no longer has; move them to another item, or cancel them, first.")
	}
	if err := applyGroups(ctx, tx, p, doc.Cash.PlanningGroups); err != nil {
		return fmt.Errorf("cash: planning groups: %w", err)
	}
	return nil
}

// itemsInUse are the codes open memo records name that items does not
// have, in order.
func itemsInUse(ctx context.Context, tx pgx.Tx, items []config.LiquidityItem) ([]string, error) {
	codes := []string{}
	for _, it := range items {
		codes = append(codes, it.Code)
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT liquidity_item FROM cash.memo_record WHERE status IN ('proposed', 'active') AND liquidity_item <> ALL($1)
		ORDER BY liquidity_item`, codes)
	used, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("cash: liquidity items in use: %w", err)
	}
	return used, nil
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
