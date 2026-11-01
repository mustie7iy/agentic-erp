package ledger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TaxItemGroup is the unreported tax items of a return's companies that
// share a direction, kind, component, jurisdiction and account key, and
// are dated in the return's period or before it (PriorPeriod): what a
// return's boxes select from (ADR-0049 §11). Amounts are the items' sums
// in local currency, signed like the lines (debit positive).
type TaxItemGroup struct {
	Direction, Kind, Component string
	Jurisdiction               *string
	AccountKey                 string
	PriorPeriod                bool
	Items                      int64
	BaseLC, TaxLC              money.Minor
	RecoverableLC              money.Minor
}

// NonRecoverableLC is the part of the group's tax not recovered: the sum
// of its items' TaxItem.NonRecoverable in local currency.
func (g TaxItemGroup) NonRecoverableLC() money.Minor {
	_, lc := TaxItem{AccountKey: g.AccountKey, TaxLC: g.TaxLC, RecoverableLC: g.RecoverableLC}.NonRecoverable()
	return lc // a sum of the items' parts, each of its tax's sign and at most it, as the database summed them
}

// TaxItemSelection chooses tax items by direction and, when listed, by
// kind, component, jurisdiction and account key: a return box's selection.
type TaxItemSelection struct {
	Direction                                     string
	Kinds, Components, Jurisdictions, AccountKeys []string
}

// Matches reports whether the selection chooses the group's items, as
// UnreportedItems chooses items.
func (s TaxItemSelection) Matches(g TaxItemGroup) bool {
	in := func(list []string, v string) bool { return len(list) == 0 || slices.Contains(list, v) }
	return g.Direction == s.Direction && in(s.Kinds, g.Kind) && in(s.Components, g.Component) && in(s.AccountKeys, g.AccountKey) &&
		(len(s.Jurisdictions) == 0 || g.Jurisdiction != nil && slices.Contains(s.Jurisdictions, *g.Jurisdiction))
}

// TaxItemKey is an item's place in the order UnreportedItems pages by.
type TaxItemKey struct {
	TaxDate    time.Time
	FiscalYear int16
	EntryID    uuid.UUID
	ItemNo     int32
}

// unreported is the condition on the items a return of legal entities $1
// for the period ending $2 reports: not yet reported, of no entry migrated
// from SAP (source_context integration), whose items SAP's own returns
// reported (ADR-0037 As built, T72c); the reversal of a migrated down
// payment's tax on clearing is posted on the platform and is reported.
const unreported = ` JOIN ledger.journal_entry e ON e.tenant_id = i.tenant_id AND e.fiscal_year = i.fiscal_year AND e.id = i.journal_entry_id
	WHERE i.legal_entity_id = ANY($1) AND i.tax_date <= $2 AND i.reported_in IS NULL AND e.source_context <> 'integration'`

// Unreported returns the groups of the tax items of the legal entities,
// each one the caller may act in, that no return has reported and whose
// tax date is on or before the period's end; an item dated before the
// period's start is a prior-period item (ADR-0049 §11, P7).
func (TaxItems) Unreported(ctx context.Context, legalEntities []uuid.UUID, start, end time.Time) ([]TaxItemGroup, error) {
	tx, err := taxItemsOf(ctx, legalEntities)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.direction, i.kind, i.component, i.jurisdiction, i.account_key, i.tax_date < $3, count(*),
		sum(i.base_lc)::bigint, sum(i.tax_lc)::bigint, sum(i.recoverable_lc)::bigint
		FROM ledger.tax_item i`+unreported+`
		GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY 1, 2, 3, 4 NULLS FIRST, 5, 6`, legalEntities, end, start)
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaxItemGroup, error) {
		var g TaxItemGroup
		err := row.Scan(&g.Direction, &g.Kind, &g.Component, &g.Jurisdiction, &g.AccountKey, &g.PriorPeriod, &g.Items, &g.BaseLC, &g.TaxLC, &g.RecoverableLC)
		return g, errs.Wrapf(err, "scan tax item group")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: unreported tax items to %s: %w", end.Format(time.DateOnly), err)
	}
	return groups, nil
}

// UnreportedItems returns, in tax date and entry order, up to limit of the
// items Unreported groups that the selection chooses, after the item at
// after when given: the items a return's box sums.
func (TaxItems) UnreportedItems(ctx context.Context, legalEntities []uuid.UUID, end time.Time, sel TaxItemSelection, after *TaxItemKey,
	limit int) ([]TaxItem, error) {
	tx, err := taxItemsOf(ctx, legalEntities)
	if err != nil {
		return nil, err
	}
	var a TaxItemKey
	if after != nil {
		a = *after
	}
	rows, _ := tx.Query(ctx, taxItemSelect+unreported+` AND i.direction = $3
		AND (coalesce(cardinality($4::text[]), 0) = 0 OR i.kind = ANY($4))
		AND (coalesce(cardinality($5::text[]), 0) = 0 OR i.component = ANY($5))
		AND (coalesce(cardinality($6::text[]), 0) = 0 OR i.jurisdiction = ANY($6))
		AND (coalesce(cardinality($7::text[]), 0) = 0 OR i.account_key = ANY($7))
		AND (NOT $8 OR (i.tax_date, i.fiscal_year, i.journal_entry_id, i.item_no) > ($9::date, $10::smallint, $11::uuid, $12::integer))
		ORDER BY i.tax_date, i.fiscal_year, i.journal_entry_id, i.item_no LIMIT $13`,
		legalEntities, end, sel.Direction, sel.Kinds, sel.Components, sel.Jurisdictions, sel.AccountKeys,
		after != nil, a.TaxDate, a.FiscalYear, a.EntryID, a.ItemNo, limit)
	items, err := pgx.CollectRows(rows, scanTaxItem)
	if err != nil {
		return nil, fmt.Errorf("ledger: unreported %s tax items to %s: %w", sel.Direction, end.Format(time.DateOnly), err)
	}
	return items, nil
}

// taxItemsOf is the request's transaction, once the caller may act in
// every one of the legal entities.
func taxItemsOf(ctx context.Context, legalEntities []uuid.UUID) (pgx.Tx, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	for _, le := range legalEntities {
		if !access.Allows(le) {
			return nil, problem.ReferenceNotFound("legal_entity_id", "legal entity", "a legal entity the caller may act in")
		}
	}
	return tx, nil
}
