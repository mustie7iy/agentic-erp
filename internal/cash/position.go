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
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The daily cash position (ADR-0051 §8, T53c) is computed at the read
// from the payments context's house banks, statements and payments and
// the ledger's open lines and rates, through the ports below, and the
// context's own memo records on the banks (T53d), all in the request's
// transaction, which the contract's x-snapshot runs at REPEATABLE READ:
// every figure is of one database state.

// Endpoints implements the cash part of the generated API.
type Endpoints struct {
	LegalEntities LegalEntities
	Settings      Settings
	Payments      Payments
	Ledger        Ledger
	Sessions      Sessions          // agents' sessions, for their proposals
	Partners      Partners          // suppliers' names, for the forecast's drill-down (T53e4)
	Pages         *pagination.Codec // list page tokens
	Now           func() time.Time  // the read date's clock; nil: time.Now
	// Consistent begins the read a snapshot reads the forecast in, apart
	// from the transaction it writes in (T53f).
	Consistent Consistency
}

// Consistency begins a read-only transaction at REPEATABLE READ of the
// caller's tenant, as the caller (uow.Consistent): it returns a context
// carrying it and the transaction, which the caller rolls back.
type Consistency interface {
	Read(ctx context.Context) (context.Context, pgx.Tx, error)
}

// LegalEntities finds a legal entity (tenancy context).
type LegalEntities interface {
	Company(ctx context.Context, id uuid.UUID) (Company, bool, error)
	// ByCode is the id of the legal entity with the company code.
	ByCode(ctx context.Context, code string) (uuid.UUID, bool, error)
}

// Company is a legal entity as the cash position reads it.
type Company struct {
	Code     string // its company code in the configuration
	Currency string // its local currency
}

// Settings reads the active configuration (configuration context): the
// company's cash settings and calendar.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Payments is what the cash position reads of the payments context.
type Payments interface {
	// Banks are the legal entity's active house banks by code, each with
	// its latest statement closing before the day.
	Banks(ctx context.Context, legalEntity uuid.UUID, before time.Time) ([]Bank, error)
	// Released are the posted payments among the ids.
	Released(ctx context.Context, ids []uuid.UUID) ([]Payment, error)
	// Pending are the legal entity's payments of runs proposed or being
	// released that are not posted yet.
	Pending(ctx context.Context, legalEntity uuid.UUID) ([]Payment, error)
	// HouseBanks are the legal entity's active house banks by code.
	HouseBanks(ctx context.Context, legalEntity uuid.UUID) ([]HouseBank, error)
	// StatementLine is a bank statement line; found is false when there is
	// none.
	StatementLine(ctx context.Context, statement uuid.UUID, lineNo int32) (l StatementLine, found bool, err error)
	// HouseBankCodes are the codes of the house banks among the ids, active
	// or not, keyed by id: what a memo record shows beside its bank's id.
	HouseBankCodes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
	// Payables are the legal entity's supplier open items that no live
	// payment run holds (the run's payment counts instead, Pending), as a
	// run on or after the date would pay them (ADR-0051 §5, T53e).
	Payables(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]Payable, error)
	// Paying is what each of the legal entity's payments among the ids
	// pays, keyed by payment: what the forecast derives a run's payment's
	// liquidity items from (ADR-0051 §4, T53e3).
	Paying(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID, on time.Time) (map[uuid.UUID]Paying, error)
}

// Ledger is what the cash position reads of the ledger context.
type Ledger interface {
	// OpenLines are the leading ledger's open lines on the accounts.
	OpenLines(ctx context.Context, legalEntity uuid.UUID, accounts []uuid.UUID) ([]OpenLine, error)
	// Rate is the latest rate of the type from one currency into another
	// valid on the date; found is false when there is none.
	Rate(ctx context.Context, rateType, from, to string, date time.Time) (r Rate, found bool, err error)
}

// Bank is a house bank with the statement its position opens at.
type Bank struct {
	ID                uuid.UUID
	Code, Last4       string
	Currency          string
	Exponent          int
	ClearingAccountID uuid.UUID
	Statement         *Statement // nil: no statement before the first day
}

// Statement is the latest statement of a bank before the first day: its
// closing date and balance, whether a statement up to it is in gap, and
// its figures against the ledger.
type Statement struct {
	Date    time.Time
	Closing money.Minor
	Gap     bool
	Book    Book
}

