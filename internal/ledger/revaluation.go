package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Revaluation is what a foreign-currency revaluation run of a legal entity
// and ledger at a key date values (ADR-0042): the open items of supplier
// and customer reconciliation accounts and the balances per currency of
// flagged accounts, each at the key date's rate of the legal entity's
// valuation rate type.
type Revaluation struct {
	LegalEntityID, LedgerID  uuid.UUID
	KeyDate                  time.Time
	FiscalYear, FiscalPeriod int16
	RateType                 string
	CurrencyLC, CurrencyGC   string
	ExpLC, ExpGC             int
	Items                    []RevaluedItem
}

// RevaluedItem is one valued open item or flagged account balance.
type RevaluedItem struct {
	AccountID     uuid.UUID
	PartnerID     *uuid.UUID
	Line          *ItemRef // the open item; nil for a balance
	CurrencyTC    string
	ExpTC         int
	AmountTC      money.Minor
	AmountLC      money.Minor // booked
	AmountGC      money.Minor // booked
	Rate          string      // shown, to 12 decimals
	RateValidFrom time.Time
	ValuedLC      money.Minor // AmountTC at the rate, rounded once
	DifferenceLC  money.Minor
	DifferenceGC  money.Minor // DifferenceLC at the key date's average rate
}

// Direction is gain for a positive difference, loss for a negative one and
// none without one.
func (it RevaluedItem) Direction() string {
	switch {
	case it.DifferenceLC > 0:
		return "gain"
	case it.DifferenceLC < 0:
		return "loss"
	}
	return "none"
}

// RevaluationTotal sums the items of one account and currency with a gain,
// or with a loss.
type RevaluationTotal struct {
	AccountID                                  uuid.UUID
	CurrencyTC                                 string
	ExpTC                                      int
	Direction                                  string
	Count                                      int32
	AmountTC, AmountLC, ValuedLC, DifferenceLC money.Minor
	DifferenceGC                               money.Minor
}

// revaluationScope reads what a run values at a key date, both kinds in
// one statement, in the currencies other than local: open items are lines
// on supplier or customer reconciliation accounts posted on or before the
// key date and neither cleared by a clearing dated on or before it nor
// reversed by a reversal dated on or before it (a reversal cancels its
// entry's items, and its own, without clearing them), down payments
// (special G/L) excluded; balances are of flagged accounts other
// than the GR/IR account, per currency, where the transaction currency
// balance is not zero. Each row says whether its account has an active
// valuation (fx_valuation_account), so a run without one fails closed. The
// account, partner and line of an item are the journal line's own. $1
// legal entity; $2 ledger; $3 key date; $4 its fiscal year, which prunes
// later years' partitions. The scope is materialized: as the inner side of
// a join with the chart's accounts it was run once per account (T48b's
// budget).
const revaluationScope = `WITH scope AS MATERIALIZED (
		SELECT l.tenant_id, l.account_id, l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no, e.currency_tc, l.amount_tc, l.amount_lc, l.amount_gc
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
		SELECT l.tenant_id, l.account_id, NULL, NULL, NULL, NULL, e.currency_tc, sum(l.amount_tc)::bigint, sum(l.amount_lc)::bigint, sum(l.amount_gc)::bigint
		FROM ledger.journal_line l
		JOIN ledger.company_account ca ON ca.tenant_id = l.tenant_id AND ca.legal_entity_id = l.legal_entity_id AND ca.account_id = l.account_id
			AND ca.revalue_foreign_currency
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id AND e.fiscal_year <= $4
		WHERE l.legal_entity_id = $1 AND l.ledger_id = $2 AND l.fiscal_year <= $4 AND l.posting_date <= $3 AND e.currency_tc <> e.currency_lc
			AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = l.tenant_id AND d.legal_entity_id = l.legal_entity_id
				AND d.key = 'gr_ir' AND d.account_id = l.account_id)
		GROUP BY l.tenant_id, l.account_id, e.currency_tc
		HAVING sum(l.amount_tc) <> 0
	)
	SELECT s.account_id, s.partner_id, s.fiscal_year, s.journal_entry_id, s.line_no, s.currency_tc, c.exponent, s.amount_tc, s.amount_lc, s.amount_gc,
		g.number, v.account_id IS NOT NULL
	FROM scope s
	JOIN ledger.gl_account g ON g.tenant_id = s.tenant_id AND g.id = s.account_id
	JOIN ref.currency c ON c.code = s.currency_tc
	LEFT JOIN ledger.fx_valuation_account v ON v.tenant_id = s.tenant_id AND v.legal_entity_id = $1 AND v.account_id = s.account_id AND v.is_active
	ORDER BY g.number, s.currency_tc, s.fiscal_year, s.journal_entry_id, s.line_no`

