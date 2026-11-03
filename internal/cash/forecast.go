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
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
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
	// Document is the document it is: a posted item's journal entry, an
	// unposted invoice, or a down payment request (T53e4).
	Document Document
}

// Document is a document a forecast flow pays, as the drill-down names it
// (ADR-0051 §9, T53e4): its kind (DocJournalEntry, DocSupplierInvoice,
// DocDownPaymentRequest, DocPaymentRun or DocMemoRecord), its id in its
// own context and its number, "" when it has none.
type Document struct {
	Kind   string
	ID     uuid.UUID
	Number string
}

// Kinds of document a forecast flow pays.
const (
	DocJournalEntry       = "journal_entry"
	DocSupplierInvoice    = "supplier_invoice"
	DocDownPaymentRequest = "down_payment_request"
	DocPaymentRun         = "payment_run"
	DocMemoRecord         = "memo_record"
)

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
// certain flows it counts ("": every flow, planned), whether what blocked
// suppliers are owed counts in the totals, one grain over a range (Grain
// with To; "" and nil: the company's buckets), and the currency the totals
// are converted into ("": none, T53e4).
type ForecastQuery struct {
	LegalEntity    uuid.UUID
	MinCertainty   string
	IncludeBlocked bool
	Grain          string
	To             *time.Time
	Display        string
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
	// Converted are the currencies in the display currency, nil when none
	// was asked for (ADR-0051 §10, T53e4).
	Converted *ForecastConverted
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
	// Opening is what the read date opens at: the cash position's opening
	// of the first day (T53e4).
	Opening ForecastOpening
	Buckets []BucketFlows
	Total   money.Minor
	Beyond  Beyond
	Blocked Beyond
	// Items are its rows by liquidity item and its hierarchy (T53e3).
	Items []ItemRow
}

// BucketFlows is a bucket's flows by source, certainty and lateness,
// their total, and the balance after them: the opening plus the totals so
// far (T53e4).
type BucketFlows struct {
	Flows   []FlowTotal
	Total   money.Minor
	Closing money.Minor
}

// ForecastOpening is a currency's opening: the sum of its active house
// banks' latest statement closing balances before the read date, how many
// banks it sums, and whether one is stale or in gap, as the cash position
// marks them.
type ForecastOpening struct {
	Amount     money.Minor
	Banks      int
	Stale, Gap bool
}

// Beyond is a sum of flows and their count: what is expected after the
// horizon (counted in no bucket), or what blocked suppliers are owed.
type Beyond struct {
	Amount money.Minor
	Count  int
}

// datedFlow is a flow in its currency, with the documents it pays, from
// which its liquidity items are derived, and its shares by item; the
// supplier it pays (nil for a memo record) and the documents it pays with
// what it pays of each, which the drill-down names (T53e4).
type datedFlow struct {
	flow
	currency string
	parts    []part
	shares   []Share
	// partShares are each part's shares (after the parts are scaled to the
	// flow), which shares sums: what a snapshot keeps per document (T53f).
	partShares [][]Share
	supplier   *uuid.UUID
	documents  []paidDocument
	// identity tells a flow from others of its day, source and documents
	// (a run's payments, a payment's lines in transit, a series'
	// occurrences) in the drill-down's keys; "" when its documents do.
	identity string
	// bank is the house bank a payment or memo record is on, bankCode the
	// one a run would pay a supplier item from (by code), payment the
	// run's payment or the payment in transit: what a snapshot keeps of a
	// flow beside its documents (T53f). nil and "" for none.
	bank     *uuid.UUID
	bankCode string
	payment  *uuid.UUID
}

// paidDocument is a document a flow pays and what the flow pays of it.
type paidDocument struct {
	Document
	amount money.Minor
}

