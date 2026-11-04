package cash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The cash actuals (ADR-0051 §11, T53f2): a legal entity's bank statement
// lines by value date, posted or not, per currency, house bank and bucket,
// each classified by liquidity item as its match says (§4): a line matched
// to payments takes the items of the supplier items they settled, as the
// forecast classifies a payment; one matched to a posting rule the rule's
// item; one matched to the incoming payments clearing account or excluded
// by a person its direction's default; and one with no decided match its
// direction's default too, counted unclassified, so the actuals are seen
// before reconciliation finishes. Computed at the read, as the position
// and the forecast are, from one database state (x-snapshot).

// ActualLine is a bank statement line as the cash actuals read it: its
// statement and number, the statement's reference and the line's at the
// bank, its house bank and currency, signed amount, value date
// and status in reconciliation (ADR-0047), and what its decided match
// clears (Match: MatchPayment, MatchPostingRule, MatchIncoming,
// MatchExcluded or MatchNone): the payments, or the posting rule's
// transaction code.
type ActualLine struct {
	StatementID   uuid.UUID
	LineNo        int32
	StatementRef  string
	BankReference string
	HouseBankID   uuid.UUID
	Currency      string
	Amount        money.Minor
	ValueDate     time.Time
	Status        string
	Match         string
	PostingRule   string
	Payments      []Payment
}

// What classified a statement line.
const (
	MatchPayment     = "payment"
	MatchPostingRule = "posting_rule"
	MatchIncoming    = "incoming"
	MatchExcluded    = "excluded"
	MatchNone        = "unmatched"
)

// ActualsQuery is what the actuals cover: the legal entity's lines whose
// value date is from From to To, in buckets of Grain ("": a day).
type ActualsQuery struct {
	LegalEntity uuid.UUID
	From, To    time.Time
	Grain       string
}

// Actuals are a legal entity's actual cash flows per currency.
type Actuals struct {
	LegalEntity uuid.UUID
	CompanyCode string
	ReadDate    time.Time
	TimeZone    string
	From, To    time.Time
	Grain       string
	Buckets     []Bucket
	Currencies  []ActualCurrency
}

// ActualCurrency is the lines of one currency: per bucket in the buckets'
// order and over the range, per house bank (the active ones by code, then
// those no longer active), and by liquidity item
// and its hierarchy (each row's own shares by what classified them, as
// ItemFlows' source, certainty actual).
type ActualCurrency struct {
	Currency string
	Exponent int
	ActualSum
	Buckets []ActualSum
	Banks   []ActualBank
	Items   []ItemRow
}

// ActualSum is the lines of a bucket or a range: money in, money out,
// their total and count, and what of it no decided match classifies.
type ActualSum struct {
	Inflow, Outflow, Total money.Minor
	Count                  int
	Unclassified           Beyond
}

// ActualBank is a house bank's lines per bucket and over the range.
type ActualBank struct {
	ID      uuid.UUID
	Code    string
	Active  bool
	Buckets []money.Minor
	Total   money.Minor
	Count   int
}

// actual is a statement line classified: its bank's code, its shares by
// liquidity item, and those shares by the documents behind them (pieces),
// which the forecast variance matches to a snapshot's (T53f3).
type actual struct {
	ActualLine
	bankCode string
	shares   []Share
	pieces   []actualPiece
}

// actualPiece is the part of a statement line one document behind it
// pays, by liquidity item: the keys it is known by in a snapshot
// (docKey), most specific first, and its shares. A line no payment
// traces is one piece of no key.
type actualPiece struct {
	keys   []string
	shares []Share
}

// unclassified reports whether no decided match classifies the line.
func (a actual) unclassified() bool { return a.Match == MatchNone }

// actualsRead is a legal entity's classified lines as read: the reader,
// the zone, the company, and its active house banks.
type actualsRead struct {
	tx    pgx.Tx
	r     reader
	zone  string
	co    Company
	lines []actual
	banks []HouseBank
}

// errActualsTooLarge refuses actuals whose sums are out of the range of an
// amount.
func errActualsTooLarge() *problem.Problem {
	return problem.Unprocessable("cash.amount_too_large", "The amounts of the cash actuals add up to more than an amount can hold.")
}

// Actuals computes the legal entity's actual cash flows. A legal entity
// the caller cannot act in is not found; a grain not known, or a range
// ending before it starts or of more periods than the grain's limit, is
// refused.
func (e *Endpoints) Actuals(ctx context.Context, q ActualsQuery) (Actuals, error) {
	grain := cmp.Or(q.Grain, config.GrainDay)
	bs, err := actualBuckets(q.From, q.To, grain)
	if err != nil {
		return Actuals{}, err
	}
	ar, err := e.readActuals(ctx, q.LegalEntity, bs[0].Start, bs[len(bs)-1].End)
	if err != nil {
		return Actuals{}, err
	}
	a := Actuals{LegalEntity: q.LegalEntity, CompanyCode: ar.co.Code, ReadDate: ar.r.read, TimeZone: ar.zone, From: bs[0].Start, To: bs[len(bs)-1].End,
		Grain: grain, Buckets: bs}
	if a.Currencies, err = ar.byCurrency(ctx, bs); err != nil {
		return Actuals{}, err
	}
	if ar.r.sums.overflow {
		return Actuals{}, errActualsTooLarge()
	}
	return a, nil
}

