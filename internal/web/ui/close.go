package ui

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// ClosePath is the period close's path (T73i): the posting periods of a
// company and ledger, a period's page with its opening, closing, lock and
// unlock request, the pre-close checks, and the foreign-currency
// revaluation.
const ClosePath = "/close"

// The period close's other paths: a period's page is under PeriodsBase,
// the pre-close checks a section of its own, and the revaluation's
// preview and post are posted under its page, so a sign-in again comes
// back to it (its path's parent).
const (
	PeriodsBase     = ClosePath + "/periods"
	ChecksPath      = ClosePath + "/checks"
	RevaluationPath = ClosePath + "/revaluation"
	PreviewPath     = RevaluationPath + "/preview"
	RevaluePostPath = RevaluationPath + "/post"
)

// PeriodsPath is the periods' list with the query, "" values left out.
func PeriodsPath(q url.Values) string { return listPath(ClosePath, q) }

// RevaluationFor is the revaluation's page with the query, "" values left
// out.
func RevaluationFor(q url.Values) string { return listPath(RevaluationPath, q) }

// The close forms' fields besides those of the other screens: the
// ledger, the fiscal year and period, and the revaluation's key date; the
// company is FieldCompany.
const (
	FieldLedger  = "ledger_id"
	FieldYear    = "fiscal_year"
	FieldPeriod  = "period"
	FieldKeyDate = "key_date"
)

// PeriodFilters are the list's filters as chosen: the company, the ledger
// and the fiscal year.
type PeriodFilters struct {
	Companies []Option // nil: the companies could not be listed, so no choice is offered
	Ledgers   []Option // nil: the ledgers could not be listed
	Year      string
	YearError i18n.Key
}

// PeriodRow is one posting period of the list.
type PeriodRow struct {
	Path          string
	Year, Period  int32
	Start, End    time.Time
	Special       bool
	Status        Label
	UnlockPending bool
	UpdatedAt     time.Time
	UpdatedBy     Name
}

// PeriodList is the periods of a company and ledger in a fiscal year:
// the filters, a page of periods with the next page's path, the
// revaluation of the company and ledger, and the API's refusal.
type PeriodList struct {
	Filters PeriodFilters
	Rows    []PeriodRow
	More    string
	Revalue string // the revaluation's page for the company and ledger; "" when the person may not read entries
	Refusal *Refusal
	Invalid bool
}

// PeriodHeader is a period's summary.
type PeriodHeader struct {
	Company   string // "" when it could not be named
	Ledger    string // the ledger's code and name, its id when it could not be named
	Start     time.Time
	End       time.Time
	Special   bool
	Status    Label
	UpdatedAt time.Time
	UpdatedBy Name
}

// periodRows are a period's summary rows.
func periodRows(h PeriodHeader) []Row {
	rows := []Row{{Key: i18n.PeriodStatus, Value: PeriodStatusTag(h.Status)}}
	if h.Company != "" {
		rows = append(rows, Row{Key: i18n.InvoiceCompany, Value: Data(h.Company)})
	}
	rows = append(rows, Row{Key: i18n.PeriodLedger, Value: Data(h.Ledger)}, Row{Key: i18n.PeriodStart, Value: Day(h.Start)},
		Row{Key: i18n.PeriodEnd, Value: Day(h.End)})
	if h.Special {
		rows = append(rows, Row{Key: i18n.PeriodKind, Value: T(i18n.PeriodSpecial)})
	}
	return append(rows, Row{Key: i18n.PeriodUpdatedBy, Value: Principal(h.UpdatedBy)}, Row{Key: i18n.PeriodUpdatedAt, Value: Date(h.UpdatedAt)})
}

// The decisions on a period, by the verb of their route.
const (
	PeriodOpen   = "open"
	PeriodClose  = "close"
	PeriodLock   = "lock"
	PeriodUnlock = "unlock"
)

// PeriodForm is one decision on a period (ADR-0052 §7): the period's
// ETag as If-Match, a fresh idempotency key, the reason the person typed
// for an unlock and its error.
type PeriodForm struct {
	Verb      string
	Action    string
	CSRF      string
	ETag      string
	Key       string
	Text      string
	TextError i18n.Key
}

// periodSpec is what a period's form asks: its button and hint, and its
// reason, "" for none.
type periodSpec struct {
	Button, Hint   i18n.Key
	Text, TextHint i18n.Key
	Warning        bool
}

var periodSpecs = map[string]periodSpec{
	PeriodOpen:   {Button: i18n.PeriodOpenButton, Hint: i18n.PeriodOpenHint},
	PeriodClose:  {Button: i18n.PeriodCloseButton, Hint: i18n.PeriodCloseHint},
	PeriodLock:   {Button: i18n.PeriodLockButton, Hint: i18n.PeriodLockHint, Warning: true},
	PeriodUnlock: {Button: i18n.PeriodUnlockButton, Hint: i18n.PeriodUnlockHint, Text: i18n.PeriodUnlockReason, TextHint: i18n.ActionReasonHint},
}

