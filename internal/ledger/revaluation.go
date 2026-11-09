package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Revaluation is what a foreign-currency revaluation run of a legal entity
// and ledger at a key date values (ADR-0042): the open items of supplier
// and customer reconciliation accounts and the balances per currency of
// flagged accounts, each at the key date's rate of the ledger's valuation
// rate type: its own (T52f) or the legal entity's.
type Revaluation struct {
	LegalEntityID, LedgerID  uuid.UUID
	KeyDate                  time.Time
	FiscalYear, FiscalPeriod int16
	RateType                 string
	CurrencyLC, CurrencyGC   string
	ExpLC, ExpGC             int
	Items                    []RevaluedItem
}

// RevaluedItem is one valued open item or flagged account balance. Local
// and group currency are each valued from the transaction-currency amount
// at the key date's rate into them (SAP's parallel-currency valuation), so
// their differences can differ in size and sign. Its trading partner is
// an open item's line's, or the one a balance is kept for (ADR-0059 §4,
// T68b2).
type RevaluedItem struct {
	AccountID       uuid.UUID
	PartnerID       *uuid.UUID
	TradingPartner  *uuid.UUID
	Line            *ItemRef // the open item; nil for a balance
	CurrencyTC      string
	ExpTC           int
	AmountTC        money.Minor
	AmountLC        money.Minor // booked
	AmountGC        money.Minor // booked
	Rate            string      // into local currency, shown to 12 decimals; "" for a balance zero in its currency
	RateValidFrom   time.Time   // zero without Rate
	ValuedLC        money.Minor // AmountTC at Rate, rounded once
	DifferenceLC    money.Minor
	RateGC          string    // into group currency, "1" when CurrencyTC is the group's; "" without Rate
	RateGCValidFrom time.Time // zero when CurrencyTC is the group's or without Rate
	ValuedGC        money.Minor
	DifferenceGC    money.Minor
}

// Direction is gain for a positive local difference, loss for a negative
// one, and, without one, as the group difference's sign says; none when
// neither currency differs. The direction picks the gain or loss account,
// so a group difference of the other sign posts to the local one's.
func (it RevaluedItem) Direction() string {
	d := it.DifferenceLC
	if d == 0 {
		d = it.DifferenceGC
	}
	switch {
	case d > 0:
		return "gain"
	case d < 0:
		return "loss"
	}
	return "none"
}

// RevaluationTotal sums the items of one account, currency and trading
// partner (none being one) with a gain, or with a loss.
type RevaluationTotal struct {
	AccountID                                  uuid.UUID
	TradingPartner                             *uuid.UUID
	CurrencyTC                                 string
	ExpTC                                      int
	Direction                                  string
	Count                                      int32
	AmountTC, AmountLC, ValuedLC, DifferenceLC money.Minor
	AmountGC, ValuedGC, DifferenceGC           money.Minor
}