// Book is a statement's closing balance against the ledger on its date,
// as the reconciliation report has it (ADR-0047): the main bank account's
// balance in the bank's currency (nil without a main bank account), what
// the statement lines not posted, excluded or posted later explain, and
// what is left.
type Book struct {
	Ledger, Unexplained              *money.Minor
	NotPosted, Excluded, PostedLater money.Minor
}

// Payment is a payment from a house bank: Amount is what it pays,
// positive, in Currency.
type Payment struct {
	ID, HouseBankID uuid.UUID
	// RunID and RunNumber are its payment run and the run's document
	// number, SupplierID the business partner it pays: what the forecast's
	// drill-down names (T53e4).
	RunID, SupplierID uuid.UUID
	RunNumber         string
	Method            string
	RunDate           time.Time
	Amount            money.Minor
	Currency          string
	// MatchedOn is the closing date of the statement that booked it, nil
	// while no statement line's match clears it.
	MatchedOn *time.Time
}

// OpenLine is an open line on a clearing account, in its entry's
// currency, with the context and document it was posted for.
type OpenLine struct {
	AccountID     uuid.UUID
	Currency      string
	Amount        money.Minor
	SourceContext string
	SourceID      *uuid.UUID
}

// Rate is a rate into the display currency.
type Rate struct {
	Rate      string // 12 decimals, as people read it
	ValidFrom time.Time
	Convert   func(money.Minor) (money.Minor, error)
}

// Sources and certainties of flows (ADR-0051 §4).
const (
	SourceInTransit    = "in_transit"
	SourcePaymentRun   = "payment_run"
	CertaintyConfirmed = "confirmed"
)

// DefaultDays is how many days a position covers when not asked.
const DefaultDays = 14

// Query is what a position covers: the legal entity, the first day (nil:
// the read date), the days (0: DefaultDays) and the display currency
// ("": the company's).
type Query struct {
	LegalEntity uuid.UUID
	From        *time.Time
	Days        int
	Display     string
}

// Position is a legal entity's cash position.
type Position struct {
	LegalEntity uuid.UUID
	ReadDate    time.Time // the company's date at the read, in TimeZone
	TimeZone    string    // the IANA zone the read date is in
	From        time.Time
	Days        int
	Banks       []BankPosition
	Currencies  []CurrencyPosition
	Converted   Converted
}

// BankPosition is a house bank's position in its currency.
type BankPosition struct {
	Bank
	Opening    Opening
	CreditLine *money.Minor
	InTransit  money.Minor // the payments in transit, whatever their day
	Days       []Day
}

// Opening is what the first day opens at.
type Opening struct {
	Balance    money.Minor
	Date       *time.Time // the statement's closing date
	Stale, Gap bool
}

// Day is a day of a position: Available is Closing with the credit line.
type Day struct {
	Date                        time.Time
	Opening, Closing, Available money.Minor
	Flows                       []FlowTotal
}

// FlowTotal is the flows of a day of one source, certainty and lateness.
type FlowTotal struct {
	Source, Certainty string
	Late              bool
	Amount            money.Minor
	Count             int
}

// CurrencyPosition is the banks of one currency summed.
type CurrencyPosition struct {
	Currency   string
	Exponent   int
	CreditLine *money.Minor
	Days       []Day
}

// Converted is every currency's position in the display currency.
type Converted struct {
	Currency   string
	Exponent   int
	Rates      []CurrencyRate
	Incomplete []string
	Days       []Day // without flows
}

// CurrencyRate is the rate a currency is converted at.
type CurrencyRate struct {
	Currency, RateType, Rate string
	ValidFrom                time.Time
	Stale                    bool
}

// errTooLarge refuses a position whose sums are out of the range of an
// amount.
var errTooLarge = problem.Unprocessable("cash.amount_too_large", "The amounts of the cash position add up to more than an amount can hold.")

