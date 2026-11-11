package ledger

import (
	"context"
	"errors"
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
	ReconciliationKind string // none, supplier, customer, asset (ADR-0057 §3: a migration loads its balances)
	DownPayment        bool   // the legal entity's down_payment account, which takes special G/L A only (ADR-0037)
	GRIR               bool   // the legal entity's gr_ir account, whose lines name their order item (T33, T72f)
}

// Chart returns the accounts set up in the legal entity, by number.
func (Accounts) Chart(ctx context.Context, legalEntity uuid.UUID) (map[string]ChartAccount, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT a.number, a.id, a.account_type, a.reconciliation_kind,
			EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $1
				AND d.key = 'down_payment' AND d.account_id = a.id),
			EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $1
				AND d.key = 'gr_ir' AND d.account_id = a.id)
		FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.status = 'active'`, legalEntity)
	out := map[string]ChartAccount{}
	var number string
	var a ChartAccount
	_, err = pgx.ForEachRow(rows, []any{&number, &a.ID, &a.Type, &a.ReconciliationKind, &a.DownPayment, &a.GRIR}, func() error { out[number] = a; return nil })
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
	if err := clearingPeriod(ctx, tx, legalEntity, by, date); err != nil {
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
	id, err := recordClearing(ctx, tx, actor, clearingDoc{le: legalEntity, date: date, by: by}, items, make([]int, len(items)), payload)
	if err != nil {
		return uuid.Nil, err
	}
	return id, audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"items": len(items), "clearing_entry_id": by.EntryID, "migrated": true}})
}

// EntryYears returns the fiscal year of each of the entries that exists,
// by id: a migration keeps only the id of an entry an earlier run posted
// (its external reference), and names the entry's lines to Clear by year
// and id.
func (Postings) EntryYears(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int16, error) {
	return entryValues[int16](ctx, `SELECT id, fiscal_year FROM ledger.journal_entry WHERE id = ANY($1)`, "fiscal years", ids)
}

// entryValues reads one value of each of the entries that exists, by id,
// with a query of their ids and values.
func entryValues[T any](ctx context.Context, query, what string, ids []uuid.UUID) (map[uuid.UUID]T, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, query, ids)
	out := map[uuid.UUID]T{}
	var id uuid.UUID
	var v T
	if _, err := pgx.ForEachRow(rows, []any{&id, &v}, func() error { out[id] = v; return nil }); err != nil {
		return out, fmt.Errorf("ledger: %s of %d entries: %w", what, len(ids), err)
	}
	return out, nil
}

// EntryCurrencies returns the transaction currency of each of the entries
// that exists, by id: a migration run posts the documents a source system
// cleared together in one currency, so a later run reads the currency an
// earlier run posted a document in (ADR-0005 As built).
func (Postings) EntryCurrencies(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return entryValues[string](ctx, `SELECT id, currency_tc FROM ledger.journal_entry WHERE id = ANY($1)`, "currencies", ids)
}

// ClearedTogether reports whether one clearing cleared every one of the
// items, so a migration run that finds a source system's clearing done
// already (an earlier run of the same extract cleared it) records nothing
// again. Items open, or cleared by different clearings, report false;
// Clear then refuses what is not open.
func (Postings) ClearedTogether(ctx context.Context, items []ItemRef) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	years, entries, nos := columns(items)
	var together bool
	err = tx.QueryRow(ctx, `SELECT count(DISTINCT ci.clearing_id) = 1 AND count(ci.clearing_id) = count(*)
		FROM unnest($1::smallint[], $2::uuid[], $3::int[]) AS x(y, je, n)
		LEFT JOIN ledger.clearing_item ci ON ci.fiscal_year = x.y AND ci.journal_entry_id = x.je AND ci.line_no = x.n`,
		years, entries, nos).Scan(&together)
	if err != nil {
		return false, fmt.Errorf("ledger: clearing of %d items: %w", len(items), err)
	}
	return together, nil
}

// clearingPeriod requires the clearing entry to be one of the legal
// entity's, the first entry of its posting group (the one a clearing is
// shown by; its copies are cleared with it, clearCopies) and in the
// leading ledger, where the open items are (ADR-0045), and the clearing
// date to fall in an open period of the legal entity in the entry's ledger
// and in each ledger of its copies, and holds each period's status row FOR
// SHARE until the transaction ends, as postingPeriod does for a posting: a
// close or lock waits for the clearing. A migrated clearing posts no entry,
// so nothing else checks its entry or date; ledger.clearing's trigger
// (000049) is the backstop. The locks come after the entry buckets
// loadItems takes, the order postingPeriod keeps.
func clearingPeriod(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, by ItemRef, date time.Time) error {
	var entity uuid.UUID
	var group *uuid.UUID
	var leading bool
	err := tx.QueryRow(ctx, `SELECT e.legal_entity_id, e.posting_group_id, g.is_leading FROM ledger.journal_entry e
		JOIN ledger.ledger g ON g.tenant_id = e.tenant_id AND g.id = e.ledger_id WHERE e.fiscal_year = $1 AND e.id = $2`,
		by.FiscalYear, by.EntryID).Scan(&entity, &group, &leading)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return problem.Unprocessable("ledger.invalid_clearing_entry", fmt.Sprintf("Entry %d/%s does not exist.", by.FiscalYear, by.EntryID))
	case err != nil:
		return fmt.Errorf("ledger: clearing entry %s: %w", by.EntryID, err)
	case entity != legalEntity:
		return problem.Unprocessable("ledger.invalid_clearing_entry", "Entry "+by.EntryID.String()+" belongs to another legal entity.")
	case group != nil && *group != by.EntryID:
		return problem.Unprocessable("ledger.invalid_clearing_entry", "Entry "+by.EntryID.String()+" is a copy in another ledger; clear by entry "+
			group.String()+", which clears its copies too.")
	case !leading:
		return problem.Unprocessable("ledger.invalid_clearing_entry", "Entry "+by.EntryID.String()+" is not in the leading ledger, "+
			"whose entries clear open items; clear by an entry posted to the leading ledger.")
	}
	return clearingPeriods(ctx, tx, entity, by, date)
}

// clearingPeriods checks the clearing date's period open in the ledger of
// the clearing entry by and of each of its copies, the entry's (the
// leading ledger's) first, then by ledger code.
func clearingPeriods(ctx context.Context, tx pgx.Tx, entity uuid.UUID, by ItemRef, date time.Time) error {
	day := date.Format(time.DateOnly)
	// FOR SHARE in a lateral subquery: it cannot lock the nullable side of
	// an outer join. The lateral runs once per ledger in the order of the
	// entries' subquery, which its ORDER BY keeps from being pulled up:
	// the leading ledger first (the clearing entry's, clearingPeriod),
	// then by code, as postingPeriod and a period close's sign-off lock
	// them (ADR-0058 §5), not in the order a scan finds the entries.
	rows, _ := tx.Query(ctx, `SELECT c.code, c.is_leading, s.fiscal_year, s.period, s.status FROM (SELECT c.ledger_id, g.code, g.is_leading
			FROM ledger.journal_entry c JOIN ledger.ledger g ON g.id = c.ledger_id
			WHERE c.fiscal_year = $3 AND (c.id = $4 OR c.posting_group_id = $4) ORDER BY NOT g.is_leading, g.code) c
		LEFT JOIN LATERAL (SELECT ps.fiscal_year, ps.period, ps.status FROM ledger.period_status ps
			JOIN ledger.fiscal_period fp ON fp.tenant_id = ps.tenant_id AND fp.calendar_id = ps.calendar_id
				AND fp.fiscal_year = ps.fiscal_year AND fp.period = ps.period
			WHERE ps.legal_entity_id = $1 AND ps.ledger_id = c.ledger_id AND NOT fp.is_special AND $2 BETWEEN fp.start_date AND fp.end_date
			FOR SHARE OF ps) s ON true
		ORDER BY NOT c.is_leading, c.code`, entity, date, by.FiscalYear, by.EntryID)
	type period struct {
		code         string
		leading      bool
		year, number *int16
		status       *string
	}
	var periods []period
	var p period
	if _, err := pgx.ForEachRow(rows, []any{&p.code, &p.leading, &p.year, &p.number, &p.status}, func() error {
		periods = append(periods, p)
		return nil
	}); err != nil {
		return fmt.Errorf("ledger: clearing period of %s on %s: %w", entity, day, err)
	}
	for _, p := range periods {
		if p.status == nil {
			return noFiscalPeriod("clearing_date", "No fiscal period of the legal entity in the clearing entry's ledger "+p.code+" covers "+day+".")
		}
		what := "clearings dated " + day
		if !p.leading {
			what += " in ledger " + p.code
		}
		if err := mustBeOpen(*p.year, *p.number, *p.status, what); err != nil {
			return err
		}
	}
	return nil
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
// ledger. The debits leave out the GR/IR take-overs a migration run posts
// (integration entries whose lines name order items, T72f), which move a
// balance within the GR/IR account and have no document in the source
// system; the balances count them, as they net to zero.
func (Reports) Totals(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (Totals, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Totals{}, err
	}
	rows, _ := tx.Query(ctx, `WITH takeover AS (SELECT DISTINCT l.journal_entry_id FROM ledger.journal_line l
			JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id AND e.source_context = 'integration'
			WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2 AND l.purchase_order_id IS NOT NULL)
		SELECT a.number, sum(l.amount_lc), coalesce(sum(greatest(l.amount_lc, 0)) FILTER (WHERE t.journal_entry_id IS NULL), 0)::bigint FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		LEFT JOIN takeover t ON t.journal_entry_id = l.journal_entry_id
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
// year in local currency per partner, in the leading ledger, without the
// special G/L items (down payments), which OpenDownPayments sums: like a
// payment run, the open payables do not net them (ADR-0037, T72c).
func (Reports) OpenPayables(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]money.Minor, error) {
	sums, _, err := openSupplierItems(ctx, legalEntity, fiscalYear, false)
	return sums, err
}

