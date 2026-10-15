package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ChartAccount is an account of a legal entity's chart as migrations see it.
type ChartAccount struct {
	ID                 uuid.UUID
	Type               string // asset, liability, equity, revenue, expense
	ReconciliationKind string // none, supplier, customer
}

// Chart returns the accounts set up in the legal entity, by number.
func (Accounts) Chart(ctx context.Context, legalEntity uuid.UUID) (map[string]ChartAccount, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT a.number, a.id, a.account_type, a.reconciliation_kind FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.status = 'active'`, legalEntity)
	out := map[string]ChartAccount{}
	var number string
	var a ChartAccount
	_, err = pgx.ForEachRow(rows, []any{&number, &a.ID, &a.Type, &a.ReconciliationKind}, func() error { out[number] = a; return nil })
	if err != nil {
		return out, fmt.Errorf("ledger: chart of %s: %w", legalEntity, err)
	}
	return out, nil
}

// Clear records open items as cleared by an entry already posted, without
// posting anything: for documents a migration brings over that were
// cleared in the source system, where the clearing document is one of the
// migrated entries (a payment). The items must be open items of the legal
// entity that net to zero per partner in transaction and local currency,
// the source system's figures; what each migrated document's conversion
// to group currency rounded differently stays on the reconciliation
// account.
func (p Postings) Clear(ctx context.Context, legalEntity uuid.UUID, date time.Time, by ItemRef, items []ItemRef) (uuid.UUID, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	loaded, err := loadItems(ctx, tx, items)
	if err != nil {
		return uuid.Nil, err
	}
	var currencies []string
	for _, it := range loaded {
		currencies = append(currencies, it.currency)
	}
	exps, err := exponents(ctx, tx, currencies...)
	if err != nil {
		return uuid.Nil, err
	}
	payload, err := migratedItems(loaded, legalEntity, exps)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := recordClearing(ctx, tx, actor, clearingDoc{le: legalEntity, date: date, by: by}, items, payload)
	if err != nil {
		return uuid.Nil, err
	}
	return id, audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"items": len(items), "clearing_entry_id": by.EntryID, "migrated": true}})
}

// migratedItems checks the items a migrated clearing settles (open items of
// the legal entity, not reversed, not reserved, netting to zero per partner
// in transaction and local currency) and returns their event payload.
func migratedItems(loaded []item, legalEntity uuid.UUID, exps map[string]int) ([]map[string]any, error) {
	sums := perPartner{}
	payload := make([]map[string]any, len(loaded))
	for i, it := range loaded {
		if err := checkItem(i, it, &legalEntity, uuid.Nil); err != nil {
			return nil, err
		}
		sums.add(*it.partner, it.tc, it.lc)
		payload[i] = map[string]any{"fiscal_year": it.ref.FiscalYear, "journal_entry_id": it.ref.EntryID, "line_no": it.ref.LineNo,
			"account_id": it.account, "partner_id": it.partner, "amount_tc": money.EventAmount(it.tc, it.currency, exps[it.currency])}
	}
	if partner, ok := sums.unbalanced(); ok {
		return nil, problem.Unprocessable("ledger.clearing_not_balanced", "The items of partner "+partner.String()+" do not net to zero in transaction and local currency.")
	}
	return payload, nil
}

// Reports answers reconciliation questions about the ledger.
type Reports struct{}

// Totals are a legal entity's journal totals in local currency for a
// fiscal year: the balance per account number and the sum of debits.
type Totals struct {
	ByAccount map[string]money.Minor
	Debits    money.Minor
}

// Totals sums the legal entity's lines of the fiscal year in the leading
// ledger.
func (Reports) Totals(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (Totals, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Totals{}, err
	}
	rows, _ := tx.Query(ctx, `SELECT a.number, sum(l.amount_lc), sum(greatest(l.amount_lc, 0))::bigint FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2 GROUP BY a.number`, legalEntity, fiscalYear)
	t := Totals{ByAccount: map[string]money.Minor{}}
	var number string
	var balance, debits money.Minor
	_, err = pgx.ForEachRow(rows, []any{&number, &balance, &debits}, func() error {
		t.ByAccount[number] = balance
		// Summed exactly: debits out of range fail the totals rather than wrap.
		total, err := money.Sum(t.Debits, debits)
		t.Debits = total
		return err
	})
	if err != nil {
		return t, fmt.Errorf("ledger: totals of %s %d: %w", legalEntity, fiscalYear, err)
	}
	return t, nil
}

// OpenPayables sums the legal entity's open supplier items of the fiscal
// year in local currency per partner, in the leading ledger.
func (Reports) OpenPayables(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.partner_id, sum(l.amount_lc) FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
		GROUP BY l.partner_id`, legalEntity, fiscalYear)
	out := map[uuid.UUID]money.Minor{}
	var partner uuid.UUID
	var sum money.Minor
	_, err = pgx.ForEachRow(rows, []any{&partner, &sum}, func() error { out[partner] = sum; return nil })
	if err != nil {
		return out, fmt.Errorf("ledger: open payables of %s %d: %w", legalEntity, fiscalYear, err)
	}
	return out, nil
}