// Position computes the legal entity's cash position. A legal entity the
// caller cannot act in is not found, as one that does not exist.
func (e *Endpoints) Position(ctx context.Context, q Query) (Position, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Position{}, err
	}
	co, err := e.company(ctx, q.LegalEntity)
	if err != nil {
		return Position{}, err
	}
	display := cmp.Or(q.Display, co.Currency)
	displayExp, err := currency.Exponent(ctx, tx, display, problem.Unprocessable("cash.unknown_currency", "The display currency is not known.",
		problem.FieldError{Field: "display_currency", Code: "unknown", Message: "an ISO 4217 currency in the reference data"}))
	if err != nil {
		return Position{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Position{}, fmt.Errorf("cash: configuration: %w", err)
	}
	read, zone, err := e.readDate(doc, co.Code)
	if err != nil {
		return Position{}, err
	}
	r := reader{doc: doc, company: co.Code, settings: companyCash(doc, co.Code), read: read, sums: &sums{}}
	p := Position{LegalEntity: q.LegalEntity, ReadDate: read, TimeZone: zone, From: read, Days: cmp.Or(q.Days, DefaultDays)}
	if q.From != nil {
		if p.From = clock.Day(*q.From); p.From.Before(r.read) {
			return Position{}, problem.Unprocessable("request.validation_failed", "The position starts on the read date or later; past days are actuals.",
				problem.FieldError{Field: "from", Code: "invalid", Message: "on or after " + r.read.Format(time.DateOnly)})
		}
	}
	banks, err := e.Payments.Banks(ctx, q.LegalEntity, p.From)
	if err != nil {
		return Position{}, fmt.Errorf("cash: house banks of %s: %w", q.LegalEntity, err)
	}
	flows, err := e.flows(ctx, r, q.LegalEntity, banks)
	if err != nil {
		return Position{}, err
	}
	if err := memoFlows(ctx, tx, r, q.LegalEntity, banks, p.From.AddDate(0, 0, p.Days-1), flows); err != nil {
		return Position{}, err
	}
	p.Banks = r.banks(banks, flows, p.From, p.Days)
	p.Currencies = r.sums.currencies(p.Banks)
	if p.Converted, err = e.convert(ctx, r, p.Currencies, display, displayExp, p.From, p.Days); err != nil {
		return Position{}, err
	}
	if r.sums.overflow {
		return Position{}, errTooLarge
	}
	return p, nil
}

// company is the legal entity, if the caller may act in it.
func (e *Endpoints) company(ctx context.Context, id uuid.UUID) (Company, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Company{}, err
	}
	if !access.Allows(id) {
		return Company{}, problem.NotFoundOf("legal entity")
	}
	co, ok, err := e.LegalEntities.Company(ctx, id)
	if err != nil {
		return Company{}, fmt.Errorf("cash: legal entity %s: %w", id, err)
	}
	if !ok {
		return Company{}, problem.NotFoundOf("legal entity")
	}
	return co, nil
}

// readDate is the company's date at the read, in its time zone, and the
// zone's name: UTC when the company has none, or the document has no such
// company (ADR-0051 as built, T72n1). A Toronto treasurer's evening is
// still that day, not the next UTC one.
func (e *Endpoints) readDate(doc *config.Config, code string) (time.Time, string, error) {
	d, err := companyDay(doc, code, clock.Now(e.Now))
	return d.Date, d.Zone, err
}

// companyCash is the company's cash settings, the defaults when the
// document has no such company.
func companyCash(doc *config.Config, code string) config.CompanyCash {
	if co := doc.Company(code); co != nil {
		return co.Cash
	}
	return config.CompanyCash{}
}

// reader is a read of a company's position: its document, settings and
// read date.
type reader struct {
	doc      *config.Config
	company  string
	settings config.CompanyCash
	read     time.Time
	sums     *sums
}

// maxDays bounds the search for a working day, so a calendar whose every
// day is a holiday still answers.
const maxDays = 366

// working reports whether the company does business on the day: not one
// of its weekend days, not one of its holidays (ADR-0048). The rule is the
// configuration's (ADR-0058 §3), shared with the close calendar.
func (r reader) working(d time.Time) bool {
	return r.doc.BusinessDay(r.company, d)
}

