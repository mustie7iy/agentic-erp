package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// AssetReads is what the ledger publishes to the assets context (ADR-0057
// §2, T57b): the ledgers a company keeps, an asset's centers derived and
// checked as a posting on its depreciation account would derive and check
// them (ADR-0050 §5), and the codes and names of centers. It runs in the
// request's transaction under row-level security (ADR-0003); assets reads
// no ledger table itself, and the composition root adapts it to the
// assets context's ports.
type AssetReads struct{}

// LedgerRef is a ledger by its code and name.
type LedgerRef struct {
	ID         uuid.UUID
	Code, Name string
	Leading    bool
}

// Ledgers returns the tenant's ledgers, the leading one first and then by
// code; with a legal entity, only those the company keeps (the leading
// ledger alone for a company assigned none, ADR-0045).
func (AssetReads) Ledgers(ctx context.Context, legalEntity *uuid.UUID) ([]LedgerRef, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, code, name, is_leading FROM ledger.ledger
		WHERE $1::uuid IS NULL OR ledger.company_keeps($1, id) ORDER BY NOT is_leading, code`, legalEntity)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LedgerRef, error) {
		var l LedgerRef
		return l, r.Scan(&l.ID, &l.Code, &l.Name, &l.Leading)
	})
	return out, errs.Wrapf(err, "ledger: ledgers")
}

// AssetAssignment is an asset's centers as AssetReads.Assignment derives
// them: the profit center (nil for none), and whether the asset needs a
// cost center it was not given.
type AssetAssignment struct {
	ProfitCenterID     *uuid.UUID
	CostCenterRequired bool
}

// Assignment derives and checks the centers an asset of the legal entity
// is created with on date, in fiscal year year, as a posting on its
// depreciation account (by number in the company's chart) dated then
// would (ADR-0057 §8): a cost center given must be one of the company's
// that the account may take, valid and not locked for costs on the date,
// and gives its profit center then (422 ledger.profit_center_mismatch for
// another given); without a cost center the profit center is as given,
// checked as a posting checks it, and none is derived. CostCenterRequired
// reports an account that needs a cost center while cost center
// accounting applies in the year (primary cost) and has no default in the
// company, so its depreciation could not post without one. Refusals name
// cost_center_id and profit_center_id. The centers are not locked: every
// posting checks them again.
func (AssetReads) Assignment(ctx context.Context, legalEntity uuid.UUID, date time.Time, year int16, account string,
	costCenter, profitCenter *uuid.UUID) (AssetAssignment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return AssetAssignment{}, err
	}
	// Activation sets up every account the configuration's asset accounts
	// name in the company (ADR-0057 §3), so a missing one is broken data.
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT a.id FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.number = $2`, legalEntity, account).Scan(&id); err != nil {
		return AssetAssignment{}, fmt.Errorf("ledger: account %s of %s: %w", account, legalEntity, err)
	}
	lines := []Line{{AccountID: id, CostCenterID: costCenter, ProfitCenterID: profitCenter}}
	r, err := readAssignRules(ctx, tx, legalEntity, year, lines)
	if err != nil || r.accounts == nil {
		return AssetAssignment{ProfitCenterID: profitCenter}, err
	}
	a := r.accounts[id]
	if costCenter == nil {
		required := a.element == config.CostElementPrimaryCost && r.costOn && a.defaultCC == nil
		return AssetAssignment{ProfitCenterID: profitCenter, CostCenterRequired: required},
			asAsset(checkLineProfitCenters(ctx, tx, legalEntity, date, lines, make([]lineCenters, 1), false))
	}
	if a.element == config.CostElementNone { // as deriveCostCenters refuses it, whose derivation an asset does not take
		return AssetAssignment{}, asAsset(centerRefused("ledger.cost_center_invalid", 0, "cost_center_id", "The cost center", reasonNotAllowed))
	}
	found := make([]lineCenters, 1)
	if err := checkLineCostCenters(ctx, tx, legalEntity, date, lines, r, found, false); err != nil {
		return AssetAssignment{}, asAsset(err)
	}
	if err := checkLineProfitCenters(ctx, tx, legalEntity, date, lines, found, false); err != nil {
		return AssetAssignment{}, asAsset(err)
	}
	return AssetAssignment{ProfitCenterID: lines[0].ProfitCenterID}, nil
}