// actualBuckets are the periods of the grain from the first day up to the
// one holding the last, that one ending on the last day, within the
// grain's limit of the company's buckets (config.MaxPeriods).
func actualBuckets(from, to time.Time, grain string) ([]Bucket, error) {
	invalid := func(field, message string) error {
		return problem.Unprocessable("request.validation_failed", "The actuals cover periods of a grain from the first value date to the last.",
			problem.FieldError{Field: field, Code: "invalid", Message: message})
	}
	from, to = clock.Day(from), clock.Day(to)
	limit, known := config.MaxPeriods(grain)
	switch {
	case !known:
		return nil, invalid("grain", "day, week or month")
	case to.Before(from):
		return nil, invalid("to", "on or after from")
	}
	bs := buckets(from, []config.Bucket{{Grain: grain, Count: limit}})
	n := slices.IndexFunc(bs, func(b Bucket) bool { return !b.End.Before(to) })
	if n < 0 {
		return nil, invalid("to", fmt.Sprintf("at most %d periods of a %s from %s", limit, grain, from.Format(time.DateOnly)))
	}
	bs = bs[:n+1]
	bs[n].End = to
	return bs, nil
}

// readActuals reads the legal entity's statement lines whose value date
// is from from to to and classifies each by the active derivation; a
// legal entity the caller cannot act in is not found.
func (e *Endpoints) readActuals(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) (actualsRead, error) {
	if _, _, err := reqctx.Request(ctx); err != nil {
		return actualsRead{}, err
	}
	co, err := e.company(ctx, legalEntity)
	if err != nil {
		return actualsRead{}, err
	}
	return e.classified(ctx, co, legalEntity, from, to)
}

// classified is readActuals of the company of the legal entity, its
// access already decided.
func (e *Endpoints) classified(ctx context.Context, co Company, legalEntity uuid.UUID, from, to time.Time) (actualsRead, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return actualsRead{}, err
	}
	ar := actualsRead{tx: tx, co: co}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return actualsRead{}, fmt.Errorf("cash: configuration: %w", err)
	}
	read, zone, err := e.readDate(doc, ar.co.Code)
	if err != nil {
		return actualsRead{}, err
	}
	ar.zone, ar.r = zone, reader{doc: doc, company: ar.co.Code, settings: companyCash(doc, ar.co.Code), read: read, sums: &sums{}}
	lines, err := e.Payments.Actuals(ctx, legalEntity, from, to)
	if err != nil {
		return actualsRead{}, fmt.Errorf("cash: statement lines of %s: %w", legalEntity, err)
	}
	if ar.banks, err = e.Payments.HouseBanks(ctx, legalEntity); err != nil {
		return actualsRead{}, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	if ar.lines, err = e.classifyActuals(ctx, ar, legalEntity, lines); err != nil {
		return actualsRead{}, err
	}
	return ar, nil
}

