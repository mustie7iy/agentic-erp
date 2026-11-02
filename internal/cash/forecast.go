package cash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The liquidity forecast (ADR-0051 §9, T53e) is computed at the read, as
// the cash position is, from the supplier open items a run will pay and
// the payments of runs not yet released, in the company's buckets per
// currency. A supplier item is expected when the payment run would pay it
// (§5, P5): the payment proposer's due-date and discount rules are the
// ledger's (ledger.ItemTerms), read through the payments context's port,
// so the two never differ.

// Payable is a supplier open item as the forecast reads it (ADR-0051 §5):
// what paying it on a day pays, the days a run would pay it on, whether
// its supplier is blocked, and the bank and method that would pay it.
type Payable struct {
	Currency string
	// Blocked: no run pays its supplier (blocked for payment or posting,
	// inactive): left out of the totals.
	Blocked bool
	// BankCode and Method are the house bank its supplier's first payment
	// method ranks for its currency and that method; "" when none fits,
	// which has no value days.
	BankCode, Method string
	// Dates are the days a run would pay it on, in the order the forecast
	// tries them: the last day of each cash discount tier of an invoice,
	// earliest first, then its net due date (a credit memo's is its
	// document date, so it nets with the next run).
	Dates []time.Time
	// Pays is what a payment on the day pays the supplier, after the cash
	// discount it takes then and the withholding tax deducted: positive
	// for an invoice, negative for a credit memo it nets.
	Pays func(on time.Time) (money.Minor, error)
}

// Sources and certainties of the forecast's flows (ADR-0051 §4).
const (
	SourcePayable     = "payable"
	CertaintyActual   = "actual"
	CertaintyExpected = "expected"
	CertaintyPlanned  = "planned"
)

// certainties are the certainty levels, most certain first: a cut at one
// keeps it and those before it.
var certainties = []string{CertaintyActual, CertaintyConfirmed, CertaintyExpected, CertaintyPlanned}

// ForecastQuery is what a forecast covers: the legal entity and the least
// certain flows it counts ("": every flow, planned).
type ForecastQuery struct {
	LegalEntity  uuid.UUID
	MinCertainty string
}

// Forecast is a legal entity's liquidity forecast.
type Forecast struct {
	LegalEntity  uuid.UUID
	ReadDate     time.Time // the company's date at the read, in TimeZone
	TimeZone     string
	MinCertainty string
	Buckets      []Bucket
	Currencies   []ForecastCurrency
}

// Bucket is a column of the forecast: the days from Start to End.
type Bucket struct {
	Grain      string
	Start, End time.Time
}

// ForecastCurrency is the flows of one currency per bucket, in the
// buckets' order, their total over the horizon, and the flows expected
// after its last bucket.
type ForecastCurrency struct {
	Currency string
	Exponent int
	Buckets  []BucketFlows
	Total    money.Minor
	Beyond   Beyond
}

// BucketFlows is a bucket's flows by source, certainty and lateness, and
// their total.
type BucketFlows struct {
	Flows []FlowTotal
	Total money.Minor
}

// Beyond is what is expected after the horizon: counted in no bucket.
type Beyond struct {
	Amount money.Minor
	Count  int
}

// datedFlow is a flow in its currency.
type datedFlow struct {
	flow
	currency string
}

// Forecast computes the legal entity's liquidity forecast. A legal entity
// the caller cannot act in is not found, as one that does not exist.
func (e *Endpoints) Forecast(ctx context.Context, q ForecastQuery) (Forecast, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Forecast{}, err
	}
	co, err := e.company(ctx, q.LegalEntity)
	if err != nil {
		return Forecast{}, err
	}
	minCertainty := cmp.Or(q.MinCertainty, CertaintyPlanned)
	keep := slices.Index(certainties, minCertainty)
	if keep < 0 {
		return Forecast{}, problem.Unprocessable("request.validation_failed", "The least certain flows counted must be a certainty level.",
			problem.FieldError{Field: "min_certainty", Code: "invalid", Message: "actual, confirmed, expected or planned"})
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Forecast{}, fmt.Errorf("cash: configuration: %w", err)
	}
	read, zone, err := e.readDate(doc, co.Code)
	if err != nil {
		return Forecast{}, err
	}
	r := reader{doc: doc, company: co.Code, settings: companyCash(doc, co.Code), read: read, sums: &sums{}}
	f := Forecast{LegalEntity: q.LegalEntity, ReadDate: read, TimeZone: zone, MinCertainty: minCertainty,
		Buckets: buckets(read, r.settings.BucketsOrDefault())}
	flows, err := e.forecastFlows(ctx, r, q.LegalEntity)
	if err != nil {
		return Forecast{}, err
	}
	flows = slices.DeleteFunc(flows, func(d datedFlow) bool { return slices.Index(certainties, d.certainty) > keep })
	if f.Currencies, err = r.byCurrency(ctx, tx, f.Buckets, flows); err != nil {
		return Forecast{}, err
	}
	if r.sums.overflow {
		return Forecast{}, errForecastTooLarge
	}
	return f, nil
}

// errForecastTooLarge refuses a forecast whose sums are out of the range
// of an amount.
var errForecastTooLarge = problem.Unprocessable("cash.amount_too_large", "The amounts of the liquidity forecast add up to more than an amount can hold.")