// revaluationScope reads what a run values at a key date, both kinds in
// one statement, in the currencies other than local: open items are lines
// on supplier or customer reconciliation accounts posted on or before the
// key date and neither cleared by a clearing dated on or before it nor
// reversed by a reversal dated on or before it (a reversal cancels its
// entry's items, and its own, without clearing them), down payments
// (special G/L) excluded; balances are of flagged accounts other
// than the GR/IR account, per currency, where the balance is not zero in
// one of the three currencies: a balance cleared to zero in its currency
// whose local or group balance is not is valued at zero, its residual
// being what the run corrects. A balance is one per trading partner, none
// being one, so an intercompany balance is valued and posted with its
// trading partner (ADR-0059 §4, T68b2). Each row says whether its account
// has an active valuation (fx_valuation_account), so a run without one
// fails closed. The account, partner, trading partner and line of an item
// are the journal line's own. $1
// legal entity; $2 ledger; $3 key date; $4 its fiscal year, which prunes
// later years' partitions. The scope is materialized: as the inner side of
// a join with the chart's accounts it was run once per account (T48b's
// budget).
const revaluationScope = `WITH scope AS MATERIALIZED (
		SELECT l.tenant_id, l.account_id, l.partner_id, l.trading_partner_id, l.fiscal_year, l.journal_entry_id, l.line_no, e.currency_tc, l.amount_tc,
			l.amount_lc, l.amount_gc
		FROM ledger.journal_line l
		JOIN ledger.gl_account g ON g.tenant_id = l.tenant_id AND g.id = l.account_id AND g.reconciliation_kind IN ('supplier', 'customer')
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id AND e.fiscal_year <= $4
		WHERE l.legal_entity_id = $1 AND l.ledger_id = $2 AND l.fiscal_year <= $4 AND l.posting_date <= $3 AND l.special_gl IS NULL
			AND l.amount_tc <> 0 AND e.currency_tc <> e.currency_lc
			AND NOT EXISTS (SELECT FROM ledger.clearing_item ci JOIN ledger.clearing c ON c.tenant_id = ci.tenant_id AND c.id = ci.clearing_id
				WHERE ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year AND ci.journal_entry_id = l.journal_entry_id
					AND ci.line_no = l.line_no AND c.clearing_date <= $3)
			AND NOT EXISTS (SELECT FROM ledger.reversal r JOIN ledger.journal_entry re ON re.tenant_id = r.tenant_id AND re.fiscal_year = r.reversal_year
					AND re.id = r.reversal_entry_id
				WHERE r.tenant_id = l.tenant_id AND re.posting_date <= $3
					AND ((r.original_year = l.fiscal_year AND r.original_entry_id = l.journal_entry_id)
						OR (r.reversal_year = l.fiscal_year AND r.reversal_entry_id = l.journal_entry_id)))
		UNION ALL
		SELECT l.tenant_id, l.account_id, NULL, l.trading_partner_id, NULL, NULL, NULL, e.currency_tc, sum(l.amount_tc)::bigint, sum(l.amount_lc)::bigint,
			sum(l.amount_gc)::bigint
		FROM ledger.journal_line l
		JOIN ledger.company_account ca ON ca.tenant_id = l.tenant_id AND ca.legal_entity_id = l.legal_entity_id AND ca.account_id = l.account_id
			AND ca.revalue_foreign_currency
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id AND e.fiscal_year <= $4
		WHERE l.legal_entity_id = $1 AND l.ledger_id = $2 AND l.fiscal_year <= $4 AND l.posting_date <= $3 AND e.currency_tc <> e.currency_lc
			AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = l.tenant_id AND d.legal_entity_id = l.legal_entity_id
				AND d.key = 'gr_ir' AND d.account_id = l.account_id)
		GROUP BY l.tenant_id, l.account_id, e.currency_tc, l.trading_partner_id
		HAVING sum(l.amount_tc) <> 0 OR sum(l.amount_lc) <> 0 OR sum(l.amount_gc) <> 0
	)
	SELECT s.account_id, s.partner_id, s.trading_partner_id, s.fiscal_year, s.journal_entry_id, s.line_no, s.currency_tc, c.exponent, s.amount_tc,
		s.amount_lc, s.amount_gc, g.number, v.account_id IS NOT NULL
	FROM scope s
	JOIN ledger.gl_account g ON g.tenant_id = s.tenant_id AND g.id = s.account_id
	JOIN ref.currency c ON c.code = s.currency_tc
	LEFT JOIN ledger.fx_valuation_account v ON v.tenant_id = s.tenant_id AND v.legal_entity_id = $1 AND v.account_id = s.account_id AND v.is_active
	ORDER BY g.number, s.currency_tc, s.fiscal_year, s.journal_entry_id, s.line_no, s.trading_partner_id NULLS FIRST`

