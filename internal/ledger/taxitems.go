package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TaxItem is what an entry taxed per tax code and component (ADR-0049 §5,
// T61c): its tax date and the rate applied, the base and the tax and the
// recoverable part in transaction and local currency, signed like the
// lines (debit positive). Returns read items, never journal lines.
type TaxItem struct {
	FiscalYear     int16
	EntryID        uuid.UUID
	ItemNo         int32
	LegalEntityID  uuid.UUID
	Code           string
	Component      string
	Jurisdiction   *string
	Direction      string
	Kind           string
	AccountKey     string
	AccountID      *uuid.UUID // the tax line's account
	TaxDate        time.Time
	TaxRateID      uuid.UUID
	RatePct        string
	RecoverablePct string
	Base, Tax      money.Minor
	Recoverable    money.Minor
	BaseLC, TaxLC  money.Minor
	RecoverableLC  money.Minor
	ReportedIn     *uuid.UUID
}

// NonRecoverable is the part of the tax not recovered: of an input item
// what the recoverable share leaves, of an output item none.
func (t TaxItem) NonRecoverable() (tc, lc money.Minor) {
	if t.AccountKey == "output" || t.AccountKey == "self_output" {
		return 0, 0
	}
	return t.Tax - t.Recoverable, t.TaxLC - t.RecoverableLC // of the tax's sign and at most it (tax_item_recoverable): in range
}

const taxItemSelect = `SELECT i.fiscal_year, i.journal_entry_id, i.item_no, i.legal_entity_id, i.code, i.component, i.jurisdiction, i.direction, i.kind,
		i.account_key, i.account_id, i.tax_date, i.tax_rate_id, trim_scale(i.rate_pct)::text, trim_scale(i.recoverable_pct)::text, i.base_tc, i.tax_tc,
		i.recoverable_tc, i.base_lc, i.tax_lc, i.recoverable_lc, i.reported_in
	FROM ledger.tax_item i`

func scanTaxItem(row pgx.CollectableRow) (TaxItem, error) {
	var t TaxItem
	err := row.Scan(&t.FiscalYear, &t.EntryID, &t.ItemNo, &t.LegalEntityID, &t.Code, &t.Component, &t.Jurisdiction, &t.Direction, &t.Kind,
		&t.AccountKey, &t.AccountID, &t.TaxDate, &t.TaxRateID, &t.RatePct, &t.RecoverablePct, &t.Base, &t.Tax, &t.Recoverable, &t.BaseLC, &t.TaxLC,
		&t.RecoverableLC, &t.ReportedIn)
	return t, errs.Wrapf(err, "scan tax item")
}

// TaxItems is the ledger's tax items port: what returns (T61g) and the
// contexts that post taxed documents read.
type TaxItems struct{}

// Of returns the tax items of a journal entry of a legal entity the caller
// may act in, in item order.
func (TaxItems) Of(ctx context.Context, entryID uuid.UUID) ([]TaxItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := getEntry(ctx, tx, entryID)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, taxItemSelect+` WHERE i.fiscal_year = $1 AND i.journal_entry_id = $2 ORDER BY i.item_no`, entry.FiscalYear, entry.ID)
	items, err := pgx.CollectRows(rows, scanTaxItem)
	if err != nil {
		return nil, fmt.Errorf("ledger: tax items of entry %s: %w", entryID, err)
	}
	return items, nil
}

// Dated returns the tax items of a legal entity the caller may act in
// whose tax date falls in [from, until), what a return period selects,
// by tax date, entry and item.
func (TaxItems) Dated(ctx context.Context, legalEntityID uuid.UUID, from, until time.Time) ([]TaxItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	if !access.Allows(legalEntityID) {
		return nil, problem.ReferenceNotFound("legal_entity_id", "legal entity", "a legal entity the caller may act in")
	}
	rows, _ := tx.Query(ctx, taxItemSelect+` WHERE i.legal_entity_id = $1 AND i.tax_date >= $2 AND i.tax_date < $3
		ORDER BY i.tax_date, i.fiscal_year, i.journal_entry_id, i.item_no`, legalEntityID, from, until)
	items, err := pgx.CollectRows(rows, scanTaxItem)
	if err != nil {
		return nil, fmt.Errorf("ledger: tax items of %s from %s: %w", legalEntityID, from.Format(time.DateOnly), err)
	}
	return items, nil
}

