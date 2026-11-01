package ui

import (
	"context"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// JournalPath is the manual journal entries' path (T73h): the parked
// entries, people's and agents' proposals, by status. A new entry is
// entered at NewEntryPath, a parked one's page is under ParkedBase, and
// the posted journal is browsed at EntriesPath, an entry's page under it.
const (
	JournalPath  = "/journal"
	NewEntryPath = JournalPath + "/new"
	NewEntryFile = NewEntryPath + "/file"
	ParkedBase   = JournalPath + "/parked"
	EntriesPath  = JournalPath + "/entries"
)

// JournalFor is the parked entries' list with the query, "" values left
// out; EntriesFor the journal's.
func JournalFor(q url.Values) string { return listPath(JournalPath, q) }

func EntriesFor(q url.Values) string { return listPath(EntriesPath, q) }

// The entry form's fields besides FieldCompany and FieldCurrency: its
// header, each line's (one of each per line, in the lines' order), the
// lines left out, and what the form's button asks; the journal's filters
// by center.
const (
	FieldDocType         = "document_type"
	FieldDocDate         = "document_date"
	FieldPostingDate     = "posting_date"
	FieldTaxDate         = "tax_date"
	FieldReference       = "reference"
	FieldHeaderText      = "header_text"
	FieldLineAccount     = "line_account"
	FieldLineAmount      = "line_amount"
	FieldLineCostCenter  = "line_cost_center"
	FieldLineProfit      = "line_profit_center"
	FieldLineSegment     = "line_segment"
	FieldLinePartner     = "line_partner"
	FieldLineTaxCode     = "line_tax_code"
	FieldLineTaxArea     = "line_tax_jurisdiction"
	FieldLineTaxBase     = "line_tax_base"
	FieldLineQuantity    = "line_quantity"
	FieldLineUnit        = "line_unit"
	FieldLineText        = "line_text"
	FieldLineRemove      = "line_remove"
	FieldOp              = "op"
	FieldFilledFor       = "filled_for"
	FieldPostTo          = "post_to"
	FieldCostCenter      = "cost_center_id"
	FieldProfitCenter    = "profit_center_id"
	FieldDocumentNumber  = "document_number"
	FieldPostingDateFrom = "posting_date_from"
	FieldPostingDateTo   = "posting_date_to"
	FieldKind            = "created_by_kind"
	FieldRequest         = "approval_request_id"
)

// What the entry form asks: OpAdd adds a line, OpPost posts the new
// entry at once, OpCompany (htmx, the company chosen) renders the form
// again for the company and OpTotals (htmx, an amount changed) its
// running totals alone; any other sends the entry (parks it, or saves the
// change of a parked one).
const (
	OpAdd     = "add"
	OpPost    = "post"
	OpCompany = "company"
	OpTotals  = "totals"
)

// A new entry's ledgers (FieldPostTo): none for every ledger its company
// keeps, else one ledger by its id or a ledger group by its code.
const (
	PostToLedger = "ledger:"
	PostToGroup  = "group:"
)

// ParkedRow is one parked entry of the list. Its header text is data, an
// agent's perhaps.
type ParkedRow struct {
	Path      string
	Company   string // "" when it could not be named
	Status    Label
	DocType   string
	Posting   time.Time
	Header    string
	Debits    *Money // nil when the lines' amounts cannot be summed
	Lines     int
	CreatedBy Name
	Created   time.Time
	Unowned   bool // an agent's proposal no person has taken on
}

// ParkedList is the parked entries: the status tabs, the kind tabs (an
// agent's proposals alone), a page of entries with the next page's path,
// the way to a new entry and to the journal, and the API's refusal.
type ParkedList struct {
	Tabs    []StatusTab
	Kinds   []StatusTab
	Rows    []ParkedRow
	More    string
	New     string // "" when the person may not park
	Entries string // "" when the person may not read the journal
	Refusal *Refusal
	Invalid bool
}

// EntryFilters are the journal's filters as chosen: the company, the
// ledger, the centers (an empty choice for any), the document number and
// the posting dates.
type EntryFilters struct {
	Companies     []Option // nil: the companies could not be listed
	Ledgers       []Option // nil: the ledgers could not be listed
	CostCenters   []Option // nil: the company's cost centers could not be listed
	ProfitCenters []Option
	Number        string
	From, To      string
	FromError     i18n.Key
	ToError       i18n.Key
}

// EntryRow is one posted entry of the journal.
type EntryRow struct {
	Path     string
	Number   string
	DocType  string
	Posting  time.Time
	Header   string
	Source   Label
	Debits   *Money
	Reversed bool // reversed by another entry
	Reversal bool // the reversal of another entry
}

// EntryList is the journal: the filters, a page of entries with the next
// page's path, the way back to the parked entries, and the API's refusal.
type EntryList struct {
	Filters EntryFilters
	Rows    []EntryRow
	More    string
	Refusal *Refusal
	Invalid bool
}

// JournalLine is one line of an entry, parked or posted: its account by
// number and name (its id when it could not be named), its amounts, its
// centers by code, its partner by id (the API names none on a line) and
// its tax code. Its text is data.
type JournalLine struct {
	No           int32
	Account      string
	Amount       Money
	AmountLC     *Money // a posted line's, in the company's currency
	CostCenter   string
	ProfitCenter string
	Segment      string
	Partner      string
	TaxCode      string
	Text         string
}

// EntryHeader is a posted entry's summary.
type EntryHeader struct {
	Company      string // "" when it could not be named
	Ledger       string
	Year, Period int32
	DocType      string
	DocDate      time.Time
	Posting      time.Time
	TaxDate      *time.Time
	Currency     string
	Local, Group string
	RateLC       string
	RateGC       string
	Reference    string
	Header       string
	Source       Label
	Parked       string // the parked entry it was posted for; "" for none
	ReversalOf   string // the entry it reverses; "" for none
	ReversedBy   string // the entry that reversed it; "" for none
	PostedBy     Name
	PostedAt     time.Time
	Debits       *Money
}

// entryRows are a posted entry's summary rows.
func entryRows(h EntryHeader) []Row {
	rows := []Row{}
	if h.Company != "" {
		rows = append(rows, Row{Key: i18n.InvoiceCompany, Value: Data(h.Company)})
	}
	rows = append(rows, Row{Key: i18n.PeriodLedger, Value: Data(h.Ledger)}, Row{Key: i18n.EntryPeriod, Value: T(i18n.PeriodHeading, h.Period, yearText(h.Year))},
		Row{Key: i18n.EntryDocType, Value: Code(h.DocType)}, Row{Key: i18n.EntryDocDate, Value: Day(h.DocDate)}, Row{Key: i18n.EntryPostingDate, Value: Day(h.Posting)})
	if h.TaxDate != nil {
		rows = append(rows, Row{Key: i18n.EntryTaxDate, Value: Day(*h.TaxDate)})
	}
	rows = append(rows, Row{Key: i18n.EntryCurrencies, Value: T(i18n.EntryCurrencyLine, h.Currency, h.Local, h.Group)},
		Row{Key: i18n.EntryRates, Value: T(i18n.EntryRateLine, h.RateLC, h.RateGC)})
	if h.Debits != nil {
		rows = append(rows, Row{Key: i18n.EntryDebits, Value: Amount(*h.Debits)})
	}
	rows = append(rows, textRows(h.Reference, h.Header)...)
	return append(rows, Row{Key: i18n.EntrySource, Value: labelText(h.Source)}, Row{Key: i18n.EntryPostedBy, Value: Principal(h.PostedBy)},
		Row{Key: i18n.EntryPostedAt, Value: Date(h.PostedAt)})
}

// textRows are an entry's reference and header text, those it has.
func textRows(reference, header string) []Row {
	var rows []Row
	if reference != "" {
		rows = append(rows, Row{Key: i18n.EntryReference, Value: Data(reference)})
	}
	if header != "" {
		rows = append(rows, Row{Key: i18n.EntryHeaderText, Value: Data(header)})
	}
	return rows
}

// ReverseForm reverses a posted entry (reverseJournalEntry): the entry's
// ETag as If-Match, a fresh idempotency key, the reversal's posting date
// (empty: the original's) and header text, and what is wrong with them.
type ReverseForm struct {
	Action    string
	CSRF      string
	ETag      string
	Key       string
	Date      string
	Text      string
	DateError i18n.Key
	TextError i18n.Key
}

// EntryPage is one posted entry: its summary and lines, its reversal or
// why there is none, a notice of what was just done and the API's
// refusal of what was asked.
type EntryPage struct {
	Number  string
	Header  EntryHeader
	Lines   []JournalLine
	Reverse *ReverseForm
	Why     i18n.Key
	Done    i18n.Key
	Refusal *Refusal
	Back    string
	Audit   string // its audit history; "" when the person may not read the audit trail
}

// ParkedHeader is a parked entry's summary.
type ParkedHeader struct {
	Status    Label
	Company   string
	DocType   string
	DocDate   time.Time
	Posting   time.Time
	TaxDate   *time.Time
	Currency  string
	Reference string
	Header    string
	Debits    *Money
	CreatedBy Name
	Created   time.Time
	Owner     *Name // nil: no person owns it yet, an agent's proposal
}

// parkedRows are a parked entry's summary rows.
func parkedRows(h ParkedHeader) []Row {
	rows := []Row{{Key: i18n.ParkedStatus, Value: ParkedStatusTag(h.Status)}}
	if h.Company != "" {
		rows = append(rows, Row{Key: i18n.InvoiceCompany, Value: Data(h.Company)})
	}
	rows = append(rows, Row{Key: i18n.EntryDocType, Value: Code(h.DocType)}, Row{Key: i18n.EntryDocDate, Value: Day(h.DocDate)},
		Row{Key: i18n.EntryPostingDate, Value: Day(h.Posting)})
	if h.TaxDate != nil {
		rows = append(rows, Row{Key: i18n.EntryTaxDate, Value: Day(*h.TaxDate)})
	}
	rows = append(rows, Row{Key: i18n.EntryCurrency, Value: Code(h.Currency)})
	if h.Debits != nil {
		rows = append(rows, Row{Key: i18n.EntryDebits, Value: Amount(*h.Debits)})
	}
	rows = append(rows, textRows(h.Reference, h.Header)...)
	rows = append(rows, Row{Key: i18n.ParkedBy, Value: Principal(h.CreatedBy)}, Row{Key: i18n.ParkedAt, Value: Date(h.Created)})
	owner := T(i18n.ParkedNoOwner)
	if h.Owner != nil {
		owner = Principal(*h.Owner)
	}
	return append(rows, Row{Key: i18n.ParkedOwner, Value: owner})
}

// The decisions on a parked entry, by the verb of their route.
const (
	ParkedPost     = "post"
	ParkedCancel   = "cancel"
	ParkedWithdraw = "withdraw"
)

// ParkedForm is one decision on a parked entry (ADR-0052 §7): the
// entry's ETag (the approval request's, for its withdrawal, which names
// the request) as If-Match, a fresh idempotency key, and the reason the
// person typed with its error.
type ParkedForm struct {
	Request   string // the approval request withdrawn; "" for the entry's own decisions
	Verb      string
	Action    string
	CSRF      string
	ETag      string
	Key       string
	Text      string
	TextError i18n.Key
}

// parkedSpec is what a parked entry's form asks: its button and hint,
// and its reason, "" for none.
type parkedSpec struct {
	Button, Hint   i18n.Key
	Text, TextHint i18n.Key
	Warning        bool
}

var parkedSpecs = map[string]parkedSpec{
	ParkedPost:     {Button: i18n.ParkedPostButton, Hint: i18n.ParkedPostHint},
	ParkedCancel:   {Button: i18n.ParkedCancelButton, Hint: i18n.ParkedCancelHint, Text: i18n.ParkedCancelReason, TextHint: i18n.ActionReasonHint, Warning: true},
	ParkedWithdraw: {Button: i18n.ParkedWithdrawButton, Hint: i18n.ParkedWithdrawHint, Text: i18n.ParkedWithdrawReason, TextHint: i18n.ActionReasonHint},
}

// ParkedRequires reports whether the decision needs a reason: the
// cancellation (ReasonInput) and the withdrawal (CommentRequiredInput).
func ParkedRequires(verb string) bool { return parkedSpecs[verb].Text != "" }

// parkedButton is a decision's button, a warning for the cancellation,
// which nothing undoes; named by id so a test and focus find it.
func parkedButton(f ParkedForm) templ.Attributes {
	a := templ.Attributes{"class": "button", "type": "submit", "id": "parked-" + f.Verb + "-button"}
	if parkedSpecs[f.Verb].Warning {
		a["class"] = "button button--warning"
	}
	return a
}

// LinesUpload replaces a parked entry's lines with a CSV file's: the
// entry's ETag, a fresh idempotency key, and what is wrong with the file
// or its rows. The file itself is never kept: after a refusal it is
// chosen again.
type LinesUpload struct {
	Action    string
	CSRF      string
	ETag      string
	Key       string
	FileError i18n.Key
	Rows      []RowError
	More      int // rows wrong besides those listed
	Again     bool
}

// RowError is what is wrong with a CSV file's row: its line in the file,
// the column, and why (the API's reason for a field it refused).
type RowError struct {
	Row    int
	Column string
	Error  i18n.Key
	Reason string // the API's reason, for EntryFieldRefused
}

// ParkedPage is one parked entry: its summary and lines, the person's
// decisions, its change and the upload of its lines, or why there are
// none; whether the person takes it on, an agent's proposal; its
// approval request and the journal entry it was posted as; a notice of
// what was just done and the API's refusal of what was asked.
type ParkedPage struct {
	Header   ParkedHeader
	Lines    []JournalLine
	Edit     string // the change's page; "" when not offered
	Upload   *LinesUpload
	Actions  []ParkedForm
	Why      i18n.Key
	TakeOn   bool
	Approval string // the approval request's page; "" for none
	Journal  string // the journal entry's page; "" until posted
	Done     i18n.Key
	Refusal  *Refusal
	Back     string
	Audit    string // its audit history; "" when the person may not read the audit trail
}

// LineInput is one line of the entry form as the person typed it, with
// what is wrong with each field (Errors, by its field's name) and what the
// API refused of it (Refused, its field's label key and the reason).
type LineInput struct {
	No           int
	Account      string
	Amount       string
	CostCenter   string
	ProfitCenter string
	Segment      string
	Partner      string
	TaxCode      string
	TaxArea      string // the tax jurisdiction
	TaxBase      string
	Quantity     string
	Unit         string
	Text         string
	Errors       map[string]i18n.Key
	Refused      []FieldRefusal
	Focus        bool // the line just added, or the one now in a removed line's place
}

// FieldRefusal is a line's field the API refused, and why.
type FieldRefusal struct {
	Field  i18n.Key
	Reason string
}

// MaxFormLines is the most lines the entry form takes: more come from a
// CSV file (a parked entry's upload), within the form's size.
const MaxFormLines = 100

// EntryForm is the entry form (ADR-0052 §7): a new entry's (its company
// chosen) or a parked one's change (its ETag), a fresh idempotency key,
// the header and lines typed with their errors, the company's accounts
// suggested by number, and the API's refusal.
type EntryForm struct {
	Edit         bool // a parked entry's change
	Action       string
	CSRF         string
	Key          string
	ETag         string   // a change's If-Match; "" for a new entry
	Company      string   // a change's company, named; "" for a new entry
	Companies    []Option // a new entry's choice
	CompanyError i18n.Key
	FilledFor    string   // the company a new entry's currency and dates were filled in for; "" for none
	Local        string   // the company's currency, "" when not known
	Changed      bool     // the company was changed: its currency and dates are filled in again
	PostTo       []Option // a new entry's ledgers; nil when its company keeps one
	DocType      string
	DocDate      string
	PostingDate  string
	TaxDate      string
	Currency     string
	Reference    string
	Header       string
	Errors       map[string]i18n.Key // the header's, by field name
	LinesError   i18n.Key
	Lines        []LineInput
	Totals       EntryTotals
	Accounts     []Option     // the company's accounts, suggested by number
	CanPost      bool         // a new entry may be posted at once
	File         *LinesUpload // a new entry parked from a file: what is wrong with it; nil when not offered
	Refusal      *Refusal
	Back         string
}

// EntryTotals are the form's running debits, credits and their
// difference in the form's currency (decimal strings as the API writes
// amounts), and how many lines' amounts could not be read and are not
// counted.
type EntryTotals struct {
	Debits, Credits, Difference string
	Currency                    string
	Uncounted                   int
}

// CanAdd reports whether the form takes another line.
func (f EntryForm) CanAdd() bool { return len(f.Lines) < MaxFormLines }

// lineID is the id of a line's field.
func lineID(no int, field string) string { return "line-" + strconv.Itoa(no) + "-" + field }

// lineInput is a line's input: its id, name and value, the lines' hint
// and its error by id, aria-invalid with an error, the accounts'
// suggestions on the account, autofocus on the line just added, and on
// the amount the running totals asked again when it changes (htmx).
func lineInput(action string, l LineInput, name, value string) templ.Attributes {
	id := lineID(l.No, name)
	a := templ.Attributes{"id": id, "name": name, "type": "text", "value": value, "class": "input", "translate": "no", "aria-describedby": "lines-hint",
		"autocomplete": "off"}
	if l.Errors[name] != "" {
		a["aria-describedby"], a["aria-invalid"] = "lines-hint "+id+"-error", "true"
	}
	switch name {
	case FieldLineAccount:
		a["list"] = "entry-accounts"
		if l.Focus {
			a["autofocus"] = true
		}
	case FieldLineAmount:
		a["inputmode"] = "decimal"
		maps.Copy(a, totalsAttrs(action))
	case FieldLineTaxBase, FieldLineQuantity:
		a["inputmode"] = "decimal"
	}
	return a
}

// totalsAttrs ask the form's running totals again, the totals' section
// alone swapped (htmx; without it they are those of the form's last
// rendering).
func totalsAttrs(action string) templ.Attributes {
	return templ.Attributes{"hx-post": string(templ.URL(action)), "hx-trigger": "change", "hx-target": "#entry-totals", "hx-swap": "outerHTML",
		"hx-vals": `{"` + FieldOp + `":"` + OpTotals + `"}`}
}

// companySelect is a new entry's company: chosen with htmx, the form is
// rendered again for it, its currency, dates, ledgers and accounts the
// company's; without htmx, sending the form does so (FieldFilledFor).
func companySelect(action string, err i18n.Key) templ.Attributes {
	a := uploadSelect("entry-company", FieldCompany, err)
	maps.Copy(a, templ.Attributes{"hx-post": string(templ.URL(action)), "hx-trigger": "change", "hx-target": "#entry-form", "hx-swap": "outerHTML",
		"hx-vals": `{"` + FieldOp + `":"` + OpCompany + `"}`})
	return a
}

// postToText is a ledgers' choice in words: every ledger, a ledger or a
// ledger group, its code and name data.
func postToText(o Option) templ.Component {
	switch {
	case strings.HasPrefix(o.Value, PostToLedger):
		return T(i18n.EntryPostToLedger, o.Text)
	case strings.HasPrefix(o.Value, PostToGroup):
		return T(i18n.EntryPostToGroup, o.Text)
	default:
		return T(i18n.EntryPostToAll)
	}
}

// postToSelect is the ledgers' choice, its hint and error by id.
func postToSelect(err i18n.Key) templ.Attributes {
	a := templ.Attributes{"id": "entry-post-to", "name": FieldPostTo, "aria-describedby": "entry-post-to-hint"}
	if err != "" {
		a["aria-describedby"], a["aria-invalid"] = "entry-post-to-hint entry-post-to-error", "true"
	}
	return a
}

// entryFileInput is a new entry's file of lines, its hint and error by id.
func entryFileInput(err i18n.Key) templ.Attributes {
	a := templ.Attributes{"id": "entry-file", "name": FieldFile, "type": "file", "aria-describedby": "entry-file-hint", "accept": ".csv,text/csv"}
	if err != "" {
		a["aria-describedby"], a["aria-invalid"] = "entry-file-hint entry-file-error", "true"
	}
	return a
}

// fileButton parks the new entry with a file's lines: the form sent as
// a multipart form to the file's path, without htmx.
func fileButton() templ.Attributes {
	return templ.Attributes{"type": "submit", "class": "button button--secondary", "id": "entry-file-button",
		"formaction": string(templ.URL(NewEntryFile)), "formenctype": "multipart/form-data", "formmethod": "post"}
}

// totalRows are the running totals as the form shows them.
func totalRows(t EntryTotals) []Row {
	return []Row{{Key: i18n.EntryDebits, Value: amount(Money{Amount: t.Debits, Currency: t.Currency})},
		{Key: i18n.EntryCredits, Value: amount(Money{Amount: t.Credits, Currency: t.Currency})},
		{Key: i18n.EntryDifference, Value: amount(Money{Amount: t.Difference, Currency: t.Currency})}}
}

// removeBox leaves the line out: on the next send without htmx, at once
// with it, which posts the form for its lines (ADR-0052 §8).
func removeBox(action string, l LineInput) templ.Attributes {
	return templ.Attributes{"type": "checkbox", "id": lineID(l.No, FieldLineRemove), "name": FieldLineRemove, "value": strconv.Itoa(l.No),
		"class": "checkbox__input", "hx-post": string(templ.URL(action)), "hx-trigger": "change", "hx-target": "#entry-lines", "hx-swap": "outerHTML"}
}

// addButton adds a line: a real submit without htmx, the lines' section
// alone swapped with it.
func addButton(action string) templ.Attributes {
	return templ.Attributes{"type": "submit", "name": FieldOp, "value": OpAdd, "class": "button button--secondary", "id": "entry-add-button",
		"hx-post": string(templ.URL(action)), "hx-target": "#entry-lines", "hx-swap": "outerHTML"}
}

// The line fields, in their order, with their labels: a FieldError names
// them as the API's JournalLineInput does.
var lineFields = []struct {
	Name, API string
	Label     i18n.Key
}{
	{FieldLineAccount, "account_id", i18n.EntryLineAccount}, {FieldLineAmount, "amount", i18n.EntryLineAmount},
	{FieldLineCostCenter, "cost_center_id", i18n.EntryLineCostCenter}, {FieldLineProfit, "profit_center_id", i18n.EntryLineProfitCenter},
	{FieldLineSegment, "segment_id", i18n.EntryLineSegment}, {FieldLinePartner, "partner_id", i18n.EntryLinePartner},
	{FieldLineTaxCode, "tax_code", i18n.EntryLineTaxCode}, {FieldLineTaxArea, "tax_jurisdiction", i18n.EntryLineTaxArea},
	{FieldLineTaxBase, "tax_base", i18n.EntryLineTaxBase}, {FieldLineQuantity, "quantity", i18n.EntryLineQuantity},
	{FieldLineUnit, "quantity.unit", i18n.EntryLineUnit}, {FieldLineText, "line_text", i18n.EntryLineText},
}

// LineFieldLabel is the label of a line's field by the API's name of
// it, "" for one the form does not have.
func LineFieldLabel(api string) i18n.Key {
	for _, f := range lineFields {
		if f.API == api {
			return f.Label
		}
	}
	return ""
}

// lineValue is a line's value of the field.
func lineValue(l LineInput, name string) string {
	switch name {
	case FieldLineAccount:
		return l.Account
	case FieldLineAmount:
		return l.Amount
	case FieldLineCostCenter:
		return l.CostCenter
	case FieldLineProfit:
		return l.ProfitCenter
	case FieldLineSegment:
		return l.Segment
	case FieldLinePartner:
		return l.Partner
	case FieldLineTaxCode:
		return l.TaxCode
	case FieldLineTaxArea:
		return l.TaxArea
	case FieldLineTaxBase:
		return l.TaxBase
	case FieldLineQuantity:
		return l.Quantity
	case FieldLineUnit:
		return l.Unit
	default:
		return l.Text
	}
}

// isoDate is a business date as its text, ISO 8601 until T73l.
func isoDate(t time.Time) string { return t.Format(time.DateOnly) }

// refusedText is a line's field the API refused, by its label, and why.
func refusedText(rf FieldRefusal) templ.Component {
	return Text(func(ctx context.Context) string {
		return i18n.T(ctx, i18n.EntryFieldRefusedOn, i18n.T(ctx, rf.Field), rf.Reason)
	})
}

// parkedStatusKind colours a parked entry's status.
func parkedStatusKind(l Label) string {
	switch l.Key {
	case i18n.ParkedStatusPosted:
		return "success"
	case i18n.ParkedStatusPending:
		return "warning"
	case i18n.ParkedStatusRejected, i18n.ParkedStatusCancelled:
		return "inactive"
	default:
		return ""
	}
}

// emptyOption is a select's choice of none: any center.
func emptyOption(chosen bool) templ.Attributes {
	a := templ.Attributes{"value": ""}
	if chosen {
		a["selected"] = true
	}
	return a
}

// anyChosen reports whether none of the options is chosen.
func anyChosen(options []Option) bool {
	for _, o := range options {
		if o.Selected {
			return false
		}
	}
	return true
}
