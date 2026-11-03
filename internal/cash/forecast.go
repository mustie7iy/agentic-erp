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

// Payable is what a run is expected to pay a supplier as the forecast
// reads it (ADR-0051 §4, §5): a supplier open item (SourcePayable), an
// invoice not posted yet as the item it would become
// (SourceInvoiceUnposted), or a down payment request (SourceDownPayment);
// what paying it on a day pays, the days a run would pay it on, whether
// its supplier is blocked, and the bank and method that would pay it.
type Payable struct {
	Source   string
	Supplier uuid.UUID // the business partner a run pays
	Currency string
	// Due is its net due date; a down payment request's due date.
	Due time.Time
	// Blocked: no run pays its supplier (blocked for payment or posting,
	// inactive, not set up in the company): shown apart on its due date
	// (SourcePayableBlocked), out of the totals unless asked.
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
	// Lines are its document's offsetting lines and PlanningGroup its
	// supplier's planning group in the company, "" for none: what its
	// liquidity items are derived from (ADR-0051 §4, T53e3). A down
	// payment request has no lines.
	Lines         []DocumentLine
	PlanningGroup string
}

// Paying is what a payment of a run not yet released pays (ADR-0051 §4,
// T53e3): its supplier's planning group in the company, and what it pays
// of each open item it settles with the item's offsetting lines; a down
// payment request's payment settles none and is classified as a whole.
type Paying struct {
	PlanningGroup string
	Items         []Paid
}

// Paid is an open item a payment settles: what the payment pays of it
// (negative for a credit memo it nets) and its offsetting lines.
type Paid struct {
	Amount money.Minor
	Lines  []DocumentLine
}

// Sources and certainties of the forecast's flows (ADR-0051 §4).
const (
	SourcePayable         = "payable"
	SourcePayableBlocked  = "payable_blocked"
	SourceInvoiceUnposted = "invoice_unposted"
	SourceDownPayment     = "down_payment"
	CertaintyActual       = "actual"
	CertaintyExpected     = "expected"
	CertaintyPlanned      = "planned"
)

// certainties are the certainty levels, most certain first: a cut at one
// keeps it and those before it.
var certainties = []string{CertaintyActual, CertaintyConfirmed, CertaintyExpected, CertaintyPlanned}

// ForecastQuery is what a forecast covers: the legal entity, the least
// certain flows it counts ("": every flow, planned), and whether what
// blocked suppliers are owed counts in the totals.
type ForecastQuery struct {
	LegalEntity    uuid.UUID
	MinCertainty   string
	IncludeBlocked bool
}

// Forecast is a legal entity's liquidity forecast.
type Forecast struct {
	LegalEntity    uuid.UUID
	ReadDate       time.Time // the company's date at the read, in TimeZone
	TimeZone       string
	MinCertainty   string
	IncludeBlocked bool
	Buckets        []Bucket
	Currencies     []ForecastCurrency
}

// Bucket is a column of the forecast: the days from Start to End.
type Bucket struct {
	Grain      string
	Start, End time.Time
}

// ForecastCurrency is the flows of one currency per bucket, in the
// buckets' order, their total over the horizon, the flows expected after
// its last bucket, and the blocked suppliers' flows in the buckets and
// after, counted in the totals and Beyond only when asked.
type ForecastCurrency struct {
	Currency string
	Exponent int
	Buckets  []BucketFlows
	Total    money.Minor
	Beyond   Beyond
	Blocked  Beyond
	// Items are its rows by liquidity item and its hierarchy (T53e3).
	Items []ItemRow
}

// BucketFlows is a bucket's flows by source, certainty and lateness, and
// their total.
type BucketFlows struct {
	Flows []FlowTotal
	Total money.Minor
}

// Beyond is a sum of flows and their count: what is expected after the
// horizon (counted in no bucket), or what blocked suppliers are owed.
type Beyond struct {
	Amount money.Minor
	Count  int
}

