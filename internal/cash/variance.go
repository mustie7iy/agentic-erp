package cash

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The forecast variance (ADR-0051 §11, T53f3): what a snapshot taken lag
// days before a period's start forecast for it against what the bank
// statements show, per currency, liquidity item and bucket. Within the 31
// days a snapshot keeps per document, each document it forecast is matched
// to the statement lines that paid it (a run's payment by the payment, a
// supplier item by its journal entry or, forecast before it was posted, by
// its invoice, a down payment request by the payment paying it, a memo
// record by the line that realised it), and the variance is attributed:
// timing (a document forecast in one bucket and paid in another), amount
// (paid in its bucket at another amount), new (paid and not forecast),
// missing (forecast and not paid) and unclassified (a line with no decided
// match that no forecast document explains). Beyond the 31 days a snapshot
// keeps bucket sums, so the variance is per item only. Computed at the
// read from one database state (x-snapshot); nothing is stored.

// The lags a variance compares at (ADR-0051 §11: 0, 7 and 30 days
// offered), and the default.
var varianceLags = []int{0, 7, 30}

// DefaultLag is the lag of a variance that names none.
const DefaultLag = 7

// VarianceQuery is what a variance covers: the legal entity's period from
// From to To, against its snapshot taken Lag days before From, in buckets
// of Grain ("": a day).
type VarianceQuery struct {
	LegalEntity uuid.UUID
	From, To    time.Time
	Lag         int
	Grain       string
}

// Variance is a legal entity's forecast against its actuals over a
// period: Buckets are the days kept per document (up to To or the
// snapshot's DocumentLevelUntil, whichever is first), attributed; Beyond
// the days after them compared per item only, nil when there are none.
type Variance struct {
	LegalEntity uuid.UUID
	CompanyCode string
	ReadDate    time.Time
	TimeZone    string
	From, To    time.Time
	Lag         int
	Grain       string
	Snapshot    Snapshot
	Buckets     []Bucket
	Beyond      *Period
	Currencies  []VarianceCurrency
}

// Period is the days from From to To.
type Period struct{ From, To time.Time }

// VarianceCurrency is one currency's variance per bucket, over the
// attributed buckets, beyond them, and by liquidity item and its
// hierarchy.
type VarianceCurrency struct {
	Currency string
	Exponent int
	Buckets  []VarianceCell
	Total    VarianceCell
	Beyond   Figures
	Items    []VarianceRow
}

// VarianceRow is a liquidity item's variance with its descendants': the
// item, its name, parent and level as the forecast's rows (ItemRow); Item
// "" is the row of what no rule assigns.
type VarianceRow struct {
	Item, Name, Parent string
	Level              int
	Buckets            []VarianceCell
	Total              VarianceCell
	Beyond             Figures
}

// Figures are what was forecast and what was actual, signed as the flows
// (money in positive), and the variance, actual less forecast.
type Figures struct {
	Forecast, Actual, Variance money.Minor
}

// VarianceCell is a bucket's figures with the variance attributed: the
// five add up to the variance.
type VarianceCell struct {
	Figures
	Timing, Amount, New, Missing, Unclassified money.Minor
}

// errNoSnapshot refuses a variance whose snapshot was not taken.
func errNoSnapshot(date time.Time) error {
	return problem.Conflict("cash.snapshot_not_taken", fmt.Sprintf("No snapshot of the liquidity forecast was taken on %s, the day the variance "+
		"compares against: a daily snapshot is taken from the company's cash.snapshot_time, and one may be taken on demand.", date.Format(time.DateOnly)))
}

// errVarianceTooLarge refuses a variance whose sums are out of the range
// of an amount.
var errVarianceTooLarge = problem.Unprocessable("cash.amount_too_large", "The amounts of the forecast variance add up to more than an amount can hold.")