// asAsset makes a refusal of the one line Assignment checks name the
// asset's own fields: lines[0].<field> becomes <field>, and a detail
// starting "Line 1: " loses that start. Other errors pass unchanged.
func asAsset(err error) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	for k := range p.Errors {
		p.Errors[k].Field = strings.TrimPrefix(p.Errors[k].Field, "lines[0].")
	}
	p.Detail = strings.TrimPrefix(p.Detail, "Line 1: ")
	return p
}

// CenterName is a cost or profit center by its code and name.
type CenterName struct{ Code, Name string }

// CenterNames returns the codes and names of the cost and profit centers
// among ids, by id; an id of neither is left out.
func (AssetReads) CenterNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CenterName, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]CenterName{}
	rows, _ := tx.Query(ctx, `SELECT id, code, name FROM ledger.cost_center WHERE id = ANY($1)
		UNION ALL SELECT id, code, name FROM ledger.profit_center WHERE id = ANY($1)`, ids)
	var id uuid.UUID
	var n CenterName
	_, err = pgx.ForEachRow(rows, []any{&id, &n.Code, &n.Name}, func() error { out[id] = n; return nil })
	return out, errs.Wrapf(err, "ledger: names of %d centers", len(ids))
}

// AssetLine is a journal line as the assets context reads a source of an
// acquisition (ADR-0057 §6, T57c): its entry's company, currencies,
// posting date, document and origin, its amounts in all three currencies
// (debit positive), its profit center, and whether its entry is the
// leading ledger's, has a copy in every ledger the company keeps, was
// reversed, or reverses another.
type AssetLine struct {
	FiscalYear                       int16
	EntryID                          uuid.UUID
	LineNo                           int32
	LegalEntityID, AccountID         uuid.UUID
	Currency, CurrencyLC, CurrencyGC string
	Amount, LC, GC                   money.Minor
	PostingDate                      time.Time
	DocumentNumber, SourceContext    string
	SourceDocumentID, ProfitCenterID *uuid.UUID
	Leading, InEveryLedger           bool
	Reversed, Reversal               bool
}

// assetLineSelect reads lines as AssetLine, l the line and e its entry.
// A line is in every ledger when each ledger its company keeps has an
// entry of its posting group (an entry posted before posting groups is
// its own group, ADR-0045).
const assetLineSelect = `SELECT l.fiscal_year, l.journal_entry_id, l.line_no, l.legal_entity_id, l.account_id, e.currency_tc, e.currency_lc, e.currency_gc,
		l.amount_tc, l.amount_lc, l.amount_gc, l.posting_date, e.document_number, e.source_context, e.source_document_id, l.profit_center_id,
		g.is_leading,
		NOT EXISTS (SELECT FROM ledger.ledger k WHERE ledger.company_keeps(l.legal_entity_id, k.id)
			AND NOT EXISTS (SELECT FROM ledger.journal_entry c WHERE c.tenant_id = e.tenant_id AND c.fiscal_year = e.fiscal_year AND c.ledger_id = k.id
				AND (c.id = e.id OR c.posting_group_id = e.posting_group_id))),
		EXISTS (SELECT FROM ledger.reversal r WHERE r.tenant_id = e.tenant_id AND r.original_entry_id = e.id), e.reversal_of_id IS NOT NULL
	FROM ledger.journal_line l
	JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
	JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id`

func scanAssetLine(r pgx.Row) (AssetLine, error) {
	var a AssetLine
	err := r.Scan(&a.FiscalYear, &a.EntryID, &a.LineNo, &a.LegalEntityID, &a.AccountID, &a.Currency, &a.CurrencyLC, &a.CurrencyGC,
		&a.Amount, &a.LC, &a.GC, &a.PostingDate, &a.DocumentNumber, &a.SourceContext, &a.SourceDocumentID, &a.ProfitCenterID,
		&a.Leading, &a.InEveryLedger, &a.Reversed, &a.Reversal)
	return a, errs.Wrapf(err, "scan asset line")
}

// Line reads line lineNo of journal entry entryID (of any fiscal year),
// found false when there is none.
func (AssetReads) Line(ctx context.Context, entryID uuid.UUID, lineNo int32) (AssetLine, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return AssetLine{}, false, err
	}
	a, err := scanAssetLine(tx.QueryRow(ctx, assetLineSelect+` WHERE l.journal_entry_id = $1 AND l.line_no = $2`, entryID, lineNo))
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetLine{}, false, nil
	}
	if err != nil {
		return AssetLine{}, false, fmt.Errorf("ledger: line %d of entry %s: %w", lineNo, entryID, err)
	}
	return a, true, nil
}

