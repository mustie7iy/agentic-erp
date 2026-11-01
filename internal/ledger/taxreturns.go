package ledger

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
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
// are dated in the return's period or, a group per tax date, before it
// (PriorPeriod, PriorTaxDate): what a return's boxes select from (ADR-0049
// §11). Amounts are the items' sums in local currency, signed like the
// lines (debit positive).
type TaxItemGroup struct {
	Direction, Kind, Component string
	Jurisdiction               *string
	AccountKey                 string
	PriorPeriod                bool
	PriorTaxDate               *time.Time // the items' tax date, of a prior-period group
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
	rows, _ := tx.Query(ctx, `SELECT i.direction, i.kind, i.component, i.jurisdiction, i.account_key, CASE WHEN i.tax_date < $3 THEN i.tax_date END,
		count(*), sum(i.base_lc)::bigint, sum(i.tax_lc)::bigint, sum(i.recoverable_lc)::bigint
		FROM ledger.tax_item i`+unreported+`
		GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY 1, 2, 3, 4 NULLS FIRST, 5, 6 NULLS FIRST`, legalEntities, end, start)
	groups, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TaxItemGroup, error) {
		var g TaxItemGroup
		err := row.Scan(&g.Direction, &g.Kind, &g.Component, &g.Jurisdiction, &g.AccountKey, &g.PriorTaxDate, &g.Items, &g.BaseLC, &g.TaxLC, &g.RecoverableLC)
		g.PriorPeriod = g.PriorTaxDate != nil
		return g, errs.Wrapf(err, "scan tax item group")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: unreported tax items to %s: %w", end.Format(time.DateOnly), err)
	}
	return groups, nil
}

// ReturnItem is a tax item a return's box sums, with the entry that wrote
// it as its reader names it: the document number, posting date, reference
// and header text (the billing system's invoice, say).
type ReturnItem struct {
	TaxItem
	DocumentNumber    string
	PostingDate       time.Time
	Reference, Header *string
}

// returnItemSelect reads an item and its entry's names.
var returnItemSelect = strings.Replace(taxItemSelect, "FROM ledger.tax_item i",
	", e.document_number, e.posting_date, e.reference, e.header_text FROM ledger.tax_item i", 1)

// UnreportedItems returns, in tax date and entry order, up to limit of the
// items Unreported groups that the selection chooses, after the item at
// after when given: the items a return's box sums.
func (TaxItems) UnreportedItems(ctx context.Context, legalEntities []uuid.UUID, end time.Time, sel TaxItemSelection, after *TaxItemKey,
	limit int) ([]ReturnItem, error) {
	tx, err := taxItemsOf(ctx, legalEntities)
	if err != nil {
		return nil, err
	}
	var a TaxItemKey
	if after != nil {
		a = *after
	}
	rows, _ := tx.Query(ctx, returnItemSelect+unreported+` AND i.direction = $3
		AND (coalesce(cardinality($4::text[]), 0) = 0 OR i.kind = ANY($4))
		AND (coalesce(cardinality($5::text[]), 0) = 0 OR i.component = ANY($5))
		AND (coalesce(cardinality($6::text[]), 0) = 0 OR i.jurisdiction = ANY($6))
		AND (coalesce(cardinality($7::text[]), 0) = 0 OR i.account_key = ANY($7))
		AND (NOT $8 OR (i.tax_date, i.fiscal_year, i.journal_entry_id, i.item_no) > ($9::date, $10::smallint, $11::uuid, $12::integer))
		ORDER BY i.tax_date, i.fiscal_year, i.journal_entry_id, i.item_no LIMIT $13`,
		legalEntities, end, sel.Direction, sel.Kinds, sel.Components, sel.Jurisdictions, sel.AccountKeys,
		after != nil, a.TaxDate, a.FiscalYear, a.EntryID, a.ItemNo, limit)
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReturnItem, error) {
		var r ReturnItem
		err := row.Scan(append(r.dest(), &r.DocumentNumber, &r.PostingDate, &r.Reference, &r.Header)...)
		return r, errs.Wrapf(err, "scan tax item")
	})
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

// ClaimedItem is an item a return claimed: its key and its amounts in
// local currency, which the return's hash covers.
type ClaimedItem struct {
	FiscalYear                   int16
	EntryID                      uuid.UUID
	ItemNo                       int32
	BaseLC, TaxLC, RecoverableLC money.Minor
}

// claimed is an item as Claim's update returns it: its key and amounts,
// and what Unreported groups it by.
type claimed struct {
	ClaimedItem
	group TaxItemGroup
}