// PeriodRequires reports whether the decision needs a reason: the unlock
// request alone (ReasonInput).
func PeriodRequires(verb string) bool { return periodSpecs[verb].Text != "" }

// periodButton is a decision's button, a warning for the lock, which
// only an approved unlock request undoes; named by id so focus stays on
// it across a swap.
func periodButton(f PeriodForm) templ.Attributes {
	a := templ.Attributes{"class": "button", "type": "submit", "id": "period-" + f.Verb + "-button"}
	if periodSpecs[f.Verb].Warning {
		a["class"] = "button button--warning"
	}
	return a
}

// PeriodPage is one posting period: its summary, its pending unlock
// request, the person's decisions or why there are none, the pre-close
// checks, the revaluation at its end, a notice of what was just done and
// the API's refusal of what was asked.
type PeriodPage struct {
	Year, Period int32
	Header       PeriodHeader
	Back         string // the periods of its company, ledger and year
	Unlock       string // the pending unlock request's page in the inbox; "" for none
	Actions      []PeriodForm
	Why          i18n.Key
	Checks       string // the pre-close checks' section; "" when the period is locked
	Revalue      string // the revaluation at its end; "" when not offered
	Done         i18n.Key
	Refusal      *Refusal
}

// ChecksView is the pre-close checks of a company's period (ADR-0052 §2):
// the GR/IR positions still open, the ledger's reconciliation with
// another ledger the company keeps, and the bank statements not yet
// reconciled, each with its result or the API's refusal in its place.
type ChecksView struct {
	Self       string // the section's own path
	Period     string // the period's page; "" when it is not known
	GRIR       GRIRCheck
	Ledgers    LedgerCheck
	Statements StatementCheck
	Invalid    bool // the query names no company, ledger, year and period
}

// GRIRCheck is the open GR/IR positions (listGrIrBalances, leading
// ledger, as they stand now): a page of them, whether there are more, or
// the refusal.
type GRIRCheck struct {
	Positions []GRIRPosition
	More      bool
	Refusal   *Refusal
}

// GRIRPosition is one purchase order item's open GR/IR position: the
// order by id (the API names none on a balance), its received and
// invoiced quantities and its balance.
type GRIRPosition struct {
	Order              string
	Item               int32
	Received, Invoiced string
	Unit               string
	Balance            Money
	Last               time.Time
}

// LedgerCheck is the ledger's reconciliation with another ledger the
// company keeps (listLedgerReconciliation) in the period: each ledger's
// trial balance check and a page of the accounts that differ; for a
// company keeping one ledger, nothing to reconcile.
type LedgerCheck struct {
	Unknown     bool // the ledgers could not be read
	Single      bool
	Ledger      string // the ledger's code
	Compare     string // the compared ledger's code
	Trials      []TrialView
	Differences []DifferenceView
	More        bool
	Refusal     *Refusal
}

// TrialView is one ledger's trial balance check in local currency.
type TrialView struct {
	Ledger   string
	Debit    Money
	Credit   Money
	Balanced bool
}

// DifferenceView is one account and currency whose balance differs
// between the two ledgers: the account by id (the API names none on a
// difference).
type DifferenceView struct {
	Account    string
	Currency   string
	Balance    Money
	Compare    Money
	Difference Money
}

// StatementCheck is the company's bank statements not reconciled: those
// imported and those after a gap, each linked to its page and its bank's
// reconciliation report, whether there are more, and the lists by status.
type StatementCheck struct {
	Rows     []StatementRow
	More     bool
	Imported string // the statements' list of that status
	Gap      string
	Refusal  *Refusal
}

// RevaluationForm is the preview of a revaluation (previewRevaluation):
// the company, the ledger (the leading one by default) and the key date,
// a fresh idempotency key, and what is wrong.
type RevaluationForm struct {
	CSRF         string
	Key          string
	Companies    []Option
	Ledgers      []Option // nil: the ledgers could not be listed, so the leading one is valued
	KeyDate      string
	CompanyError i18n.Key
	KeyDateError i18n.Key
}

// PostForm posts the run a preview valued (postRevaluationRun): the
// preview's company, ledger and key date, and a fresh idempotency key.
type PostForm struct {
	CSRF    string
	Key     string
	Company string
	Ledger  string // "" for the leading ledger
	KeyDate string
}

// TotalView is a revaluation's items of one account and currency with a
// gain or a loss, summed.
type TotalView struct {
	Account      string // the account's number, its id when it could not be named
	Currency     string
	Direction    Label
	Items        int32
	Amount       Money
	Booked       Money // in local currency
	Valued       Money
	Difference   Money
	DifferenceGC Money
}