// Forecast computes the legal entity's liquidity forecast. A legal entity
// the caller cannot act in is not found, as one that does not exist.
func (e *Endpoints) Forecast(ctx context.Context, q ForecastQuery) (Forecast, error) {
	var bs []Bucket
	fr, err := e.readForecast(ctx, q.LegalEntity, q.MinCertainty, func(r reader) (time.Time, error) {
		var err error
		bs, err = forecastBuckets(r, q.Grain, q.To)
		if err != nil {
			return time.Time{}, err
		}
		return bs[len(bs)-1].End, nil
	})
	if err != nil {
		return Forecast{}, err
	}
	f := Forecast{LegalEntity: q.LegalEntity, ReadDate: fr.r.read, TimeZone: fr.zone, MinCertainty: fr.minCertainty, IncludeBlocked: q.IncludeBlocked,
		Buckets: bs}
	if f.Currencies, err = fr.r.byCurrency(ctx, fr.tx, bs, fr.flows, fr.banks, q.IncludeBlocked); err != nil {
		return Forecast{}, err
	}
	if q.Display != "" {
		c, err := e.convertForecast(ctx, fr.tx, fr.r, f.Currencies, q.Display, len(bs))
		if err != nil {
			return Forecast{}, err
		}
		f.Converted = &c
	}
	if fr.r.sums.overflow {
		return Forecast{}, errForecastTooLarge
	}
	return f, nil
}

// forecastRead is a forecast's flows as read, before they are summed: the
// request's transaction, the company's reader, its zone, the certainty
// cut, the flows from the read date, each classified by liquidity item,
// and the company's active house banks, whose statements open it.
type forecastRead struct {
	tx           pgx.Tx
	r            reader
	zone         string
	minCertainty string
	flows        []datedFlow
	banks        []Bank
	// version is the configuration's whose derivation classified the
	// flows (T53f: a snapshot records it).
	version int32
}

// readForecast reads the legal entity's forecast flows up to the horizon
// last gives for the company's reader (the last day a memo series is
// expanded to), the less certain than minCertainty cut, and classifies
// them by the active derivation: what the forecast sums and the drill-down
// lists alike.
func (e *Endpoints) readForecast(ctx context.Context, legalEntity uuid.UUID, minCertainty string, last func(reader) (time.Time, error)) (forecastRead, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return forecastRead{}, err
	}
	co, err := e.company(ctx, legalEntity)
	if err != nil {
		return forecastRead{}, err
	}
	return e.readForecastOf(ctx, tx, co, legalEntity, minCertainty, last)
}

// readForecastOf reads, in tx, the forecast flows of a legal entity whose
// company is known, the caller's access to it judged before (readForecast)
// or not at all (the snapshot workflow acts in every company, T53f).
func (e *Endpoints) readForecastOf(ctx context.Context, tx pgx.Tx, co Company, legalEntity uuid.UUID, minCertainty string,
	last func(reader) (time.Time, error)) (forecastRead, error) {
	var err error
	fr := forecastRead{tx: tx, minCertainty: cmp.Or(minCertainty, CertaintyPlanned)}
	keep := slices.Index(certainties, fr.minCertainty)
	if keep < 0 {
		return forecastRead{}, problem.Unprocessable("request.validation_failed", "The least certain flows counted must be a certainty level.",
			problem.FieldError{Field: "min_certainty", Code: "invalid", Message: "actual, confirmed, expected or planned"})
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return forecastRead{}, fmt.Errorf("cash: configuration: %w", err)
	}
	fr.version = version
	read, zone, err := e.readDate(doc, co.Code)
	if err != nil {
		return forecastRead{}, err
	}
	fr.zone, fr.r = zone, reader{doc: doc, company: co.Code, settings: companyCash(doc, co.Code), read: read, sums: &sums{}}
	horizon, err := last(fr.r)
	if err != nil {
		return forecastRead{}, err
	}
	if fr.flows, fr.banks, err = e.forecastFlows(ctx, tx, fr.r, legalEntity, horizon); err != nil {
		return forecastRead{}, err
	}
	fr.flows = slices.DeleteFunc(fr.flows, func(d datedFlow) bool { return slices.Index(certainties, d.certainty) > keep })
	d := newRules(doc.Cash.Derivation)
	for i := range fr.flows {
		f := &fr.flows[i]
		if f.partShares, err = partsShares(d, f.amount, f.parts); err != nil {
			return forecastRead{}, err
		}
		f.shares = sumShares(fr.r.sums, f.partShares)
	}
	return fr, nil
}