// classifyActuals are the lines with their banks' codes and their shares
// by liquidity item: what the payments a line was matched to settled is
// read once for every line.
func (e *Endpoints) classifyActuals(ctx context.Context, ar actualsRead, legalEntity uuid.UUID, lines []ActualLine) ([]actual, error) {
	var payIDs, bankIDs []uuid.UUID
	for _, l := range lines {
		payIDs = append(payIDs, ids(l.Payments)...)
		if !slices.Contains(bankIDs, l.HouseBankID) {
			bankIDs = append(bankIDs, l.HouseBankID)
		}
	}
	var paying map[uuid.UUID]Paying
	if len(payIDs) > 0 {
		var err error
		if paying, err = e.Payments.Paying(ctx, legalEntity, payIDs, ar.r.read); err != nil {
			return nil, fmt.Errorf("cash: what the matched payments of %s paid: %w", legalEntity, err)
		}
	}
	codes, err := idref.Resolve(ctx, e.Payments.HouseBankCodes, bankIDs, houseBankRef)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	d := newRules(ar.r.doc.Cash.Derivation)
	out := make([]actual, len(lines))
	for i, l := range lines {
		out[i] = actual{ActualLine: l, bankCode: codes[l.HouseBankID]}
		if out[i].shares, out[i].pieces, err = actualShares(d, ar.r.sums, l, paying); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// actualShares split a line over its liquidity items (ADR-0051 §4): a line
// matched to payments over what they settled, each item derived on its
// own and shared out over the line's amount (classify), so a bank charge
// deducted from a payment takes its items' shares down; a line matched to
// a posting rule the rule's item, else its direction's default; any other
// line its direction's default. The pieces are those shares by the
// documents the payments settled, each with the keys a snapshot knows the
// document by.
func actualShares(d *rules, s *sums, l ActualLine, paying map[uuid.UUID]Paying) ([]Share, []actualPiece, error) {
	// A payment match whose payments are not read (the cash position reads
	// none its run dropped, and no match can name one) is classified as an
	// excluded line is, by its direction's default: nothing traces it.
	if l.Match == MatchPayment && len(l.Payments) > 0 {
		var parts []part
		var keys [][]string
		for _, p := range l.Payments {
			parts = append(parts, paymentParts(-p.Amount, paying[p.ID])...)
			keys = append(keys, paymentKeys(p.ID, paying[p.ID])...)
		}
		each, err := partsShares(d, l.Amount, parts)
		if err != nil {
			return nil, nil, err
		}
		if len(each) != len(keys) {
			// Parts that cancel out are one, the line's: known by its payments.
			keys = [][]string{{}}
			for _, p := range l.Payments {
				keys[0] = append(keys[0], paymentKey(p.ID))
			}
		}
		pieces := make([]actualPiece, len(each))
		for i, shares := range each {
			pieces[i] = actualPiece{keys: keys[i], shares: shares}
		}
		return sumShares(s, each), pieces, nil
	}
	b := Basis{Direction: directionOf(l.Amount)}
	if l.Match == MatchPostingRule {
		b.PostingRule = l.PostingRule
	}
	shares, err := d.derive(l.Amount, b)
	return shares, []actualPiece{{shares: shares}}, err
}

// directionOf is the direction of a flow of the amount: money in is an
// inflow, everything else an outflow.
func directionOf(amount money.Minor) string {
	if amount > 0 {
		return config.DirectionInflow
	}
	return config.DirectionOutflow
}

// byCurrency sums the lines per currency, by code, into their buckets, per
// house bank and by liquidity item; every active house bank is listed in
// its currency by code (as the port reads them), with lines or not, then
// each bank no longer active in the order of its first line.
func (ar actualsRead) byCurrency(ctx context.Context, bs []Bucket) ([]ActualCurrency, error) {
	var codes []string
	for _, b := range ar.banks {
		codes = append(codes, b.Currency)
	}
	for _, l := range ar.lines {
		codes = append(codes, l.Currency)
	}
	slices.Sort(codes)
	codes = slices.Compact(codes)
	out, rows := make([]ActualCurrency, len(codes)), make([]*itemRows, len(codes))
	for i, code := range codes {
		exp, err := currency.Exponent(ctx, ar.tx, code, fmt.Errorf("cash: currency %s of a statement is not known", code))
		if err != nil {
			return nil, err
		}
		out[i], rows[i] = ActualCurrency{Currency: code, Exponent: exp, Buckets: make([]ActualSum, len(bs)), Banks: []ActualBank{}},
			newItemRows(ar.r.doc.Cash.LiquidityItems, len(bs))
	}
	at := func(code string) int { i, _ := slices.BinarySearch(codes, code); return i }
	for _, b := range ar.banks {
		c := &out[at(b.Currency)]
		c.Banks = append(c.Banks, ActualBank{ID: b.ID, Code: b.Code, Active: true, Buckets: make([]money.Minor, len(bs))})
	}
	for _, l := range ar.lines {
		ar.into(&out[at(l.Currency)], rows[at(l.Currency)], bs, l)
	}
	for i := range out {
		out[i].Items = rows[i].ordered()
	}
	return out, nil
}

// into adds a line to its currency: to its bucket and the range, to its
// bank, made when the bank is no longer active, and its shares to the
// rows by liquidity item under what classified it.
func (ar actualsRead) into(c *ActualCurrency, rows *itemRows, bs []Bucket, l actual) {
	s := ar.r.sums
	// The first bucket ending on or after the value date: the lines are
	// those of the buckets' range.
	b, _ := slices.BinarySearchFunc(bs, l.ValueDate, func(b Bucket, d time.Time) int { return cmp.Compare(b.End.Unix(), d.Unix()) })
	c.Buckets[b], c.ActualSum = s.addLine(c.Buckets[b], l), s.addLine(c.ActualSum, l)
	i := slices.IndexFunc(c.Banks, func(x ActualBank) bool { return x.ID == l.HouseBankID })
	if i < 0 {
		i = len(c.Banks)
		c.Banks = append(c.Banks, ActualBank{ID: l.HouseBankID, Code: l.bankCode, Buckets: make([]money.Minor, len(bs))})
	}
	bank := &c.Banks[i]
	bank.Buckets[b], bank.Total, bank.Count = s.add(bank.Buckets[b], l.Amount), s.add(bank.Total, l.Amount), bank.Count+1
	f := datedFlow{flow: flow{date: l.ValueDate, source: l.Match, certainty: CertaintyActual, amount: l.Amount}, currency: l.Currency}
	for _, sh := range l.shares {
		rows.add(s, sh, f, b, true)
	}
}

// addLine adds a line to a sum: to money in or out, the total and count,
// and to what is unclassified when no decided match classifies it.
func (s *sums) addLine(t ActualSum, l actual) ActualSum {
	if l.Amount > 0 {
		t.Inflow = s.add(t.Inflow, l.Amount)
	} else {
		t.Outflow = s.add(t.Outflow, l.Amount)
	}
	t.Total, t.Count = s.add(t.Total, l.Amount), t.Count+1
	if l.unclassified() {
		t.Unclassified = s.addTo(t.Unclassified, l.Amount)
	}
	return t
}