// sourcedTaxItem is an item written from an item of another entry: its
// code, component, rate and the rest as there, with amounts of its own
// (a reversal's negated, a cash discount's share). A nil recoverable part
// is the whole tax, for an input item: a share of what the source posted
// on its tax account, which is what it recovered; its tax date is the
// source's when keepSourceDate, else the entry's.
type sourcedTaxItem struct {
	year           int16
	entry          uuid.UUID
	itemNo         int32
	baseTC, taxTC  money.Minor
	baseLC, taxLC  money.Minor
	recTC, recLC   *money.Minor
	keepSourceDate bool
}

// negatedTaxItems are the items of the entries negated, a reversal's or a
// transfer's of the tax it reverses (ADR-0049 §4): keepDate keeps each
// item's tax date, a reversal's, which reverses the original's tax where
// it was due.
func negatedTaxItems(ctx context.Context, tx pgx.Tx, years []int16, entries []uuid.UUID, keepDate bool) ([]sourcedTaxItem, error) {
	rows, _ := tx.Query(ctx, taxItemSelect+` WHERE (i.fiscal_year, i.journal_entry_id) IN (SELECT * FROM unnest($1::smallint[], $2::uuid[]))
		ORDER BY i.fiscal_year, i.journal_entry_id, i.item_no`, years, entries)
	items, err := pgx.CollectRows(rows, scanTaxItem)
	if err != nil {
		return nil, fmt.Errorf("ledger: tax items of %d entries: %w", len(entries), err)
	}
	out := make([]sourcedTaxItem, len(items))
	for i, t := range items {
		// Stored amounts were posted, so their negation is in range.
		recTC, recLC := -t.Recoverable, -t.RecoverableLC
		out[i] = sourcedTaxItem{year: t.FiscalYear, entry: t.EntryID, itemNo: t.ItemNo, baseTC: -t.Base, taxTC: -t.Tax, baseLC: -t.BaseLC,
			taxLC: -t.TaxLC, recTC: &recTC, recLC: &recLC, keepSourceDate: keepDate}
	}
	return out, nil
}