// onOrAfter is the first working day on or after d.
func (r reader) onOrAfter(d time.Time) time.Time {
	for i := 0; i < maxDays && !r.working(d); i++ {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// before is the last working day before d.
func (r reader) before(d time.Time) time.Time {
	d = d.AddDate(0, 0, -1)
	for i := 0; i < maxDays && !r.working(d); i++ {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// flow is one payment's effect on a bank's balance on a day.
type flow struct {
	date              time.Time
	source, certainty string
	late              bool
	amount            money.Minor
}

// valueDate is a payment's day at the bank: its run date plus the value
// days of the house bank of the code.
func (r reader) valueDate(bankCode string, p Payment) (time.Time, bool) {
	return r.atBank(clock.Day(p.RunDate).AddDate(0, 0, r.settings.ValueDaysFor(bankCode, p.Method)))
}

// atBank is the day a flow dated d is at the bank: the working day on or
// after d; one before the read date is late and expected the working day
// after it.
func (r reader) atBank(d time.Time) (time.Time, bool) {
	d = r.onOrAfter(d)
	if d.Before(r.read) {
		return r.onOrAfter(r.read.AddDate(0, 0, 1)), true
	}
	return d, false
}

// flows are each bank's payments in transit and of runs not released yet.
func (e *Endpoints) flows(ctx context.Context, r reader, legalEntity uuid.UUID, banks []Bank) (map[uuid.UUID][]flow, error) {
	transit, err := e.inTransit(ctx, r, legalEntity, banks)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]flow{}
	for _, t := range transit {
		out[t.bank] = append(out[t.bank], t.flows...)
	}
	pending, err := e.Payments.Pending(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: payments of runs not released of %s: %w", legalEntity, err)
	}
	for _, p := range pending {
		if i := slices.IndexFunc(banks, func(b Bank) bool { return b.ID == p.HouseBankID }); i >= 0 && p.Currency == banks[i].Currency {
			d, late := r.valueDate(banks[i].Code, p)
			out[p.HouseBankID] = append(out[p.HouseBankID], flow{date: d, source: SourcePaymentRun, certainty: CertaintyConfirmed, late: late, amount: -p.Amount})
		}
	}
	return out, nil
}

// transit is a payment in transit from a bank: its open lines on the
// bank's clearing account, each a flow on the payment's day at the bank.
type transit struct {
	bank    uuid.UUID
	payment Payment
	flows   []flow
}

// inTransit are the banks' payments in transit: the payments' open lines
// on the bank's clearing account that no statement line up to the opening
// statement booked. A revaluation's valuation and reset lines there are
// the ledger's, not a payment's, and never count (ADR-0038, T53a).
func (e *Endpoints) inTransit(ctx context.Context, r reader, legalEntity uuid.UUID, banks []Bank) ([]transit, error) {
	byID, accounts := map[uuid.UUID]Bank{}, make([]uuid.UUID, len(banks))
	for i, b := range banks {
		byID[b.ID], accounts[i] = b, b.ClearingAccountID
	}
	lines, err := e.Ledger.OpenLines(ctx, legalEntity, accounts)
	if err != nil {
		return nil, fmt.Errorf("cash: open lines on the clearing accounts of %s: %w", legalEntity, err)
	}
	open, ids := paymentLines(lines)
	released, err := e.Payments.Released(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("cash: payments in transit of %s: %w", legalEntity, err)
	}
	var out []transit
	for _, p := range released {
		if b, ok := byID[p.HouseBankID]; ok && !booked(p, b) {
			out = append(out, transit{bank: b.ID, payment: p, flows: r.inTransit(b, p, open[p.ID])})
		}
	}
	return out, nil
}

// paymentLines are the open lines entries posted for payments, by
// payment, and the payments in their order. A payment posts one line on
// its bank's clearing account; a second would count too.
func paymentLines(lines []OpenLine) (map[uuid.UUID][]OpenLine, []uuid.UUID) {
	open := map[uuid.UUID][]OpenLine{}
	var ids []uuid.UUID
	for _, l := range lines {
		if l.SourceContext != "payments" || l.SourceID == nil {
			continue
		}
		if open[*l.SourceID] == nil {
			ids = append(ids, *l.SourceID)
		}
		open[*l.SourceID] = append(open[*l.SourceID], l)
	}
	return open, ids
}

// inTransit are a payment's open lines on its bank's clearing account in
// the bank's currency, on its day at the bank.
func (r reader) inTransit(b Bank, p Payment, lines []OpenLine) []flow {
	d, late := r.valueDate(b.Code, p)
	var out []flow
	for _, l := range lines {
		if l.AccountID == b.ClearingAccountID && l.Currency == b.Currency {
			out = append(out, flow{date: d, source: SourceInTransit, certainty: CertaintyConfirmed, late: late, amount: l.Amount})
		}
	}
	return out
}

// booked reports whether the bank's opening statement already holds the
// payment: a line of a statement up to it is matched to the payment, whose
// posting may still wait for approval.
func booked(p Payment, b Bank) bool {
	return p.MatchedOn != nil && b.Statement != nil && !p.MatchedOn.After(b.Statement.Date)
}

// banks are the banks' positions over the days from the first.
func (r reader) banks(banks []Bank, flows map[uuid.UUID][]flow, from time.Time, days int) []BankPosition {
	credit := map[string]string{}
	for _, c := range r.settings.CreditLines {
		credit[c.HouseBank] = c.Amount
	}
	out := make([]BankPosition, len(banks))
	for i, b := range banks {
		bp := BankPosition{Bank: b, Opening: r.opening(b)}
		if c, ok := credit[b.Code]; ok {
			// Safe to ignore: the document's validation parsed the amount in
			// the bank's currency before it was activated (config.creditLines).
			amount, _ := money.ParseDecimal(c, b.Exponent)
			bp.CreditLine = &amount
		}
		bp.Days, bp.InTransit = r.position(bp, flows[b.ID], from, days)
		out[i] = bp
	}
	return out
}

// opening is what a bank's first day opens at: its latest statement's
// closing balance before it, stale when that statement closes before the
// working day before the read date; none (stale) without one.
func (r reader) opening(b Bank) Opening {
	s := b.Statement
	if s == nil {
		return Opening{Stale: true}
	}
	return Opening{Balance: s.Closing, Date: &s.Date, Gap: s.Gap, Stale: s.Date.Before(r.before(r.read))}
}

// sums adds amounts and remembers whether a sum fell out of the range of
// an amount, so a position is refused once (errTooLarge) rather than
// checked at every addition.
type sums struct{ overflow bool }

func (s *sums) add(a, b money.Minor) money.Minor {
	t, err := money.Sum(a, b)
	s.overflow = s.overflow || err != nil
	return t
}

// position is a bank's days from the first: the opening balance with the
// flows before the first day, then each day's flows. inTransit is the
// payments in transit, whatever their day.
func (r reader) position(bp BankPosition, flows []flow, from time.Time, days int) (out []Day, inTransit money.Minor) {
	var credit money.Minor
	if bp.CreditLine != nil {
		credit = *bp.CreditLine
	}
	bal := bp.Opening.Balance
	byDay := map[time.Time][]flow{}
	for _, f := range flows {
		if f.source == SourceInTransit {
			inTransit = r.sums.add(inTransit, f.amount)
		}
		if f.date.Before(from) {
			bal = r.sums.add(bal, f.amount)
			continue
		}
		byDay[f.date] = append(byDay[f.date], f)
	}
	out = make([]Day, days)
	for i := range out {
		d := Day{Date: from.AddDate(0, 0, i), Opening: bal, Flows: []FlowTotal{}}
		for _, f := range byDay[d.Date] {
			d.Flows = r.sums.addTotal(d.Flows, FlowTotal{Source: f.source, Certainty: f.certainty, Late: f.late, Amount: f.amount, Count: 1})
			bal = r.sums.add(bal, f.amount)
		}
		d.Closing, d.Available = bal, r.sums.add(bal, credit)
		out[i] = d
	}
	return out, inTransit
}

// addTotal adds t to the total of its source, certainty and lateness,
// keeping the totals in that order.
func (s *sums) addTotal(ts []FlowTotal, t FlowTotal) []FlowTotal {
	key := func(x FlowTotal) string { return fmt.Sprintf("%s/%s/%v", x.Source, x.Certainty, x.Late) }
	i, found := slices.BinarySearchFunc(ts, key(t), func(x FlowTotal, k string) int { return cmp.Compare(key(x), k) })
	if !found {
		return slices.Insert(ts, i, t)
	}
	ts[i].Amount, ts[i].Count = s.add(ts[i].Amount, t.Amount), ts[i].Count+t.Count
	return ts
}