// valueRevaluation values what a run of the legal entity and ledger at the
// key date would value, without writing anything. The key date is the last
// day of a posting period of the ledger; every currency is valued at the
// latest rate of the legal entity's rate type valid on it, one valid from
// before the key date's period being stale; and every account with items
// in scope has an active valuation.
func valueRevaluation(ctx context.Context, tx pgx.Tx, le tenancy.Entity, ledgerID uuid.UUID, group string, keyDate time.Time) (Revaluation, error) {
	r := Revaluation{LegalEntityID: le.ID, LedgerID: ledgerID, KeyDate: keyDate, CurrencyLC: le.LocalCurrency, CurrencyGC: group}
	var start time.Time
	err := tx.QueryRow(ctx, `SELECT f.fiscal_year, f.period, f.start_date,
			coalesce((SELECT rate_type FROM ledger.fx_valuation WHERE legal_entity_id = $1), 'average')
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
	toGC, err := rate(ctx, tx, le.LocalCurrency, group, keyDate)
	if err != nil {
		return Revaluation{}, err
	}
	if r.Items, err = revaluationItems(ctx, tx, r, keyDate); err != nil {
		return Revaluation{}, err
	}
	rates := map[string]exchange{}
	for i := range r.Items {
		it := &r.Items[i]
		x, ok := rates[it.CurrencyTC]
		if !ok {
			if x, err = valuationRate(ctx, tx, r, it.CurrencyTC, start); err != nil {
				return Revaluation{}, err
			}
			rates[it.CurrencyTC] = x
		}
		if err := it.value(x, toGC, r.ExpLC, r.ExpGC); err != nil {
			return Revaluation{}, err
		}
	}
	return r, nil
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
	var partner, entry *uuid.UUID
	var lineNo *int32
	var number string
	var valued bool
	_, err := pgx.ForEachRow(rows, []any{&it.AccountID, &partner, &year, &entry, &lineNo, &it.CurrencyTC, &it.ExpTC, &it.AmountTC, &it.AmountLC,
		&it.AmountGC, &number, &valued}, func() error {
		// Copied: pgx scans the next row into the same pointers.
		it.PartnerID, it.Line = nil, nil
		if partner != nil {
			p := *partner
			it.PartnerID = &p
		}
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

// valuationRate is the rate a currency is valued at: the latest of the
// run's rate type valid on the key date, refused when there is none or it
// is valid from before the key date's period (start), and when its 12
// decimals, what a revaluation item records, are zero.
func valuationRate(ctx context.Context, tx pgx.Tx, r Revaluation, currency string, start time.Time) (exchange, error) {
	x, found, err := rateOfType(ctx, tx, r.RateType, currency, r.CurrencyLC, r.KeyDate)
	switch {
	case err != nil:
		return exchange{}, err
	case !found:
		return exchange{}, problem.Unprocessable("ledger.no_exchange_rate",
			fmt.Sprintf("No %s exchange rate from %s to %s is valid on %s.", r.RateType, currency, r.CurrencyLC, r.KeyDate.Format(time.DateOnly)),
			problem.FieldError{Field: "key_date", Code: "no_exchange_rate", Message: "record the " + r.RateType + " rate of the key date first"})
	case x.validFrom.Before(start):
		return exchange{}, problem.Unprocessable("ledger.rate_stale",
			fmt.Sprintf("The latest %s exchange rate from %s to %s on %s is valid from %s, before the key date's period starts on %s.", r.RateType, currency,
				r.CurrencyLC, r.KeyDate.Format(time.DateOnly), x.validFrom.Format(time.DateOnly), start.Format(time.DateOnly)),
			problem.FieldError{Field: "key_date", Code: "rate_stale", Message: "record a " + r.RateType + " rate valid from within the key date's period"})
	case money.ParseRat(x.shown).Sign() == 0:
		return exchange{}, problem.Unprocessable("ledger.rate_not_recordable",
			fmt.Sprintf("The %s rate from %s to %s, the reciprocal of the recorded %s/%s rate, is zero to the 12 decimals a revaluation records.",
				r.RateType, currency, r.CurrencyLC, r.CurrencyLC, currency),
			problem.FieldError{Field: "key_date", Code: "rate_not_recordable", Message: "record a " + currency + "/" + r.CurrencyLC + " rate"})
	}
	return x, nil
}

// value values the item at the rate into local currency, rounded once,
// and its difference into group currency at toGC, as a posting of it
// would convert; an amount out of range refuses the run.
func (it *RevaluedItem) value(x, toGC exchange, expLC, expGC int) error {
	it.Rate, it.RateValidFrom = x.shown, x.validFrom
	var err error
	if it.ValuedLC, err = money.ConvertAt(it.AmountTC, x.exact, it.ExpTC, expLC); err != nil {
		return errTotalTooLarge("key_date")
	}
	if it.DifferenceLC, err = money.Diff(it.ValuedLC, it.AmountLC); err != nil {
		return errTotalTooLarge("key_date")
	}
	if it.DifferenceGC, err = money.ConvertAt(it.DifferenceLC, toGC.exact, expLC, expGC); err != nil {
		return errTotalTooLarge("key_date")
	}
	return nil
}

// Totals sums the items with a difference per account, currency and
// direction, in the items' order.
func (r Revaluation) Totals() ([]RevaluationTotal, error) {
	var out []RevaluationTotal
	at := map[string]int{}
	for _, it := range r.Items {
		d := it.Direction()
		if d == "none" {
			continue
		}
		k := it.AccountID.String() + "/" + it.CurrencyTC + "/" + d
		i, ok := at[k]
		if !ok {
			i, at[k] = len(out), len(out)
			out = append(out, RevaluationTotal{AccountID: it.AccountID, CurrencyTC: it.CurrencyTC, ExpTC: it.ExpTC, Direction: d})
		}
		t := &out[i]
		t.Count++
		var err error
		for _, s := range []struct {
			sum *money.Minor
			add money.Minor
		}{{&t.AmountTC, it.AmountTC}, {&t.AmountLC, it.AmountLC},
			{&t.ValuedLC, it.ValuedLC}, {&t.DifferenceLC, it.DifferenceLC}, {&t.DifferenceGC, it.DifferenceGC}} {
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
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, req.LegalEntityID)
	if err != nil {
		return nil, err
	}
	if !ok || !access.Allows(le.ID) {
		return nil, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id")
	}
	var ledgerID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM ledger.ledger WHERE ($1::uuid IS NULL AND is_leading) OR id = $1`, apitypes.Ptr[uuid.UUID](req.LedgerID)).Scan(&ledgerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem.ReferenceNotFound("ledger_id", "ledger", "no ledger with this id")
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: revaluation preview of %s: find ledger: %w", le.ID, err)
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
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
	return &gen.RevaluationPreviewHeaders{Response: revaluationToAPI(r, totals)}, nil
}

// revaluationToAPI renders a valuation and its totals.
func revaluationToAPI(r Revaluation, totals []RevaluationTotal) gen.RevaluationPreview {
	out := gen.RevaluationPreview{LegalEntityID: r.LegalEntityID, LedgerID: r.LedgerID, KeyDate: r.KeyDate, FiscalYear: int32(r.FiscalYear),
		FiscalPeriod: int32(r.FiscalPeriod), RateType: r.RateType, Items: make([]gen.RevaluationPreviewItem, len(r.Items)),
		Totals: make([]gen.RevaluationPreviewTotal, len(totals))}
	lc := func(m money.Minor) gen.Money { return apitypes.Money(m, r.CurrencyLC, r.ExpLC) }
	gc := func(m money.Minor) gen.Money { return apitypes.Money(m, r.CurrencyGC, r.ExpGC) }
	for i, it := range r.Items {
		item := gen.RevaluationPreviewItem{AccountID: it.AccountID, Amount: apitypes.Money(it.AmountTC, it.CurrencyTC, it.ExpTC), AmountLc: lc(it.AmountLC),
			AmountGc: gc(it.AmountGC), Rate: gen.Decimal(it.Rate), RateValidFrom: it.RateValidFrom, ValuedLc: lc(it.ValuedLC),
			DifferenceLc: lc(it.DifferenceLC), DifferenceGc: gc(it.DifferenceGC), Direction: it.Direction()}
		if it.PartnerID != nil {
			item.PartnerID.SetTo(*it.PartnerID)
		}
		if it.Line != nil {
			item.FiscalYear.SetTo(int32(it.Line.FiscalYear))
			item.JournalEntryID.SetTo(it.Line.EntryID)
			item.LineNo.SetTo(it.Line.LineNo)
		}
		out.Items[i] = item
	}
	for i, t := range totals {
		out.Totals[i] = gen.RevaluationPreviewTotal{AccountID: t.AccountID, Currency: t.CurrencyTC, Direction: t.Direction, ItemCount: t.Count,
			Amount: apitypes.Money(t.AmountTC, t.CurrencyTC, t.ExpTC), AmountLc: lc(t.AmountLC), ValuedLc: lc(t.ValuedLC),
			DifferenceLc: lc(t.DifferenceLC), DifferenceGc: gc(t.DifferenceGC)}
	}
	return out
}