// datedFlow is a flow in its currency, with the documents it pays, from
// which its liquidity items are derived, and its shares by item.
type datedFlow struct {
	flow
	currency string
	parts    []part
	shares   []Share
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
	f := Forecast{LegalEntity: q.LegalEntity, ReadDate: read, TimeZone: zone, MinCertainty: minCertainty, IncludeBlocked: q.IncludeBlocked,
		Buckets: buckets(read, r.settings.BucketsOrDefault())}
	flows, err := e.forecastFlows(ctx, r, q.LegalEntity)
	if err != nil {
		return Forecast{}, err
	}
	flows = slices.DeleteFunc(flows, func(d datedFlow) bool { return slices.Index(certainties, d.certainty) > keep })
	for i := range flows {
		if flows[i].shares, err = classify(doc.Cash.Derivation, r.sums, flows[i].amount, flows[i].parts); err != nil {
			return Forecast{}, err
		}
	}
	if f.Currencies, err = r.byCurrency(ctx, tx, f.Buckets, flows, q.IncludeBlocked); err != nil {
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

// forecastFlows are the legal entity's supplier payments on their
// expected days (supplierFlows: payable, invoice_unposted, down_payment,
// and payable_blocked apart) and the payments of runs not yet released
// (payment_run, confirmed), whose items and requests the payables port
// leaves out: never both.
func (e *Endpoints) forecastFlows(ctx context.Context, r reader, legalEntity uuid.UUID) ([]datedFlow, error) {
	payables, err := e.Payments.Payables(ctx, legalEntity, r.read)
	if err != nil {
		return nil, fmt.Errorf("cash: supplier open items of %s: %w", legalEntity, err)
	}
	out, err := r.supplierFlows(payables)
	if err != nil {
		return nil, fmt.Errorf("cash: supplier open items of %s: %w", legalEntity, err)
	}
	pending, err := e.Payments.Pending(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: payments of runs not released of %s: %w", legalEntity, err)
	}
	bankIDs := make([]uuid.UUID, len(pending))
	for i, p := range pending {
		bankIDs[i] = p.HouseBankID
	}
	banks, err := e.Payments.HouseBankCodes(ctx, bankIDs)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	paying, err := e.Payments.Paying(ctx, legalEntity, ids(pending), r.read)
	if err != nil {
		return nil, fmt.Errorf("cash: what the payments of runs not released of %s pay: %w", legalEntity, err)
	}
	for _, p := range pending {
		d, late := r.valueDate(banks[p.HouseBankID], p)
		out = append(out, datedFlow{flow: flow{date: d, source: SourcePaymentRun, certainty: CertaintyConfirmed, late: late, amount: -p.Amount}, currency: p.Currency,
			parts: paymentParts(p, paying[p.ID])})
	}
	return out, nil
}

// ids are the payments' ids.
func ids(ps []Payment) []uuid.UUID {
	out := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// paymentParts are the documents a payment pays: each open item it
// settles at what it pays of it, which add up to the payment (a run pays
// a supplier the sum of its items, netted); a payment of no item (a down
// payment request's) whole, by its supplier's planning group.
func paymentParts(p Payment, paying Paying) []part {
	if len(paying.Items) == 0 {
		return []part{{amount: -p.Amount, planningGroup: paying.PlanningGroup}}
	}
	out := make([]part, len(paying.Items))
	for i, it := range paying.Items {
		out[i] = part{amount: -it.Amount, lines: it.Lines, planningGroup: paying.PlanningGroup}
	}
	return out
}

// netting is what one run payment pays (ADR-0051 §5): a supplier's
// documents of one source and certainty in one currency, which a run nets
// into one payment; a down payment request alone (request > 0), which a
// run pays whole and never nets.
type netting struct {
	supplier                    uuid.UUID
	currency, source, certainty string
	request                     int
}

// planned is a document on the day a run is expected to pay it.
type planned struct {
	day time.Time
	p   Payable
}

// supplierFlows are the payments the runs on the suppliers' expected
// payment days make, as a run nets them (ADR-0051 §5): per supplier,
// currency, source and certainty, each document on its day (expected),
// and on each day what the documents due then and those carried from
// before pay, each at what it pays on that day. A day whose documents owe
// nothing net (a credit memo, or credits above the invoices) pays
// nothing, as the proposer excludes a supplier with a debit balance
// (DebitBalance), and they are carried to the supplier's next day; what
// is still a debit balance after the supplier's last day is no cash flow
// and is left out. A down payment request is paid alone. A blocked
// supplier's documents are netted the same way on their due dates, as
// payable_blocked.
func (r reader) supplierFlows(payables []Payable) ([]datedFlow, error) {
	groups := map[netting][]planned{}
	var order []netting
	for i, p := range payables {
		k, day := r.expected(p)
		if p.Source == SourceDownPayment {
			k.request = i + 1
		}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], planned{day, p})
	}
	var out []datedFlow
	for _, k := range order {
		flows, err := r.netted(k, groups[k])
		if err != nil {
			return nil, err
		}
		out = append(out, flows...)
	}
	return out, nil
}

// expected is the run payment a document is in, its source and certainty,
// and the day a run is expected to pay it: a blocked supplier's on its due
// date (the read date when overdue), a down payment request on its due
// date, any other on its payment day (payDay).
func (r reader) expected(p Payable) (netting, time.Time) {
	k := netting{supplier: p.Supplier, currency: p.Currency, source: p.Source, certainty: CertaintyExpected}
	if p.Source == SourceInvoiceUnposted {
		k.certainty = CertaintyPlanned
	}
	switch {
	case p.Blocked:
		k.source = SourcePayableBlocked
		if p.Due.Before(r.read) {
			return k, r.read
		}
		return k, p.Due
	case p.Source == SourceDownPayment:
		return k, p.Due
	}
	return k, r.payDay(p)
}

// netted are the flows of one run payment's documents (netting): on each
// of their days what those due then and those carried pay, when it is
// more than nothing, at the bank on its day (atBankOn).
func (r reader) netted(k netting, items []planned) ([]datedFlow, error) {
	slices.SortStableFunc(items, func(a, b planned) int { return a.day.Compare(b.day) })
	var out []datedFlow
	var carried []Payable
	for i, it := range items {
		carried = append(carried, it.p)
		if i+1 < len(items) && items[i+1].day.Equal(it.day) {
			continue
		}
		paid, parts, err := r.pays(carried, it.day)
		if err != nil {
			return nil, err
		}
		if paid <= 0 {
			continue
		}
		at, late := r.atBankOn(k.source, it)
		out = append(out, datedFlow{flow: flow{date: at, source: k.source, certainty: k.certainty, late: late, amount: -paid}, currency: k.currency,
			parts: parts})
		carried = nil
	}
	return out, nil
}

// atBankOn is the day a run payment is at the bank: a blocked supplier's
// on its day as it is (no run pays it); a down payment request's on the
// working day on or after its due date, late when that is before the read
// date (atBank); any other its payment day plus the value days of the bank
// that would pay it, on the working day on or after.
func (r reader) atBankOn(source string, it planned) (time.Time, bool) {
	switch source {
	case SourcePayableBlocked:
		return it.day, false
	case SourceDownPayment:
		return r.atBank(it.day)
	}
	return r.onOrAfter(it.day.AddDate(0, 0, r.settings.ValueDaysFor(it.p.BankCode, it.p.Method))), false
}

// pays is what a run on the day pays for the items: each at what it pays
// then, summed, with each item as a part of the payment (negative: paid
// out) to derive its liquidity items from.
func (r reader) pays(items []Payable, day time.Time) (money.Minor, []part, error) {
	var total money.Minor
	parts := make([]part, len(items))
	for i, p := range items {
		amount, err := p.Pays(day)
		if err != nil {
			return 0, nil, err
		}
		total = r.sums.add(total, amount)
		parts[i] = part{amount: -amount, lines: p.Lines, planningGroup: p.PlanningGroup}
	}
	return total, parts, nil
}

// payDay is the day a run is expected to pay a supplier item (ADR-0051 §5,
// P5): the last day of its earliest discount tier still open while the
// company takes discounts, else its net due date; moved back to the
// company's last payment day on or before it, which a tier must not put
// before the read date (the next tier, or the due date, is then tried); an
// item overdue, or due before the next payment day, is expected on the
// next payment day from the read date. Its day at the bank adds the value
// days of the bank that would pay it, moved to the next working day of the
// company's calendar (supplierFlows).
func (r reader) payDay(p Payable) time.Time {
	dates := p.Dates
	if !r.settings.TakesDiscounts() {
		dates = dates[len(dates)-1:]
	}
	for _, d := range dates {
		if day := r.lastPaymentDay(d); !day.Before(r.read) {
			return day
		}
	}
	return r.nextPaymentDay(r.read)
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
// buckets their days fall in; a flow after the last bucket is beyond. The
// blocked suppliers' flows are listed in the buckets and summed in
// Blocked, and counted in the totals and Beyond only with includeBlocked.
func (r reader) byCurrency(ctx context.Context, tx pgx.Tx, bs []Bucket, flows []datedFlow, includeBlocked bool) ([]ForecastCurrency, error) {
	var out []ForecastCurrency
	at := map[string]int{}
	var rows []*itemRows
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
			rows = append(rows, newItemRows(r.doc.Cash.LiquidityItems, len(bs)))
		}
		r.into(&out[i], rows[i], bs, f, includeBlocked)
	}
	for i := range out {
		out[i].Items = rows[i].ordered()
	}
	slices.SortFunc(out, func(a, b ForecastCurrency) int { return cmp.Compare(a.Currency, b.Currency) })
	return out, nil
}