// valueRevaluation values what a run of the legal entity and ledger at the
// key date would value, without writing anything: the ledger's own lines,
// so a parallel ledger values its copies of the items and its own
// postings (T52f). The key date is the last day of a posting period of the
// ledger; every currency is valued into local and into group currency at
// the latest rate of the ledger's rate type (its active
// fx_valuation_ledger, else the legal entity's) valid on it, one valid from
// before the key date's period being stale; and every account with items
// in scope has an active valuation.
func valueRevaluation(ctx context.Context, tx pgx.Tx, le tenancy.Entity, ledgerID uuid.UUID, group string, keyDate time.Time) (Revaluation, error) {
	r := Revaluation{LegalEntityID: le.ID, LedgerID: ledgerID, KeyDate: keyDate, CurrencyLC: le.LocalCurrency, CurrencyGC: group}
	var start time.Time
	// The ledger's own rate type, its valuation area's, else the legal
	// entity's (T52f, ADR-0045).
	err := tx.QueryRow(ctx, `SELECT f.fiscal_year, f.period, f.start_date,
			coalesce((SELECT rate_type FROM ledger.fx_valuation_ledger WHERE legal_entity_id = $1 AND ledger_id = $2 AND is_active),
				(SELECT rate_type FROM ledger.fx_valuation WHERE legal_entity_id = $1), 'average')
		FROM ledger.period_status s JOIN ledger.fiscal_period f ON f.tenant_id = s.tenant_id AND f.calendar_id = s.calendar_id
			AND f.fiscal_year = s.fiscal_year AND f.period = s.period
		WHERE s.legal_entity_id = $1 AND s.ledger_id = $2 AND f.end_date = $3 AND NOT f.is_special`,
		le.ID, ledgerID, keyDate).Scan(&r.FiscalYear, &r.FiscalPeriod, &start, &r.RateType)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revaluation{}, problem.Unprocessable("ledger.key_date_not_period_end",
			fmt.Sprintf("%s is not the last day of a posting period of the ledger; a revaluation values at a period end.", keyDate.Format(time.DateOnly)),
			problem.FieldError{Field: "key_date", Code: "not_period_end", Message: "the end date of a posting period"})
	}
	if err != nil {
		return Revaluation{}, fmt.Errorf("ledger: revaluation of %s at %s: key date's period: %w", le.ID, keyDate.Format(time.DateOnly), err)
	}
	exps, err := exponents(ctx, tx, le.LocalCurrency, group)
	if err != nil {
		return Revaluation{}, err
	}
	r.ExpLC, r.ExpGC = exps[le.LocalCurrency], exps[group]
	if r.Items, err = revaluationItems(ctx, tx, r, keyDate); err != nil {
		return Revaluation{}, err
	}
	return r, valueItems(ctx, tx, r, start)
}

// valueItems values each item at its currency's rates, looked up once per
// currency. A balance zero in its currency is worth zero at any rate, so
// it is valued without one: its difference is its booked residual, and a
// missing or stale rate of a currency the legal entity no longer holds
// does not stop the run (ADR-0042).
func valueItems(ctx context.Context, tx pgx.Tx, r Revaluation, start time.Time) error {
	// Zero, shown as no rate: any amount at it is zero.
	none := exchange{exact: new(big.Rat)}
	rates := map[string][2]exchange{}
	for i := range r.Items {
		it := &r.Items[i]
		x, ok := rates[it.CurrencyTC]
		switch {
		case it.AmountTC == 0:
			x = [2]exchange{none, none}
		case !ok:
			var err error
			if x[0], err = valuationRate(ctx, tx, r, it.CurrencyTC, r.CurrencyLC, start); err != nil {
				return err
			}
			x[1] = x[0]
			if r.CurrencyGC != r.CurrencyLC {
				if x[1], err = valuationRate(ctx, tx, r, it.CurrencyTC, r.CurrencyGC, start); err != nil {
					return err
				}
			}
			rates[it.CurrencyTC] = x
		}
		if err := it.value(x[0], x[1], r.ExpLC, r.ExpGC); err != nil {
			return err
		}
	}
	return nil
}