// Variance computes the legal entity's forecast variance. A legal entity
// the caller cannot act in is not found; a lag other than 0, 7 and 30, a
// grain not known, a period ending before it starts, after the company's
// date or past the snapshot's horizon, or of more periods than the
// grain's limit, is refused; a period whose snapshot was not taken is a
// conflict.
func (e *Endpoints) Variance(ctx context.Context, q VarianceQuery) (Variance, error) {
	from, to, grain := clock.Day(q.From), clock.Day(q.To), cmp.Or(q.Grain, config.GrainDay)
	if !slices.Contains(varianceLags, q.Lag) {
		return Variance{}, problem.Unprocessable("request.validation_failed", "A variance compares at a lag of 0, 7 or 30 days.",
			problem.FieldError{Field: "lag", Code: "invalid", Message: "0, 7 or 30"})
	}
	if _, err := actualBuckets(from, to, grain); err != nil {
		return Variance{}, err
	}
	taken := from.AddDate(0, 0, -q.Lag)
	docEnd := taken.AddDate(0, 0, documentLevelDays)
	ar, err := e.readActuals(ctx, q.LegalEntity, taken, maxDay(to, docEnd))
	if err != nil {
		return Variance{}, err
	}
	if to.After(ar.r.read) {
		return Variance{}, problem.Unprocessable("request.validation_failed", "A variance compares days that have passed.",
			problem.FieldError{Field: "to", Code: "invalid", Message: "on or before " + ar.r.read.Format(time.DateOnly) + ", the company's date"})
	}
	s, err := snapshotOn(ctx, ar.tx, q.LegalEntity, taken)
	if err != nil {
		return Variance{}, err
	}
	s.CompanyCode = ar.co.Code
	if to.After(s.HorizonEnd) {
		return Variance{}, problem.Unprocessable("request.validation_failed", "The snapshot forecast no further than its horizon.",
			problem.FieldError{Field: "to", Code: "invalid", Message: "on or before " + s.HorizonEnd.Format(time.DateOnly) + ", the snapshot's horizon"})
	}
	v := Variance{LegalEntity: q.LegalEntity, CompanyCode: ar.co.Code, ReadDate: ar.r.read, TimeZone: ar.zone, From: from, To: to, Lag: q.Lag,
		Grain: grain, Snapshot: s}
	// Safe to ignore: the period's buckets were checked above, and these end
	// no later.
	v.Buckets, _ = actualBuckets(from, minDay(to, docEnd), grain)
	if v.Currencies, v.Beyond, err = e.compare(ctx, ar, s, v.Buckets, to); err != nil {
		return Variance{}, err
	}
	if ar.r.sums.overflow {
		return Variance{}, errVarianceTooLarge
	}
	return v, nil
}

// minDay is the earlier day.
func minDay(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// snapshotOn is the legal entity's snapshot of the date the variance
// compares against: its daily snapshot, else the last taken on demand
// that day.
func snapshotOn(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, date time.Time) (Snapshot, error) {
	s, err := scanSnapshot(tx.QueryRow(ctx, snapshotSelect+` WHERE s.legal_entity_id = $1 AND s.snapshot_date = $2
		ORDER BY s.kind = 'daily' DESC, s.taken_at DESC, s.id DESC LIMIT 1`, legalEntity, date))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Snapshot{}, errNoSnapshot(date)
	case err != nil:
		return Snapshot{}, fmt.Errorf("cash: snapshot of %s on %s: %w", legalEntity, date.Format(time.DateOnly), err)
	}
	return s, nil
}

// keptLine is a snapshot's line as the variance reads it.
type keptLine struct {
	date     time.Time
	end      *time.Time
	currency string
	item     string
	amount   money.Minor
	payment  *uuid.UUID
	doc      *Document
}

// keptLines are the snapshot's lines dated up to the last day, but what
// blocked suppliers were owed: the forecast's totals did not count them
// (ADR-0051 §9, T53e2), so neither does the variance.
func keptLines(ctx context.Context, tx pgx.Tx, snapshot uuid.UUID, last time.Time) ([]keptLine, error) {
	rows, _ := tx.Query(ctx, `SELECT f.value_date, f.bucket_end, f.currency, coalesce(f.liquidity_item, ''), f.amount, f.payment_id,
			f.document_kind, f.document_id
		FROM cash.snapshot_flow f WHERE f.snapshot_id = $1 AND f.source <> 'payable_blocked' AND f.value_date <= $2 ORDER BY f.line_no`, snapshot, last)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (keptLine, error) {
		var l keptLine
		var kind *string
		var id *uuid.UUID
		err := r.Scan(&l.date, &l.end, &l.currency, &l.item, &l.amount, &l.payment, &kind, &id)
		if id != nil {
			l.doc = &Document{Kind: *kind, ID: *id}
		}
		return l, errs.Wrapf(err, "scan cash snapshot line")
	})
	if err != nil {
		return nil, fmt.Errorf("cash: lines of snapshot %s: %w", snapshot, err)
	}
	return lines, nil
}
