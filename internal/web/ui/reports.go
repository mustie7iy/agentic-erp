package ui

import (
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// ReportsPath is the reports' path (T73j): their index, the trial
// balance with an account's entries (T73j2), the supplier open items with their aging, the GR/IR positions,
// the ledger reconciliation with the entries behind a difference, and a
// document's audit history; a house bank's reconciliation report is the
// bank statements' (T73g).
const ReportsPath = "/reports"

// The reports' own paths; a report's CSV download is its path with
// CSVSuffix.
const (
	TrialBalancePath   = ReportsPath + "/trial-balance"
	AccountEntriesPath = TrialBalancePath + "/entries"
	OpenItemsPath      = ReportsPath + "/open-items"
	GRIRPath           = ReportsPath + "/gr-ir"
	LedgerReconPath    = ReportsPath + "/ledger-reconciliation"
	ReconEntriesPath   = LedgerReconPath + "/entries"
	AuditPath          = ReportsPath + "/audit"
	CSVSuffix          = ".csv"
)

// ReportFor is a report's path with the query, "" values left out.
func ReportFor(base string, q url.Values) string { return listPath(base, q) }

// The reports' filters besides those of the other screens; the company,
// ledger, year, period and key date are FieldCompany, FieldLedger,
// FieldYear, FieldPeriod and FieldKeyDate.
const (
	FieldCompare   = "compare_ledger_id"
	FieldAccount   = "account_id"
	FieldSupplier  = "supplier"
	FieldDueDateTo = "due_date_to"
	FieldAllGRIR   = "all"
	FieldObject    = "object_id"
	FieldAgingDays = "aging_days"
)

// ReportLinks are the reports the person may read, "" for one they may
// not, and the house banks whose reconciliation reports they may read.
type ReportLinks struct {
	TrialBalance string
	OpenItems    string
	GRIR         string
	LedgerRecon  string
	Banks        []HouseBank
	BanksUnread  bool // the person may read statements, but the house banks could not be listed
	Audit        bool // the audit history's form
}

// LedgerFilters are a ledger report's filters as chosen: the company, the
// ledger and the one it is compared with (the ledger reconciliation's),
// the fiscal year and the period, which "" leaves out.
type LedgerFilters struct {
	Action       string
	Companies    []Option // nil: the companies could not be listed
	Ledgers      []Option // nil: the ledgers could not be listed
	Compare      []Option // nil: not a reconciliation, or the ledgers could not be listed
	CompareError i18n.Key // the reconciliation's ledger compared with itself
	Year         string
	YearError    i18n.Key
	Period       string
	PeriodError  i18n.Key
}

// BalanceRow is one account's totals of a period in a transaction
// currency, linked to the account's entries of the period.
type BalanceRow struct {
	Account  string // the account's number and name, its id when it could not be named
	Entries  string
	Period   int32
	Currency string
	Debit    Money
	Credit   Money
	DebitLC  Money
	CreditLC Money
}

// TrialBalancePage is a company's trial balance in a ledger and fiscal
// year (listBalances): a page of totals per account, period and
// transaction currency, the next page's path, the download, and the
// API's refusal.
type TrialBalancePage struct {
	Filters LedgerFilters
	Rows    []BalanceRow
	More    string
	CSV     string
	Refusal *Refusal
	Invalid bool
}

// ItemFilters are the open items' filters as chosen: the company, the
// supplier reconciliation account, the supplier by number, the last
// posting and due dates, and the key date the items are aged on.
type ItemFilters struct {
	Companies     []Option
	Accounts      []Option // nil: the chart could not be read, so every reconciliation account's items are listed
	Supplier      string
	SupplierError i18n.Key
	Suppliers     bool // the person may look suppliers up by number
	PostingTo     string
	PostingError  i18n.Key
	DueTo         string
	DueError      i18n.Key
	KeyDate       string
	KeyDateError  i18n.Key
	AgingDays     string // the buckets' last days past the due date, as typed; "" for the API's
	AgingError    i18n.Key
}

// AgingRow is one aging bucket of one currency: not due, overdue From to
// To days (To 0: and more), or without a due date.
type AgingRow struct {
	Bucket   Label
	From, To int32
	Currency string
	Items    int64
	Amount   Money
}

// OpenItemRow is one open item: its entry (linked), its partner by
// number and name (by id when it could not be named), its dates and its
// amounts.
type OpenItemRow struct {
	Entry     string
	Number    string
	Partner   string
	Reference string
	Posting   time.Time
	Due       *time.Time // nil: posted without payment terms
	Discount  *time.Time // the last day of the earliest discount still open on the key date
	Overdue   int        // days past the net due date on the key date; 0 when not overdue
	DownPay   bool
	Amount    Money
	AmountLC  Money
}

// OpenItemsPage is the supplier open items (listOpenItems): their aging
// on the key date, a page of them, the next page's path, the download and
// the API's refusal.
type OpenItemsPage struct {
	Filters  ItemFilters
	Supplier string // the supplier the filter named: its number and name
	Aging    []AgingRow
	Rows     []OpenItemRow
	More     string
	CSV      string
	Refusal  *Refusal
	Invalid  bool
}

// GRIRPage is a company's GR/IR positions (listGrIrBalances): those still
// open, or every one, a page of them, the next page's path, the download
// and the API's refusal.
type GRIRPage struct {
	Companies []Option
	All       bool
	Positions []GRIRPosition
	More      string
	CSV       string
	Refusal   *Refusal
	Invalid   bool
}

// LedgerReconPage is the reconciliation of two ledgers of a company
// (listLedgerReconciliation): each ledger's trial balance check and a
// page of the accounts that differ, each linked to the entries behind its
// difference.
type LedgerReconPage struct {
	Filters     LedgerFilters
	Ledger      string // the ledgers' codes
	Compare     string
	Single      bool // the company keeps one ledger
	Unknown     bool // the ledgers could not be read, and the address names none
	Trials      []TrialView
	Differences []DifferenceView
	More        string
	Refusal     *Refusal
	Invalid     bool
}

// ReconEntryRow is one journal line behind a ledger difference, its entry
// linked.
type ReconEntryRow struct {
	Ledger   string
	Entry    string
	Number   string
	Line     int32
	Posting  time.Time
	Period   int32
	Amount   Money
	AmountLC Money
}

// ReconEntriesPage is the entries behind one account's difference
// (listLedgerReconciliationEntries): the lines of each ledger whose entry
// has no copy in the other.
type ReconEntriesPage struct {
	Back     string
	Account  string
	Currency string
	Ledger   string
	Compare  string
	Rows     []ReconEntryRow
	More     string
	Refusal  *Refusal
	Invalid  bool
}

// AccountEntryRow is one line of a journal entry on the account, its
// entry linked; its text (the line's, else the entry's) is data.
type AccountEntryRow struct {
	Entry    string
	Number   string
	Line     int32
	Posting  time.Time
	Period   int32
	Text     string
	Amount   Money
	AmountLC Money
}

// AccountEntriesPage is a trial balance account's entries in a ledger,
// year and period (listJournalEntries by account_id): the lines on the
// account of a page of entries, the next page's path, the way back and
// the API's refusal.
type AccountEntriesPage struct {
	Back    string
	Account string
	Ledger  string // "": the leading ledger, not named
	Year    string
	Period  string // "": the whole year
	Rows    []AccountEntryRow
	More    string
	Refusal *Refusal
	Invalid bool
}

// AuditDetail is one of an audit event's details: its name and its value
// as the API gives it, data.
type AuditDetail struct {
	Name, Value string
}

// AuditRow is one audit event of a document: when, who, what and its
// outcome, the client address a person acted from, and its details.
type AuditRow struct {
	Seq     int64
	At      time.Time
	Actor   Name
	Kind    Label
	Action  string
	Outcome string
	Address string
	Details []AuditDetail
}

// AuditPage is a document's audit history (listAuditEvents by object),
// newest first, the next page's path and the API's refusal; without a
// document, the form that names one.
type AuditPage struct {
	Object      string
	ObjectError i18n.Key
	Rows        []AuditRow
	More        string
	Refusal     *Refusal
}

// allBox is the GR/IR report's choice of every position, the settled ones
// besides those still open.
func allBox(checked bool) templ.Attributes {
	a := templ.Attributes{"class": "checkbox__input", "type": "checkbox", "id": "grir-all", "name": FieldAllGRIR, "value": "1"}
	if checked {
		a["checked"] = true
	}
	return a
}

// agingKind colours an aging bucket.
func agingKind(l Label) string {
	switch l.Key {
	case i18n.AgingNotDue:
		return "success"
	case i18n.AgingOverdue:
		return "warning"
	default:
		return ""
	}
}

// countText is a number of things as it is written.
func countText(n int64) string { return strconv.FormatInt(n, 10) }