// revaluationItems reads the items in scope (revaluationScope), refusing
// the run when an account among them has no active valuation: its
// differences would have nowhere to post.
func revaluationItems(ctx context.Context, tx pgx.Tx, r Revaluation, keyDate time.Time) ([]RevaluedItem, error) {
	rows, _ := tx.Query(ctx, revaluationScope, r.LegalEntityID, r.LedgerID, keyDate, r.FiscalYear)
	var items []RevaluedItem
	var missing []string
	var it RevaluedItem
	var year *int16
	var partner, tradingPartner, entry *uuid.UUID
	var lineNo *int32
	var number string
	var valued bool
	_, err := pgx.ForEachRow(rows, []any{&it.AccountID, &partner, &tradingPartner, &year, &entry, &lineNo, &it.CurrencyTC, &it.ExpTC, &it.AmountTC,
		&it.AmountLC, &it.AmountGC, &number, &valued}, func() error {
		it.PartnerID, it.TradingPartner, it.Line = copied(partner), copied(tradingPartner), nil
		if entry != nil {
			it.Line = &ItemRef{FiscalYear: *year, EntryID: *entry, LineNo: *lineNo}
		}
		if !valued && (len(missing) == 0 || missing[len(missing)-1] != number) {
			missing = append(missing, number)
		}
		items = append(items, it)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: revaluation of %s at %s: items: %w", r.LegalEntityID, keyDate.Format(time.DateOnly), err)
	}
	if len(missing) > 0 {
		return nil, problem.Unprocessable("ledger.revaluation_account_missing",
			fmt.Sprintf("Account %s has items to revalue but no valuation accounts; add it to the company's fx_valuation.accounts.", strings.Join(missing, ", ")),
			problem.FieldError{Field: "legal_entity_id", Code: "revaluation_account_missing", Message: "accounts " + strings.Join(missing, ", ")})
	}
	return items, nil
}

// valuationRate is the rate a currency is valued at into local or group
// currency (to): 1 into itself, else the latest of the run's rate type
// valid on the key date, refused when there is none or it is valid from
// before the key date's period (start), and when its 12 decimals, what a
// revaluation item records, are zero.
func valuationRate(ctx context.Context, tx pgx.Tx, r Revaluation, currency, to string, start time.Time) (exchange, error) {
	if currency == to {
		return exchange{exact: big.NewRat(1, 1), shown: "1"}, nil
	}
	x, found, err := rateOfType(ctx, tx, r.RateType, currency, to, r.KeyDate)
	switch {
	case err != nil:
		return exchange{}, err
	case !found:
		return exchange{}, problem.Unprocessable("ledger.no_exchange_rate",
			fmt.Sprintf("No %s exchange rate from %s to %s is valid on %s.", r.RateType, currency, to, r.KeyDate.Format(time.DateOnly)),
			problem.FieldError{Field: "key_date", Code: "no_exchange_rate", Message: "record the " + r.RateType + " rate of the key date first"})
	case x.validFrom.Before(start):
		return exchange{}, problem.Unprocessable("ledger.rate_stale",
			fmt.Sprintf("The latest %s exchange rate from %s to %s on %s is valid from %s, before the key date's period starts on %s.", r.RateType, currency,
				to, r.KeyDate.Format(time.DateOnly), x.validFrom.Format(time.DateOnly), start.Format(time.DateOnly)),
			problem.FieldError{Field: "key_date", Code: "rate_stale", Message: "record a " + r.RateType + " rate valid from within the key date's period"})
	case money.ParseRat(x.shown).Sign() == 0:
		return exchange{}, problem.Unprocessable("ledger.rate_not_recordable",
			fmt.Sprintf("The %s rate from %s to %s, the reciprocal of the recorded %s/%s rate, is zero to the 12 decimals a revaluation records.",
				r.RateType, currency, to, to, currency),
			problem.FieldError{Field: "key_date", Code: "rate_not_recordable", Message: "record a " + currency + "/" + to + " rate"})
	}
	return x, nil
}

// value values the item from its transaction-currency amount into local
// currency at toLC and into group currency at toGC, each rounded once, and
// its differences to the booked amounts; an amount out of range refuses
// the run.
func (it *RevaluedItem) value(toLC, toGC exchange, expLC, expGC int) error {
	it.Rate, it.RateValidFrom, it.RateGC, it.RateGCValidFrom = toLC.shown, toLC.validFrom, toGC.shown, toGC.validFrom
	var err error
	if it.ValuedLC, it.DifferenceLC, err = valued(it.AmountTC, it.AmountLC, toLC, it.ExpTC, expLC); err != nil {
		return err
	}
	it.ValuedGC, it.DifferenceGC, err = valued(it.AmountTC, it.AmountGC, toGC, it.ExpTC, expGC)
	return err
}

// valued is an amount at a rate, rounded once, and its difference to what
// was booked.
func valued(tc, booked money.Minor, x exchange, expTC, exp int) (at, diff money.Minor, err error) {
	if at, err = money.ConvertAt(tc, x.exact, expTC, exp); err != nil {
		return 0, 0, errTotalTooLarge("key_date")
	}
	if diff, err = money.Diff(at, booked); err != nil {
		return 0, 0, errTotalTooLarge("key_date")
	}
	return at, diff, nil
}

// copied is a copy of the uuid id points to, nil for none: pgx scans
// each row into the same pointers.
func copied(id *uuid.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	c := *id
	return &c
}

// totalKey is what a total sums items by: account, currency, direction
// and trading partner, uuid.Nil for none (ADR-0059 §4, T68b2).
type totalKey struct {
	account, tradingPartner uuid.UUID
	currency, direction     string
}

// Totals sums the items with a difference per account, currency,
// direction and trading partner, in the items' order: an intercompany
// item's difference posts with its trading partner, and the items naming
// none sum as they did before trading partners.
func (r Revaluation) Totals() ([]RevaluationTotal, error) {
	var out []RevaluationTotal
	at := map[totalKey]int{}
	for _, it := range r.Items {
		d := it.Direction()
		if d == "none" {
			continue
		}
		k := totalKey{account: it.AccountID, currency: it.CurrencyTC, direction: d}
		if it.TradingPartner != nil {
			k.tradingPartner = *it.TradingPartner
		}
		i, ok := at[k]
		if !ok {
			i, at[k] = len(out), len(out)
			out = append(out, RevaluationTotal{AccountID: it.AccountID, TradingPartner: it.TradingPartner, CurrencyTC: it.CurrencyTC, ExpTC: it.ExpTC,
				Direction: d})
		}
		t := &out[i]
		t.Count++
		var err error
		for _, s := range []struct {
			sum *money.Minor
			add money.Minor
		}{{&t.AmountTC, it.AmountTC}, {&t.AmountLC, it.AmountLC}, {&t.ValuedLC, it.ValuedLC}, {&t.DifferenceLC, it.DifferenceLC},
			{&t.AmountGC, it.AmountGC}, {&t.ValuedGC, it.ValuedGC}, {&t.DifferenceGC, it.DifferenceGC}} {
			if *s.sum, err = money.Sum(*s.sum, s.add); err != nil {
				return nil, errTotalTooLarge("key_date")
			}
		}
	}
	return out, nil
}

// PreviewRevaluation implements POST /v1/revaluation-runs/preview: what a
// revaluation run would value (ADR-0042), read only and open to agents. A
// legal entity the caller cannot act in is not found.
func (e *Endpoints) PreviewRevaluation(ctx context.Context, req *gen.RevaluationPreviewRequest, _ gen.PreviewRevaluationParams) (gen.PreviewRevaluationRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, ledgerID, group, err := e.revaluationTarget(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	r, err := valueRevaluation(ctx, tx, le, ledgerID, group, req.KeyDate)
	if err != nil {
		return nil, err
	}
	totals, err := r.Totals()
	if err != nil {
		return nil, err
	}
	rep := revaluationToAPI(r, totals)
	if err := e.nameRevaluation(ctx, rep.Items, rep.Totals); err != nil {
		return nil, err
	}
	return &gen.RevaluationPreviewHeaders{Response: rep}, nil
}

// revaluationTarget resolves the legal entity, ledger (the leading one by
// default, else one the company keeps) and group currency a preview or run
// values; a legal entity the caller cannot act in is not found.
func (e *Endpoints) revaluationTarget(ctx context.Context, tx pgx.Tx, req *gen.RevaluationPreviewRequest) (tenancy.Entity, uuid.UUID, string, error) {
	le, err := e.visibleEntity(ctx, req.LegalEntityID)
	if err != nil {
		return tenancy.Entity{}, uuid.Nil, "", err
	}
	var ledgerID uuid.UUID
	var kept bool
	err = tx.QueryRow(ctx, `SELECT id, ledger.company_keeps($2, id) FROM ledger.ledger WHERE ($1::uuid IS NULL AND is_leading) OR id = $1`,
		apitypes.Ptr[uuid.UUID](req.LedgerID), le.ID).Scan(&ledgerID, &kept)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return tenancy.Entity{}, uuid.Nil, "", problem.ReferenceNotFound("ledger_id", "ledger", "no ledger with this id")
	case err != nil:
		return tenancy.Entity{}, uuid.Nil, "", fmt.Errorf("ledger: revaluation of %s: find ledger: %w", le.ID, err)
	case !kept:
		// Each ledger is valued on its own, so only one the company keeps.
		return tenancy.Entity{}, uuid.Nil, "", notKept("ledger_id")
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return tenancy.Entity{}, uuid.Nil, "", err
	}
	return le, ledgerID, group, nil
}

// revaluationToAPI renders a valuation and its totals.
func revaluationToAPI(r Revaluation, totals []RevaluationTotal) gen.RevaluationPreview {
	items, sums := revaluationItemsToAPI(r, totals)
	return gen.RevaluationPreview{LegalEntityID: r.LegalEntityID, LedgerID: r.LedgerID, KeyDate: r.KeyDate, FiscalYear: int32(r.FiscalYear),
		FiscalPeriod: int32(r.FiscalPeriod), RateType: r.RateType, Items: items, Totals: sums}
}

// revaluationItemsToAPI renders a valuation's items and totals.
func revaluationItemsToAPI(r Revaluation, totals []RevaluationTotal) ([]gen.RevaluationItem, []gen.RevaluationTotal) {
	items, sums := make([]gen.RevaluationItem, len(r.Items)), make([]gen.RevaluationTotal, len(totals))
	lc := func(m money.Minor) gen.Money { return apitypes.Money(m, r.CurrencyLC, r.ExpLC) }
	gc := func(m money.Minor) gen.Money { return apitypes.Money(m, r.CurrencyGC, r.ExpGC) }
	for i, it := range r.Items {
		item := gen.RevaluationItem{AccountID: it.AccountID, Amount: apitypes.Money(it.AmountTC, it.CurrencyTC, it.ExpTC), AmountLc: lc(it.AmountLC),
			AmountGc: gc(it.AmountGC), ValuedLc: lc(it.ValuedLC), DifferenceLc: lc(it.DifferenceLC), ValuedGc: gc(it.ValuedGC),
			DifferenceGc: gc(it.DifferenceGC), Direction: it.Direction()}
		// A balance zero in its currency has no rates (valueItems).
		if it.Rate != "" {
			item.Rate.SetTo(gen.Decimal(it.Rate))
			item.RateValidFrom.SetTo(it.RateValidFrom)
			item.RateGc.SetTo(gen.Decimal(it.RateGC))
		}
		if !it.RateGCValidFrom.IsZero() {
			item.RateGcValidFrom.SetTo(it.RateGCValidFrom)
		}
		if it.PartnerID != nil {
			item.PartnerID.SetTo(*it.PartnerID)
		}
		item.TradingPartner = tradingPartnerRef(it.TradingPartner)
		if it.Line != nil {
			item.FiscalYear.SetTo(int32(it.Line.FiscalYear))
			item.JournalEntryID.SetTo(it.Line.EntryID)
			item.LineNo.SetTo(it.Line.LineNo)
		}
		items[i] = item
	}
	for i, t := range totals {
		sums[i] = gen.RevaluationTotal{AccountID: t.AccountID, TradingPartner: tradingPartnerRef(t.TradingPartner), Currency: t.CurrencyTC,
			Direction: t.Direction, ItemCount: t.Count,
			Amount: apitypes.Money(t.AmountTC, t.CurrencyTC, t.ExpTC), AmountLc: lc(t.AmountLC), ValuedLc: lc(t.ValuedLC),
			DifferenceLc: lc(t.DifferenceLC), AmountGc: gc(t.AmountGC), ValuedGc: gc(t.ValuedGC), DifferenceGc: gc(t.DifferenceGC)}
	}
	return items, sums
}