// OpenPayableItems counts the items OpenPayables sums, per partner: a
// migrated clearing the platform did not record leaves both of its items
// open, which the sums net away and the counts show (review:batch22).
func (Reports) OpenPayableItems(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]int, error) {
	_, counts, err := openSupplierItems(ctx, legalEntity, fiscalYear, false)
	return counts, err
}

// OpenDownPayments sums the legal entity's open down payments (special G/L
// A) of the fiscal year in local currency per partner, in the leading
// ledger.
func (Reports) OpenDownPayments(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]money.Minor, error) {
	sums, _, err := openSupplierItems(ctx, legalEntity, fiscalYear, true)
	return sums, err
}

// OpenPayablesByTradingPartner sums the legal entity's open supplier items
// of the fiscal year that carry a trading partner, ordinary items and
// down payments alike, in local currency per trading partner, in the
// leading ledger: the intercompany payables a consolidation run reads by
// trading partner, which a migration run reconciles with SAP's
// (ADR-0059 As built, the migration leaf).
func (Reports) OpenPayablesByTradingPartner(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.trading_partner_id, sum(l.amount_lc) FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2 AND l.trading_partner_id IS NOT NULL
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
		GROUP BY l.trading_partner_id`, legalEntity, fiscalYear)
	out := map[uuid.UUID]money.Minor{}
	var partner uuid.UUID
	var sum money.Minor
	if _, err := pgx.ForEachRow(rows, []any{&partner, &sum}, func() error { out[partner] = sum; return nil }); err != nil {
		return out, fmt.Errorf("ledger: open supplier items of %s %d by trading partner: %w", legalEntity, fiscalYear, err)
	}
	return out, nil
}

// OpenForeignItems sums the legal entity's open supplier items of the
// fiscal year posted in a currency other than its local one, ordinary
// items and down payments alike, in their transaction currency per
// partner and currency, in the leading ledger: what a migration run
// loaded in their document currency (ADR-0005 As built), which the local
// currency controls cannot tell from items loaded in the wrong amount.
func (Reports) OpenForeignItems(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]map[string]money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.partner_id, e.currency_tc, sum(l.amount_tc) FROM ledger.journal_line l
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
			AND e.currency_tc <> e.currency_lc
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
		GROUP BY l.partner_id, e.currency_tc`, legalEntity, fiscalYear)
	out := map[uuid.UUID]map[string]money.Minor{}
	var partner uuid.UUID
	var currency string
	var sum money.Minor
	_, err = pgx.ForEachRow(rows, []any{&partner, &currency, &sum}, func() error {
		if out[partner] == nil {
			out[partner] = map[string]money.Minor{}
		}
		out[partner][currency] = sum
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("ledger: open foreign-currency supplier items of %s %d: %w", legalEntity, fiscalYear, err)
	}
	return out, nil
}

