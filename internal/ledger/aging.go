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
	return AgingDays(params.AgingDays)
}

// AgingDays are the overdue buckets' last days an aging is asked for
// (aging_days): the default, 30, 60 and 90, without any; days that do not
// ascend are refused (400 request.invalid_parameter naming aging_days).
// The open items' aging and the AR aging (receivables, T75e) share it.
func AgingDays(days []int32) ([]int32, error) {
	if len(days) == 0 {
		return slices.Clone(defaultAgingDays), nil
	}
	for i := 1; i < len(days); i++ {
		if days[i] <= days[i-1] {
			return nil, problem.BadRequest("request.invalid_parameter", "The aging buckets' days (aging_days) must ascend.",
				problem.FieldError{Field: "aging_days", Code: "not_ascending", Message: "each bucket's last day after the one before"})
		}
	}
	return days, nil
}

// AgingBucket is the bucket of an item that is daysOverdue days past its
// net due date on the key date (ReceivableItem.DaysOverdue), as
// agingSelect counts the open items' in SQL: -1 for an item without terms
// (due false), which has no due date; 0 for one not yet due, due on the
// key date included; else k, overdue past the k-1 first of the days, so 1
// is up to days[0] days overdue and len(days)+1 more than the last.
// TestAgingBucketAsTheList holds the two to each other.
func AgingBucket(daysOverdue int, due bool, days []int32) int {
	switch {
	case !due:
		return -1
	case daysOverdue <= 0:
		return 0
	}
	bucket := 1
	for _, d := range days {
		if int(d) < daysOverdue {
			bucket++
		}
	}
	return bucket
}

// AgingRange names a bucket as the API shows it: no_due_date (-1),
// not_due (0), or overdue with its first and last day past the net due
// date, the last bucket open-ended (to nil).
func AgingRange(bucket int, days []int32) (name string, from, to *int32) {
	switch {
	case bucket < 0:
		return "no_due_date", nil, nil
	case bucket == 0:
		return "not_due", nil, nil
	}
	first := int32(1)
	if bucket > 1 {
		first = days[bucket-2] + 1
	}
	if bucket <= len(days) {
		to = &days[bucket-1]
	}
	return "overdue", &first, to
}

// agingSelect sums the open items openItemFilters select ($1-$8) per
// local currency and bucket on the key date $9: -1 without terms, 0 not
// due, k overdue past the k-1 first of the days $10; a bucket without items
// is left out.
const agingSelect = `SELECT bucket, currency_lc, exp_lc, count(*), sum(amount_lc)::bigint FROM (
	SELECT CASE WHEN o.net_days IS NULL THEN -1
		WHEN $9::date - (o.baseline_date + o.net_days) <= 0 THEN 0
		ELSE 1 + (SELECT count(*) FROM unnest($10::int[]) d WHERE d < $9::date - (o.baseline_date + o.net_days)) END AS bucket,
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
	name, from, to := AgingRange(b.Bucket, days)
	return gen.OpenItemAging{Bucket: name, DaysFrom: apitypes.OptInt32(from), DaysTo: apitypes.OptInt32(to), Currency: b.Currency, ItemCount: b.Count,
		AmountLc: apitypes.Money(b.Amount, b.Currency, b.Exp)}
}