// ClearingQuery selects the leading ledger's lines on accounts of a
// company in a fiscal year, in the order of (posting date, entry, line)
// after After, at most Limit: Skip and the lines of entries ExceptContext
// posted are left out, and lines of a reversed entry and of a reversal are
// left out unless Keep names them.
type ClearingQuery struct {
	LegalEntityID uuid.UUID
	Accounts      []uuid.UUID
	ExceptContext string
	FiscalYear    int16
	After         *ClearingKey
	Limit         int
	Skip, Keep    []ItemRef
}

// ClearingKey is a line's place in ClearingLines' order.
type ClearingKey struct {
	PostingDate time.Time
	EntryID     uuid.UUID
	LineNo      int32
}

// ClearingLines reads the lines the query selects (ADR-0057 §6's
// worklist): one fiscal year's partition, by the account index.
func (AssetReads) ClearingLines(ctx context.Context, q ClearingQuery) ([]AssetLine, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var after ClearingKey
	if q.After != nil {
		after = *q.After
	}
	sy, se, sl := columns(q.Skip)
	ky, ke, kl := columns(q.Keep)
	rows, _ := tx.Query(ctx, assetLineSelect+` WHERE l.legal_entity_id = $1 AND l.account_id = ANY($2) AND l.fiscal_year = $3 AND g.is_leading AND e.source_context <> $15
			AND ($4 OR (l.posting_date, l.journal_entry_id, l.line_no) > ($5, $6, $7))
			AND (l.fiscal_year, l.journal_entry_id, l.line_no) NOT IN (SELECT * FROM unnest($9::smallint[], $10::uuid[], $11::integer[]))
			AND ((e.reversal_of_id IS NULL AND NOT EXISTS (SELECT FROM ledger.reversal r WHERE r.tenant_id = e.tenant_id AND r.original_entry_id = e.id))
				OR (l.fiscal_year, l.journal_entry_id, l.line_no) IN (SELECT * FROM unnest($12::smallint[], $13::uuid[], $14::integer[])))
		ORDER BY l.posting_date, l.journal_entry_id, l.line_no LIMIT $8`,
		q.LegalEntityID, q.Accounts, q.FiscalYear, q.After == nil, after.PostingDate, after.EntryID, after.LineNo, q.Limit, sy, se, sl, ky, ke, kl, q.ExceptContext)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AssetLine, error) { return scanAssetLine(r) })
	return out, errs.Wrapf(err, "ledger: clearing lines of %s in %d", q.LegalEntityID, q.FiscalYear)
}

// AssetEntry is one ledger's entry of a posting group.
type AssetEntry struct {
	ID, LedgerID       uuid.UUID
	FiscalYear, Period int16
	DocumentNumber     string
}

// Entries reads the entries of entry's posting group, entry's own first
// and then by ledger code: one per ledger it was posted to.
func (AssetReads) Entries(ctx context.Context, fiscalYear int16, entry uuid.UUID) ([]AssetEntry, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT e.id, e.ledger_id, e.fiscal_year, e.fiscal_period, e.document_number FROM ledger.journal_entry e
		JOIN ledger.ledger g ON g.tenant_id = e.tenant_id AND g.id = e.ledger_id
		WHERE e.fiscal_year = $1 AND (e.id = $2 OR e.posting_group_id = $2) ORDER BY e.id <> $2, g.code`, fiscalYear, entry)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AssetEntry, error) {
		var a AssetEntry
		return a, r.Scan(&a.ID, &a.LedgerID, &a.FiscalYear, &a.Period, &a.DocumentNumber)
	})
	return out, errs.Wrapf(err, "ledger: entries of the posting group of %s", entry)
}

// Accounts returns the ids of the company's accounts by number: those the
// configuration's asset accounts name (ADR-0057 §3), which activation sets
// up in the company; a number it has no account for is left out.
func (AssetReads) Accounts(ctx context.Context, legalEntity uuid.UUID, numbers []string) (map[string]uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT a.number, a.id FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.number = ANY($2)`, legalEntity, numbers)
	out := map[string]uuid.UUID{}
	var number string
	var id uuid.UUID
	_, err = pgx.ForEachRow(rows, []any{&number, &id}, func() error { out[number] = id; return nil })
	return out, errs.Wrapf(err, "ledger: accounts %v of %s", numbers, legalEntity)
}
