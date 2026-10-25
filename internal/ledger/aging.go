package ledger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Open items' aging (ADR-0052 G4): an item posted with payment terms
// (T36b) is due on their net due date, the baseline date plus the net
// days, computed as ItemTerms.NetDueDate computes it and never stored
// (ADR-0051 §1); on a key date it is not due, or overdue by the days past
// that date, counted into the buckets the caller names. An item without
// terms has no due date the ledger keeps: its supplier's current terms
// are the partner context's, which the payment proposal reads for itself.

// defaultAgingDays are the buckets' last days when the caller names none:
// not due, 1-30, 31-60, 61-90 and more than 90 days overdue.
var defaultAgingDays = []int32{30, 60, 90}

// agingDays are the buckets' last days asked for, the default without;
// days without a key date, or not ascending, are refused.
func agingDays(params gen.ListOpenItemsParams) ([]int32, error) {
	if !params.AgingKeyDate.Set {
		if len(params.AgingDays) > 0 {
			return nil, problem.BadRequest("request.invalid_parameter", "Aging buckets (aging_days) age the items on a key date; name it.",
				problem.FieldError{Field: "aging_key_date", Code: "required", Message: "the date the items are aged on"})
		}
		return nil, nil
	}
	if len(params.AgingDays) == 0 {
		return defaultAgingDays, nil
	}
	for i := 1; i < len(params.AgingDays); i++ {
		if params.AgingDays[i] <= params.AgingDays[i-1] {
			return nil, problem.BadRequest("request.invalid_parameter", "The aging buckets' days (aging_days) must ascend.",
				problem.FieldError{Field: "aging_days", Code: "not_ascending", Message: "each bucket's last day after the one before"})
		}
	}
	return params.AgingDays, nil
}

// agingSelect sums the open items openItemFilters select ($1-$7) per
// local currency and bucket on the key date $8: -1 without terms, 0 not
// due, k overdue past the k-1 first of the days $9; a bucket without items
// is left out.
const agingSelect = `SELECT bucket, currency_lc, exp_lc, count(*), sum(amount_lc)::bigint FROM (
	SELECT CASE WHEN o.net_days IS NULL THEN -1
		WHEN $8::date - (o.baseline_date + o.net_days) <= 0 THEN 0
		ELSE 1 + (SELECT count(*) FROM unnest($9::int[]) d WHERE d < $8::date - (o.baseline_date + o.net_days)) END AS bucket,
		o.currency_lc, o.exp_lc, o.amount_lc
	FROM (` + openItemSelect + openItemFilters + `) o) o) a
GROUP BY currency_lc, exp_lc, bucket ORDER BY currency_lc, bucket < 0, bucket`

// agingRow is one bucket of one local currency.
type agingRow struct {
	Bucket   int
	Currency string
	Exp      int
	Count    int64
	Amount   money.Minor
}

// aging is the aging of the open items the filters' args select, on the
// key date in the buckets of days.
func aging(ctx context.Context, tx pgx.Tx, args []any, key time.Time, days []int32) ([]gen.OpenItemAging, error) {
	rows, _ := tx.Query(ctx, agingSelect, append(slices.Clone(args), key, days)...)
	buckets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[agingRow])
	if err != nil {
		return nil, fmt.Errorf("ledger: age open items on %s: %w", key.Format(time.DateOnly), err)
	}
	out := make([]gen.OpenItemAging, len(buckets))
	for i, b := range buckets {
		out[i] = agingBucket(b, days)
	}
	return out, nil
}

// agingBucket is a bucket as the API shows it: an overdue one with its
// first and last day past the net due date, the last one open-ended.
func agingBucket(b agingRow, days []int32) gen.OpenItemAging {
	out := gen.OpenItemAging{Currency: b.Currency, ItemCount: b.Count, AmountLc: apitypes.Money(b.Amount, b.Currency, b.Exp)}
	switch {
	case b.Bucket < 0:
		out.Bucket = "no_due_date"
	case b.Bucket == 0:
		out.Bucket = "not_due"
	default:
		out.Bucket = "overdue"
		out.DaysFrom.SetTo(1)
		if b.Bucket > 1 {
			out.DaysFrom.SetTo(days[b.Bucket-2] + 1)
		}
		if b.Bucket <= len(days) {
			out.DaysTo.SetTo(days[b.Bucket-1])
		}
	}
	return out
}