// forecastFlows are the legal entity's supplier open items on their
// expected days (source payable, expected) and the payments of runs not
// yet released (payment_run, confirmed), whose items the payables port
// leaves out: never both.
func (e *Endpoints) forecastFlows(ctx context.Context, r reader, legalEntity uuid.UUID) ([]datedFlow, error) {
	payables, err := e.Payments.Payables(ctx, legalEntity, r.read)
	if err != nil {
		return nil, fmt.Errorf("cash: supplier open items of %s: %w", legalEntity, err)
	}
	var out []datedFlow
	for _, p := range payables {
		if p.Blocked {
			continue
		}
		day, amount, err := r.expected(p)
		if err != nil {
			return nil, fmt.Errorf("cash: a supplier open item of %s: %w", legalEntity, err)
		}
		out = append(out, datedFlow{flow: flow{date: day, source: SourcePayable, certainty: CertaintyExpected, amount: -amount}, currency: p.Currency})
	}
	pending, err := e.Payments.Pending(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: payments of runs not released of %s: %w", legalEntity, err)
	}
	ids := make([]uuid.UUID, len(pending))
	for i, p := range pending {
		ids[i] = p.HouseBankID
	}
	banks, err := e.Payments.HouseBankCodes(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	for _, p := range pending {
		d, late := r.valueDate(banks[p.HouseBankID], p)
		out = append(out, datedFlow{flow: flow{date: d, source: SourcePaymentRun, certainty: CertaintyConfirmed, late: late, amount: -p.Amount}, currency: p.Currency})
	}
	return out, nil
}

// expected is the day a supplier item is expected at the bank, and what it
// pays then (ADR-0051 §5, P5): the last day of its earliest discount tier
// still open while the company takes discounts, else its net due date;
// moved back to the company's last payment day on or before it, which a
// tier must not put before the read date (the next tier, or the due date,
// is then tried); an item overdue, or due before the next payment day, is
// expected on the next payment day from the read date; then the value
// days of the bank that would pay it, moved to the next working day of the
// company's calendar.
func (r reader) expected(p Payable) (time.Time, money.Minor, error) {
	dates := p.Dates
	if !r.settings.TakesDiscounts() {
		dates = dates[len(dates)-1:]
	}
	pay := r.nextPaymentDay(r.read)
	for _, d := range dates {
		if day := r.lastPaymentDay(d); !day.Before(r.read) {
			pay = day
			break
		}
	}
	amount, err := p.Pays(pay)
	if err != nil {
		return time.Time{}, 0, err
	}
	return r.onOrAfter(pay.AddDate(0, 0, r.settings.ValueDaysFor(p.BankCode, p.Method))), amount, nil
}

// paymentDay reports whether the company runs payments on the day: any
// day when it names none.
func (r reader) paymentDay(d time.Time) bool {
	if len(r.settings.PaymentDays) == 0 {
		return true
	}
	return slices.Contains(r.settings.PaymentDays, strings.ToLower(d.Weekday().String()[:3]))
}

// lastPaymentDay is the last payment day on or before d.
func (r reader) lastPaymentDay(d time.Time) time.Time {
	for i := 0; i < 7 && !r.paymentDay(d); i++ {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// nextPaymentDay is the first payment day on or after d.
func (r reader) nextPaymentDay(d time.Time) time.Time {
	for i := 0; i < 7 && !r.paymentDay(d); i++ {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// buckets are the forecast's columns from the read date: each grain's
// periods after the previous grain's, a day, seven days, or a month (to
// the same day of the next month, its last day when shorter).
func buckets(read time.Time, bs []config.Bucket) []Bucket {
	var out []Bucket
	start := read
	for _, b := range bs {
		from := start
		for k := 1; k <= b.Count; k++ {
			var next time.Time
			switch b.Grain {
			case config.GrainWeek:
				next = from.AddDate(0, 0, 7*k)
			case config.GrainMonth:
				next = addMonths(from, k)
			default:
				next = from.AddDate(0, 0, k)
			}
			out = append(out, Bucket{Grain: b.Grain, Start: start, End: next.AddDate(0, 0, -1)})
			start = next
		}
	}
	return out
}

// byCurrency sums the flows per currency, by currency code, into the
// buckets their days fall in; a flow after the last bucket is beyond.
func (r reader) byCurrency(ctx context.Context, tx pgx.Tx, bs []Bucket, flows []datedFlow) ([]ForecastCurrency, error) {
	var out []ForecastCurrency
	at := map[string]int{}
	for _, f := range flows {
		i, ok := at[f.currency]
		if !ok {
			exp, err := currency.Exponent(ctx, tx, f.currency, fmt.Errorf("cash: currency %s of a forecast flow is not known", f.currency))
			if err != nil {
				return nil, err
			}
			i, at[f.currency] = len(out), len(out)
			out = append(out, ForecastCurrency{Currency: f.currency, Exponent: exp, Buckets: make([]BucketFlows, len(bs))})
			for b := range out[i].Buckets {
				out[i].Buckets[b].Flows = []FlowTotal{}
			}
		}
		c := &out[i]
		b, found := slices.BinarySearchFunc(bs, f.date, func(b Bucket, d time.Time) int {
			return cmp.Compare(b.End.Unix(), d.Unix())
		})
		if !found && b == len(bs) {
			c.Beyond = Beyond{Amount: r.sums.add(c.Beyond.Amount, f.amount), Count: c.Beyond.Count + 1}
			continue
		}
		bf := &c.Buckets[b]
		bf.Flows = r.sums.addTotal(bf.Flows, FlowTotal{Source: f.source, Certainty: f.certainty, Late: f.late, Amount: f.amount, Count: 1})
		bf.Total, c.Total = r.sums.add(bf.Total, f.amount), r.sums.add(c.Total, f.amount)
	}
	slices.SortFunc(out, func(a, b ForecastCurrency) int { return cmp.Compare(a.Currency, b.Currency) })
	return out, nil
}
