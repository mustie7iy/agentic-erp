package ledger

// Order items that are assets (ADR-0057 §6; accounting specialist B10,
// product owner 2026-11-10). An order item on an asset clearing account of
// its company is an asset's: its receipt debits that account at the order
// price, and what its invoices, credit memos and GR/IR clearings differ
// from the order price, the input tax not recovered included, posts to
// the same account rather than to the price difference account, since the
// cost of an asset is what was paid for it (IAS 16.16). From there the
// assets context capitalises it as it does the receipt's line.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// AssetClearingAccounts are those of the accounts that are asset clearing
// accounts of the legal entity's company, by id, with their numbers and
// names: what a GR/IR clearing is proposed with (procurement), so that it
// shows the account each of its items will clear against, which the
// posting judges again (GRIRItem.Clears). A read for a proposal on an
// order the caller was already given: it refuses nothing the proposal
// did not refuse before, an inactive company included, whose posting is
// refused when it is approved.
func (p Postings) AssetClearingAccounts(ctx context.Context, legalEntity uuid.UUID, accounts []uuid.UUID) (map[uuid.UUID]AccountRef, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, ok, err := p.LegalEntities.Entity(ctx, legalEntity)
	if err == nil && !ok {
		err = fmt.Errorf("ledger: asset clearing accounts: no legal entity %s", legalEntity)
	}
	if err != nil {
		return nil, err
	}
	return assetClearingAccounts(ctx, tx, p.Settings, le, accounts)
}

// itemAccounts are the accounts the invoiced and clearing items name
// (GRIRItem.ItemAccountID), each once, in order.
func itemAccounts(items []GRIRItem) []uuid.UUID {
	var out []uuid.UUID
	for _, it := range items {
		if it.ItemAccountID != nil && (it.Invoiced != nil || it.Clears != nil) {
			out = append(out, *it.ItemAccountID)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// assetClearingAccounts are those of the accounts that are asset clearing
// accounts of the legal entity's company in the active configuration
// (companies[].asset_accounts[].clearing), set up in the company. A
// company the configuration does not have, or one without asset accounts,
// has none. Without accounts to judge the configuration is not read.
func assetClearingAccounts(ctx context.Context, tx pgx.Tx, settings Settings, le tenancy.Entity, accounts []uuid.UUID) (map[uuid.UUID]AccountRef, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	if settings == nil {
		return nil, errors.New("ledger: no configuration to read the asset clearing accounts from")
	}
	doc, _, err := settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: asset clearing accounts of %s: %w", le.Code, err)
	}
	co := doc.Company(le.Code)
	if co == nil || len(co.AssetAccounts) == 0 {
		return nil, nil
	}
	rows, _ := tx.Query(ctx, `SELECT a.id, a.number, a.name FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.number = ANY($2) AND a.id = ANY($3)`, le.ID, co.AssetClearingAccounts(), accounts)
	out := map[uuid.UUID]AccountRef{}
	var a AccountRef
	_, err = pgx.ForEachRow(rows, []any{&a.ID, &a.Number, &a.Name}, func() error { out[a.ID] = a; return nil })
	return out, errs.Wrapf(err, "ledger: asset clearing accounts of %s", le.Code)
}