// forecastBuckets are the forecast's columns: the company's buckets, or,
// asked for one grain over a range, the periods of the grain from the read
// date up to the one holding the last day, within the grain's limit of the
// company's buckets (config.MaxPeriods). One without the other, a grain
// not known, a last day before the read date or more periods are refused.
func forecastBuckets(r reader, grain string, to *time.Time) ([]Bucket, error) {
	if grain == "" && to == nil {
		return buckets(r.read, r.settings.BucketsOrDefault()), nil
	}
	invalid := func(field, message string) error {
		return problem.Unprocessable("request.validation_failed", "One grain over a range takes a grain and the last day it reaches.",
			problem.FieldError{Field: field, Code: "invalid", Message: message})
	}
	limit, known := config.MaxPeriods(grain)
	switch {
	case !known:
		return nil, invalid("grain", "day, week or month, with to")
	case to == nil:
		return nil, invalid("to", "the last day, with grain")
	case clock.Day(*to).Before(r.read):
		return nil, invalid("to", "on or after "+r.read.Format(time.DateOnly))
	}
	bs := buckets(r.read, []config.Bucket{{Grain: grain, Count: limit}})
	n := slices.IndexFunc(bs, func(b Bucket) bool { return !b.End.Before(clock.Day(*to)) })
	if n < 0 {
		return nil, invalid("to", fmt.Sprintf("at most %d periods of a %s from %s", limit, grain, r.read.Format(time.DateOnly)))
	}
	return bs[:n+1], nil
}

// errForecastTooLarge refuses a forecast whose sums are out of the range
// of an amount.
var errForecastTooLarge = problem.Unprocessable("cash.amount_too_large", "The amounts of the liquidity forecast add up to more than an amount can hold.")