// Claim reports in the return ret the items Unreported reads of the legal
// entities, each one the caller may act in, dated on or before end (and
// on or after from, when given) that one of the selections chooses: it
// sets their reported_in, an update that waits for and then skips an item
// another return claimed first, so an item is reported once (ADR-0049
// §11). It returns the items claimed, in key order, and their groups as
// Unreported groups them against the period's start.
func (TaxItems) Claim(ctx context.Context, legalEntities []uuid.UUID, from *time.Time, start, end time.Time, sels []TaxItemSelection,
	ret uuid.UUID) ([]TaxItemGroup, []ClaimedItem, error) {
	tx, err := taxItemsOf(ctx, legalEntities)
	if err != nil {
		return nil, nil, err
	}
	var all []claimed
	for _, sel := range sels {
		rows, _ := tx.Query(ctx, `UPDATE ledger.tax_item i SET reported_in = $8 FROM ledger.journal_entry e
			WHERE e.tenant_id = i.tenant_id AND e.fiscal_year = i.fiscal_year AND e.id = i.journal_entry_id
				AND i.legal_entity_id = ANY($1) AND i.tax_date <= $2 AND i.reported_in IS NULL AND e.source_context <> 'integration'
				AND ($9::date IS NULL OR i.tax_date >= $9) AND i.direction = $3
				AND (coalesce(cardinality($4::text[]), 0) = 0 OR i.kind = ANY($4))
				AND (coalesce(cardinality($5::text[]), 0) = 0 OR i.component = ANY($5))
				AND (coalesce(cardinality($6::text[]), 0) = 0 OR i.jurisdiction = ANY($6))
				AND (coalesce(cardinality($7::text[]), 0) = 0 OR i.account_key = ANY($7))
			RETURNING i.fiscal_year, i.journal_entry_id, i.item_no, i.direction, i.kind, i.component, i.jurisdiction, i.account_key,
				CASE WHEN i.tax_date < $10 THEN i.tax_date END, i.base_lc, i.tax_lc, i.recoverable_lc`,
			legalEntities, end, sel.Direction, sel.Kinds, sel.Components, sel.Jurisdictions, sel.AccountKeys, ret, from, start)
		items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claimed, error) {
			var c claimed
			g := &c.group
			err := row.Scan(&c.FiscalYear, &c.EntryID, &c.ItemNo, &g.Direction, &g.Kind, &g.Component, &g.Jurisdiction, &g.AccountKey,
				&g.PriorTaxDate, &c.BaseLC, &c.TaxLC, &c.RecoverableLC)
			return c, errs.Wrapf(err, "scan claimed tax item")
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ledger: claim %s tax items to %s for return %s: %w", sel.Direction, end.Format(time.DateOnly), ret, err)
		}
		all = append(all, items...)
	}
	return groupClaimed(all)
}

// groupClaimed sorts the claimed items by key and sums them into the
// groups Unreported would read them as.
func groupClaimed(all []claimed) ([]TaxItemGroup, []ClaimedItem, error) {
	slices.SortFunc(all, func(a, b claimed) int {
		if c := cmp.Compare(a.FiscalYear, b.FiscalYear); c != 0 {
			return c
		}
		if c := bytes.Compare(a.EntryID[:], b.EntryID[:]); c != 0 {
			return c
		}
		return cmp.Compare(a.ItemNo, b.ItemNo)
	})
	type key struct {
		direction, kind, component, jurisdiction, accountKey string
		hasJurisdiction                                      bool
		prior                                                time.Time
	}
	var groups []TaxItemGroup
	at := map[key]int{}
	items := make([]ClaimedItem, len(all))
	for n, c := range all {
		items[n] = c.ClaimedItem
		g := c.group
		k := key{direction: g.Direction, kind: g.Kind, component: g.Component, accountKey: g.AccountKey, hasJurisdiction: g.Jurisdiction != nil}
		if g.Jurisdiction != nil {
			k.jurisdiction = *g.Jurisdiction
		}
		if g.PriorTaxDate != nil {
			k.prior = *g.PriorTaxDate
		}
		i, ok := at[k]
		if !ok {
			g.PriorPeriod = g.PriorTaxDate != nil
			at[k], groups = len(groups), append(groups, g)
			i = len(groups) - 1
		}
		sum := &groups[i]
		sum.Items++
		var err error
		if sum.BaseLC, err = money.Sum(sum.BaseLC, c.BaseLC); err == nil {
			if sum.TaxLC, err = money.Sum(sum.TaxLC, c.TaxLC); err == nil {
				sum.RecoverableLC, err = money.Sum(sum.RecoverableLC, c.RecoverableLC)
			}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("ledger: claimed tax items: %w", err)
		}
	}
	return groups, items, nil
}

// Release returns the items reported in the return ret to unreported, as
// its withdrawal or rejection does, and counts them. The owning context
// decided who may end the return: its approver need not act in its
// companies.
func (TaxItems) Release(ctx context.Context, ret uuid.UUID) (int64, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE ledger.tax_item SET reported_in = NULL WHERE reported_in = $1`, ret)
	if err != nil {
		return 0, fmt.Errorf("ledger: release the tax items of return %s: %w", ret, err)
	}
	return tag.RowsAffected(), nil
}