// FixedMethodItems are open supplier items that fix one payment method:
// how many, and their sum in local currency.
type FixedMethodItems struct {
	Count int
	LC    money.Minor
}

// OpenItemsByMethod sums and counts the legal entity's open supplier
// items of the fiscal year that fix a payment method (ADR-0038 As built),
// in local currency per partner and method, in the leading ledger: what a
// migration run took over of the methods SAP fixed on its open items,
// which the open payables cannot tell from items loaded without one.
func (Reports) OpenItemsByMethod(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]map[string]FixedMethodItems, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.partner_id, pm.payment_method, count(*), sum(l.amount_lc) FROM ledger.journal_line l
		JOIN ledger.open_item_payment_method pm ON pm.tenant_id = l.tenant_id AND pm.fiscal_year = l.fiscal_year
			AND pm.journal_entry_id = l.journal_entry_id AND pm.line_no = l.line_no
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
		GROUP BY l.partner_id, pm.payment_method`, legalEntity, fiscalYear)
	out := map[uuid.UUID]map[string]FixedMethodItems{}
	var partner uuid.UUID
	var method string
	var items FixedMethodItems
	_, err = pgx.ForEachRow(rows, []any{&partner, &method, &items.Count, &items.LC}, func() error {
		if out[partner] == nil {
			out[partner] = map[string]FixedMethodItems{}
		}
		out[partner][method] = items
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("ledger: open supplier items of %s %d by fixed payment method: %w", legalEntity, fiscalYear, err)
	}
	return out, nil
}

// OpenGRIR is an order item's open GR/IR position as the migration
// reconciliation compares it: its quantity, invoiced + cleared + migrated -
// received, its balance in local currency, and in its transaction
// currency, the order's.
type OpenGRIR struct {
	Quantity string
	LC, TC   money.Minor
	Currency string
}

// OpenGRIR reads the open GR/IR positions of the orders' items in the
// legal entity (ledger.gr_ir_position), by order and item.
func (Reports) OpenGRIR(ctx context.Context, legalEntity uuid.UUID, orders []uuid.UUID) (map[uuid.UUID]map[int32]OpenGRIR, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT purchase_order_id, po_item_no, trim_scale(invoiced + cleared + migrated - received)::text, amount_lc, amount_tc,
			currency_tc
		FROM ledger.gr_ir_position WHERE legal_entity_id = $1 AND purchase_order_id = ANY($2) AND is_open`, legalEntity, orders)
	out := map[uuid.UUID]map[int32]OpenGRIR{}
	var order uuid.UUID
	var item int32
	var p OpenGRIR
	_, err = pgx.ForEachRow(rows, []any{&order, &item, &p.Quantity, &p.LC, &p.TC, &p.Currency}, func() error {
		if out[order] == nil {
			out[order] = map[int32]OpenGRIR{}
		}
		out[order][item] = p
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("ledger: open GR/IR positions of %d orders in %s: %w", len(orders), legalEntity, err)
	}
	return out, nil
}

// openSupplierItems sums and counts open supplier items per partner: the
// special G/L items, or the ordinary ones.
func openSupplierItems(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16, special bool) (map[uuid.UUID]money.Minor, map[uuid.UUID]int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.partner_id, sum(l.amount_lc), count(*) FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		WHERE l.legal_entity_id = $1 AND l.fiscal_year = $2 AND (l.special_gl IS NOT NULL) = $3
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
		GROUP BY l.partner_id`, legalEntity, fiscalYear, special)
	sums, counts := map[uuid.UUID]money.Minor{}, map[uuid.UUID]int{}
	var partner uuid.UUID
	var sum money.Minor
	var n int
	_, err = pgx.ForEachRow(rows, []any{&partner, &sum, &n}, func() error { sums[partner], counts[partner] = sum, n; return nil })
	if err != nil {
		return sums, counts, fmt.Errorf("ledger: open supplier items of %s %d: %w", legalEntity, fiscalYear, err)
	}
	return sums, counts, nil
}