// forecastFlows are the legal entity's supplier payments on their
// expected days (supplierFlows: payable, invoice_unposted, down_payment,
// and payable_blocked apart), the payments of runs not yet released
// (payment_run, confirmed), whose items and requests the payables port
// leaves out (never both), the payments in transit from its active house
// banks (in_transit, confirmed) and its memo records' occurrences up to the
// last day (memo, T53e4); and those banks, whose statements open the
// forecast.
func (e *Endpoints) forecastFlows(ctx context.Context, tx pgx.Tx, r reader, legalEntity uuid.UUID, last time.Time) ([]datedFlow, []Bank, error) {
	payables, err := e.Payments.Payables(ctx, legalEntity, r.read)
	if err != nil {
		return nil, nil, fmt.Errorf("cash: supplier open items of %s: %w", legalEntity, err)
	}
	out, err := r.supplierFlows(payables)
	if err != nil {
		return nil, nil, fmt.Errorf("cash: supplier open items of %s: %w", legalEntity, err)
	}
	banks, err := e.Payments.Banks(ctx, legalEntity, r.read)
	if err != nil {
		return nil, nil, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	payments, err := e.paymentFlows(ctx, r, legalEntity, banks)
	if err != nil {
		return nil, nil, err
	}
	memos, err := memoForecast(ctx, tx, r, legalEntity, last)
	if err != nil {
		return nil, nil, err
	}
	return append(append(out, payments...), memos...), banks, nil
}

// paymentFlows are the payments of runs not yet released, on the run
// date plus the paying bank's value days, and the payments in transit from
// the banks, each line on its bank's clearing account on its day at the
// bank as the cash position has it; each classified by the open items it
// pays (paymentParts).
func (e *Endpoints) paymentFlows(ctx context.Context, r reader, legalEntity uuid.UUID, banks []Bank) ([]datedFlow, error) {
	pending, err := e.Payments.Pending(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: payments of runs not released of %s: %w", legalEntity, err)
	}
	transit, err := e.inTransit(ctx, r, legalEntity, banks)
	if err != nil {
		return nil, err
	}
	bankIDs, payIDs := make([]uuid.UUID, len(pending)), ids(pending)
	for i, p := range pending {
		bankIDs[i] = p.HouseBankID
	}
	for _, t := range transit {
		payIDs = append(payIDs, t.payment.ID)
	}
	codes, err := e.Payments.HouseBankCodes(ctx, bankIDs)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	paying, err := e.Payments.Paying(ctx, legalEntity, payIDs, r.read)
	if err != nil {
		return nil, fmt.Errorf("cash: what the payments of runs not released of %s pay: %w", legalEntity, err)
	}
	var out []datedFlow
	for _, p := range pending {
		d, late := r.valueDate(codes[p.HouseBankID], p)
		out = append(out, paymentFlow(flow{date: d, source: SourcePaymentRun, certainty: CertaintyConfirmed, late: late, amount: -p.Amount}, p, paying[p.ID],
			p.ID.String()))
	}
	for _, t := range transit {
		for i, f := range t.flows {
			out = append(out, paymentFlow(f, t.payment, paying[t.payment.ID], fmt.Sprintf("%s#%d", t.payment.ID, i)))
		}
	}
	return out, nil
}

// paymentFlow is a payment's flow in its currency, classified by what it
// pays, its supplier and its run as the document it pays; identity tells
// it from the run's other payments (and a payment's other lines in
// transit) in the drill-down's keys.
func paymentFlow(f flow, p Payment, paying Paying, identity string) datedFlow {
	return datedFlow{flow: f, currency: p.Currency, parts: paymentParts(f.amount, paying), supplier: &p.SupplierID, identity: identity,
		documents: []paidDocument{{Document: Document{Kind: DocPaymentRun, ID: p.RunID, Number: p.RunNumber}, amount: f.amount}},
		bank:      &p.HouseBankID, payment: &p.ID}
}

// ids are the payments' ids.
func ids(ps []Payment) []uuid.UUID {
	out := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// paymentParts are the documents a payment of the amount (negative: paid
// out) pays: each open item it settles at what it pays of it, which add up
// to the payment (a run pays a supplier the sum of its items, netted), and
// classify shares a line in transit of part of it out over them; a payment
// of no item (a down payment request's) whole, by its supplier's planning
// group.
func paymentParts(amount money.Minor, paying Paying) []part {
	if len(paying.Items) == 0 {
		return []part{{amount: amount, planningGroup: paying.PlanningGroup}}
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
	p   *Payable
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
	for i := range payables {
		p := &payables[i]
		k, day := r.expected(*p)
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
	var carried []*Payable
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
		docs := make([]paidDocument, len(parts))
		for j, pt := range parts {
			docs[j] = paidDocument{Document: carried[j].Document, amount: pt.amount}
		}
		d := datedFlow{flow: flow{date: at, source: k.source, certainty: k.certainty, late: late, amount: -paid}, currency: k.currency,
			parts: parts, supplier: &k.supplier, documents: docs}
		if k.source == SourcePayable { // the bank whose value days date it (§4: the supplier's ranked bank)
			d.bankCode = it.p.BankCode
		}
		out = append(out, d)
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
func (r reader) pays(items []*Payable, day time.Time) (money.Minor, []part, error) {
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
// Each currency of a flow or of a bank opens at its banks' opening
// balances, and each bucket closes at the opening plus the totals so far.
func (r reader) byCurrency(ctx context.Context, tx pgx.Tx, bs []Bucket, flows []datedFlow, banks []Bank, includeBlocked bool) ([]ForecastCurrency, error) {
	var codes []string
	for _, b := range banks {
		codes = append(codes, b.Currency)
	}
	for _, f := range flows {
		codes = append(codes, f.currency)
	}
	slices.Sort(codes)
	codes = slices.Compact(codes)
	out, at, rows := make([]ForecastCurrency, len(codes)), map[string]int{}, make([]*itemRows, len(codes))
	for i, code := range codes {
		exp, err := currency.Exponent(ctx, tx, code, fmt.Errorf("cash: currency %s of a forecast flow is not known", code))
		if err != nil {
			return nil, err
		}
		out[i], at[code], rows[i] = ForecastCurrency{Currency: code, Exponent: exp, Buckets: make([]BucketFlows, len(bs))}, i, newItemRows(r.doc.Cash.LiquidityItems, len(bs))
		for b := range out[i].Buckets {
			out[i].Buckets[b].Flows = []FlowTotal{}
		}
	}
	for _, b := range banks {
		o, op := r.opening(b), &out[at[b.Currency]].Opening
		op.Amount, op.Banks, op.Stale, op.Gap = r.sums.add(op.Amount, o.Balance), op.Banks+1, op.Stale || o.Stale, op.Gap || o.Gap
	}
	for _, f := range flows {
		r.into(&out[at[f.currency]], rows[at[f.currency]], bs, f, includeBlocked)
	}
	for i := range out {
		out[i].Items = rows[i].ordered()
		closing := out[i].Opening.Amount
		for b := range out[i].Buckets {
			closing = r.sums.add(closing, out[i].Buckets[b].Total)
			out[i].Buckets[b].Closing = closing
		}
	}
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