// ItemView is one valued item: an open item (its entry and line, its
// partner by id) or a flagged account's balance in one currency.
type ItemView struct {
	Account      string
	Partner      string // "" for a balance
	Entry        string // "" for a balance
	Line         int32
	Amount       Money
	Booked       Money
	Rate         string // "" for a balance valued at zero without a rate
	Valued       Money
	Difference   Money
	DifferenceGC Money
	Direction    Label
}

// PreviewView is what a revaluation at a key date would value: the period
// it ends, the rate type, the totals and the items, and the post of the
// run for a person who may post.
type PreviewView struct {
	KeyDate      time.Time
	Year, Period int32
	RateType     Label
	Totals       []TotalView
	Items        []ItemView
	Post         *PostForm
}

// RevaluationRow is one posted or cancelled run of the company.
type RevaluationRow struct {
	Path      string
	KeyDate   time.Time
	Ledger    string
	Status    Label
	Items     int32
	CreatedBy Name
	CreatedAt time.Time
}

// RevaluationPage is the revaluation of a company: the preview's form, a
// preview with the post of its run, the company's runs, and the API's
// refusals.
type RevaluationPage struct {
	Form      *RevaluationForm
	Refusal   *Refusal
	Preview   *PreviewView
	Runs      []RevaluationRow
	RunsMore  string
	RunsError *Refusal
}

// RevaluationRunPage is one revaluation run: its summary, its entries by
// id, its totals and items, and a notice of its post.
type RevaluationRunPage struct {
	Header    RunSummary
	Totals    []TotalView
	Items     []ItemView
	Done      i18n.Key
	Back      string
	Valuation string // the valuation entry's id; "" when no item had a difference
	Reset     string
}

// RunSummary is a revaluation run's summary.
type RunSummary struct {
	Company   string
	Ledger    string
	KeyDate   time.Time
	RateType  Label
	Status    Label
	Items     int32
	CreatedBy Name
	CreatedAt time.Time
}

// revaluationRows are a run's summary rows.
func revaluationRows(h RunSummary) []Row {
	rows := []Row{{Key: i18n.RevaluationStatus, Value: RevaluationStatusTag(h.Status)}}
	if h.Company != "" {
		rows = append(rows, Row{Key: i18n.InvoiceCompany, Value: Data(h.Company)})
	}
	return append(rows, Row{Key: i18n.PeriodLedger, Value: Data(h.Ledger)}, Row{Key: i18n.RevaluationKeyDate, Value: Day(h.KeyDate)},
		Row{Key: i18n.RevaluationRateType, Value: labelText(h.RateType)}, Row{Key: i18n.RevaluationItemCount, Value: Number(h.Items)},
		Row{Key: i18n.RevaluationPostedBy, Value: Principal(h.CreatedBy)}, Row{Key: i18n.RevaluationPostedAt, Value: Date(h.CreatedAt)})
}

// periodStatusKind colours a period's status; the text says it.
func periodStatusKind(l Label) string {
	switch l.Key {
	case i18n.PeriodStatusOpen:
		return "success"
	case i18n.PeriodStatusClosed:
		return "warning"
	case i18n.PeriodStatusLocked:
		return "inactive"
	default:
		return ""
	}
}

// revaluationStatusKind colours a run's status.
func revaluationStatusKind(l Label) string {
	switch l.Key {
	case i18n.RevaluationPosted:
		return "success"
	case i18n.RevaluationCancelled:
		return "inactive"
	default:
		return ""
	}
}

// directionKind colours a valued item's direction.
func directionKind(l Label) string {
	switch l.Key {
	case i18n.DirectionGain:
		return "success"
	case i18n.DirectionLoss:
		return "warning"
	default:
		return ""
	}
}

// decimal is a decimal string in the request's locale; outside a request
// with a locale, as the API gives it.
func decimal(ctx context.Context, value string) string {
	if l, ok := i18n.From(ctx); ok {
		return l.Decimal(value)
	}
	return value
}

// rate is an exchange rate in the locale's digits.
func rate(value string) templ.Component {
	return Text(func(ctx context.Context) string { return decimal(ctx, value) })
}

// checked is a check's result: whether it found nothing to look at.
func checked(ok bool) templ.Component {
	if ok {
		return Tag(i18n.CheckClear, "success")
	}
	return Tag(i18n.CheckAttention, "warning")
}

// yearText is a fiscal year as it is written, never grouped as a number.
func yearText(y int32) string { return strconv.Itoa(int(y)) }

// balanced reports whether every ledger's debits and credits net to zero.
func balanced(trials []TrialView) bool {
	for _, tb := range trials {
		if !tb.Balanced {
			return false
		}
	}
	return true
}

// trialResult is a ledger's trial balance check in words.
func trialResult(ok bool) templ.Component {
	if ok {
		return T(i18n.CheckTrialBalanced)
	}
	return T(i18n.CheckTrialUnbalanced)
}