// into adds a flow to its currency: to the bucket its day falls in, else
// beyond, and to the totals, unless it is a blocked supplier's not asked
// for, which is listed in its bucket and summed in Blocked only; and its
// shares to the currency's rows by liquidity item alike.
func (r reader) into(c *ForecastCurrency, rows *itemRows, bs []Bucket, f datedFlow, includeBlocked bool) {
	blocked := f.source == SourcePayableBlocked
	if blocked {
		c.Blocked = r.sums.addTo(c.Blocked, f.amount)
	}
	counted := includeBlocked || !blocked
	// The first bucket ending on or after the day, found or not: its start
	// is on or before the day, as no flow is before the read date.
	b, _ := slices.BinarySearchFunc(bs, f.date, func(b Bucket, d time.Time) int {
		return cmp.Compare(b.End.Unix(), d.Unix())
	})
	for _, sh := range f.shares {
		rows.add(r.sums, sh, f, b, counted)
	}
	switch {
	case b < len(bs):
		bf := &c.Buckets[b]
		bf.Flows = r.sums.addTotal(bf.Flows, FlowTotal{Source: f.source, Certainty: f.certainty, Late: f.late, Amount: f.amount, Count: 1})
		if counted {
			bf.Total, c.Total = r.sums.add(bf.Total, f.amount), r.sums.add(c.Total, f.amount)
		}
	case counted:
		c.Beyond = r.sums.addTo(c.Beyond, f.amount)
	}
}

// addTo adds a flow's amount to a sum and counts it.
func (s *sums) addTo(b Beyond, amount money.Minor) Beyond {
	return Beyond{Amount: s.add(b.Amount, amount), Count: b.Count + 1}
}