// writeTaxItems writes the items of the entry just inserted (ADR-0049 §5):
// those the draft brings from other entries' items, or else those its
// lines carry, at its tax date (ledger.write_tax_items). The database
// checks at commit that its tax lines and items agree (tax_item_lines).
func writeTaxItems(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, year int16, id uuid.UUID, d draft) error {
	if d.givenItems != nil {
		return writeGivenItems(ctx, tx, tenant, year, id, d)
	}
	if d.sourcedItems == nil {
		if !taxed(d.lines) {
			return nil
		}
		if _, err := tx.Exec(ctx, `SELECT ledger.write_tax_items($1, $2, $3, $4, false)`, tenant, year, id, d.taxDay()); err != nil {
			return fmt.Errorf("ledger: post entry %s: tax items: %w", id, err)
		}
		return nil
	}
	n := len(d.sourcedItems)
	years, entries, nos := make([]int16, n), make([]uuid.UUID, n), make([]int32, n)
	baseTC, taxTC, baseLC, taxLC := make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n)
	recTC, recLC, keep := make([]*money.Minor, n), make([]*money.Minor, n), make([]bool, n)
	for i, s := range d.sourcedItems {
		years[i], entries[i], nos[i] = s.year, s.entry, s.itemNo
		baseTC[i], taxTC[i], baseLC[i], taxLC[i], recTC[i], recLC[i], keep[i] = s.baseTC, s.taxTC, s.baseLC, s.taxLC, s.recTC, s.recLC, s.keepSourceDate
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component,
			jurisdiction, direction, kind, account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc,
			base_lc, tax_lc, recoverable_lc)
		SELECT $1, $2, $3, x.n, o.legal_entity_id, o.code, o.component, o.jurisdiction, o.direction, o.kind, o.account_key, o.account_id,
			CASE WHEN x.keep THEN o.tax_date ELSE $4 END, o.tax_rate_id, o.rate_pct, o.recoverable_pct, x.btc, x.ttc,
			coalesce(x.rtc, CASE WHEN o.account_key IN ('output', 'self_output') THEN 0 ELSE x.ttc END), x.blc, x.tlc,
			coalesce(x.rlc, CASE WHEN o.account_key IN ('output', 'self_output') THEN 0 ELSE x.tlc END)
		FROM unnest($5::smallint[], $6::uuid[], $7::int[], $8::bigint[], $9::bigint[], $10::bigint[], $11::bigint[], $12::bigint[], $13::bigint[],
			$14::boolean[]) WITH ORDINALITY AS x(y, e, i, btc, ttc, blc, tlc, rtc, rlc, keep, n)
		JOIN ledger.tax_item o ON o.fiscal_year = x.y AND o.journal_entry_id = x.e AND o.item_no = x.i`,
		tenant, year, id, d.taxDay(), years, entries, nos, baseTC, taxTC, baseLC, taxLC, recTC, recLC, keep); err != nil {
		return fmt.Errorf("ledger: post entry %s: tax items: %w", id, err)
	}
	return nil
}

// taxed reports whether a line carries a tax code.
func taxed(lines []Line) bool {
	for _, l := range lines {
		if l.TaxCode != nil {
			return true
		}
	}
	return false
}

// taxDay is the draft's tax date: the one it names, else its posting date
// (ADR-0049 §4).
func (d draft) taxDay() time.Time {
	if d.taxDate.IsZero() {
		return d.postingDate
	}
	return d.taxDate
}

// writeGivenItems writes the items a context calculated (T61d), each on
// its component's account with the rate in force on the tax date, numbered
// in order; the commit-time check holds them to the tax lines.
func writeGivenItems(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, year int16, id uuid.UUID, d draft) error {
	n := len(d.givenItems)
	codes, components := make([]string, n), make([]string, n)
	base, tax, rec, baseLC, taxLC, recLC := make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n),
		make([]money.Minor, n), make([]money.Minor, n)
	for i, t := range d.givenItems {
		codes[i], components[i] = t.Code, t.Component
		base[i], tax[i], rec[i], baseLC[i], taxLC[i], recLC[i] = t.Base, t.Tax, t.Recoverable, t.BaseLC, t.TaxLC, t.RecoverableLC
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component,
			jurisdiction, direction, kind, account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc,
			base_lc, tax_lc, recoverable_lc)
		SELECT $1, $2, $3, x.n, c.legal_entity_id, c.code, c.component, c.jurisdiction, d.direction, d.kind, c.account_key, c.account_id, $5, r.id,
			r.rate_pct, c.recoverable_pct, x.b, x.t, x.r, x.bl, x.tl, x.rl
		FROM unnest($6::text[], $7::text[], $8::bigint[], $9::bigint[], $10::bigint[], $11::bigint[], $12::bigint[], $13::bigint[])
			WITH ORDINALITY AS x(code, component, b, t, r, bl, tl, rl, n)
		JOIN ledger.tax_component c ON c.legal_entity_id = $4 AND c.code = x.code AND c.component = x.component
		JOIN ledger.tax_code_definition d ON d.tenant_id = c.tenant_id AND d.legal_entity_id = c.legal_entity_id AND d.code = c.code
		JOIN ledger.tax_rate r ON r.tenant_id = c.tenant_id AND r.legal_entity_id = c.legal_entity_id AND r.code = c.code AND r.component = c.component
			AND r.withdrawn_at IS NULL AND r.valid_from <= $5 AND (r.valid_to IS NULL OR r.valid_to > $5)`,
		tenant, year, id, d.entity.ID, d.taxDay(), codes, components, base, tax, rec, baseLC, taxLC, recLC); err != nil {
		return fmt.Errorf("ledger: post entry %s: tax items: %w", id, err)
	}
	return nil
}

// keepLocal sets the local amounts the posting keeps (Line.keptLC) on the
// converted lines, each with its group amount converted from it at the
// posting date's rate, and books what that leaves unbalanced in local and
// group currency as an exchange difference (fxLines): a tax line converted
// at the tax date's rate (ADR-0049 §8). Lines without kept amounts are
// returned as they are.
func keepLocal(ctx context.Context, le uuid.UUID, lines []Line, rateGC exchange, expLC, expGC int, text *string) ([]Line, error) {
	kept := false
	lcs, gcs := make([]money.Minor, len(lines)), make([]money.Minor, len(lines))
	for i := range lines {
		if k := lines[i].keptLC; k != nil {
			g, err := convert(*k, rateGC, expLC, expGC)
			if err != nil {
				return nil, err
			}
			lines[i].LC, lines[i].GC, kept = *k, g, true
		}
		lcs[i], gcs[i] = lines[i].LC, lines[i].GC
	}
	if !kept {
		return lines, nil
	}
	lc, err := money.Sum(lcs...)
	if err != nil {
		return nil, errTotalTooLarge("lines")
	}
	gc, err := money.Sum(gcs...)
	if err != nil {
		return nil, errTotalTooLarge("lines")
	}
	fx, err := fxLines(ctx, le, -lc, -gc, text)
	return append(lines, fx...), err
}
