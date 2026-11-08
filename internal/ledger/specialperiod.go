package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Postings into the special periods 13-16 (ADR-0060 §3, T64m1): a year-end
// adjustment is a manual entry, posted directly or parked first, or a
// reversal through the API, that names its special period. The contexts'
// postings never do: only these paths set NewEntry.special or call
// reverseManually, and each checks it here before anything is written.
// postingPeriod then reads the special period's status, FOR SHARE in the
// postings' order, as it reads a regular one's, and the database refuses
// any other posting into a special period (check_entry_period, 000225).

// checkSpecial holds a prepared manual entry naming a special period to
// specialPeriod's rules; one naming none passes.
func checkSpecial(ctx context.Context, tx pgx.Tx, v prepared, in NewEntry) error {
	if in.special == nil {
		return nil
	}
	return specialPeriod(ctx, tx, v.le.FiscalCalendarID, in.PostingDate, *in.special, v.lines)
}

// specialPeriod refuses a posting dated date into the special period n of
// the calendar's fiscal year of date unless the calendar has it (422
// ledger.no_fiscal_period), the date is in its dates, which are the
// year's last regular period's (422 ledger.special_period_date), and no
// line carries a tax code (422 ledger.special_period_tax_code): tax
// returns select their items by the tax period's dates, and the product
// owner keeps the refusal until the tax specialist confirms how year-end
// adjustments enter the returns (ADR-0060 question 2, 2026-11-05).
// Whether the period is open is postingPeriod's to say.
func specialPeriod(ctx context.Context, tx pgx.Tx, calendar uuid.UUID, date time.Time, n int16, lines []Line) error {
	day := date.Format(time.DateOnly)
	var year int16
	var start, end *time.Time
	err := tx.QueryRow(ctx, `SELECT r.fiscal_year, s.start_date, s.end_date FROM ledger.fiscal_period r
		LEFT JOIN ledger.fiscal_period s ON s.tenant_id = r.tenant_id AND s.calendar_id = r.calendar_id AND s.fiscal_year = r.fiscal_year
			AND s.period = $3 AND s.is_special
		WHERE r.calendar_id = $1 AND NOT r.is_special AND $2 BETWEEN r.start_date AND r.end_date`, calendar, date, n).Scan(&year, &start, &end)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return noFiscalPeriod("posting_date", "No fiscal period of the legal entity covers "+day+
			"; a fiscal year is set up by a number range for it in the configuration.")
	case err != nil:
		return fmt.Errorf("ledger: special period %d of calendar %s on %s: %w", n, calendar, day, err)
	case start == nil:
		return noFiscalPeriod("fiscal_period", fmt.Sprintf("The fiscal calendar has no special period %d in fiscal year %d.", n, year))
	case date.Before(*start) || date.After(*end):
		return problem.Unprocessable("ledger.special_period_date", fmt.Sprintf("Special period %d/%d takes postings dated %s to %s, the year's last regular period, whose dates it shares; this one is dated %s.",
			year, n, start.Format(time.DateOnly), end.Format(time.DateOnly), day),
			problem.FieldError{Field: "posting_date", Code: "special_period_date", Message: "a date from " + start.Format(time.DateOnly) + " to " + end.Format(time.DateOnly)})
	}
	for i, l := range lines {
		if l.TaxCode != nil {
			return problem.Unprocessable("ledger.special_period_tax_code", fmt.Sprintf("Line %d carries tax code %s; a posting into special period %d/%d carries no tax code "+
				"(ADR-0060 question 2: refused until the tax specialist confirms how year-end adjustments enter the returns).", i+1, *l.TaxCode, year, n),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].tax_code", i), Code: "special_period_tax_code", Message: "no tax code in a special period"})
		}
	}
	return nil
}

// specialOf is the special period a request names, or nil.
func specialOf(o gen.OptInt32) *int16 {
	v, ok := o.Get()
	if !ok {
		return nil
	}
	n := int16(v) // 13 to 16: the contract's bounds, which ogen validates
	return &n
}

// specialChange maps a merge patch's fiscal_period: absent → nil, null →
// a pointer to nil (the regular period again), a value → a pointer to it.
func specialChange(o gen.OptNilInt32) **int16 {
	if !o.Set {
		return nil
	}
	var v *int16
	if !o.Null {
		n := int16(o.Value) // 13 to 16, as specialOf
		v = &n
	}
	return &v
}
