package ui

import (
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// BankPath is the bank statements' path (T73g): the statements of the
// house banks, a statement's lines and the reconciliation report.
const BankPath = "/bank"

// StatementsPath is the statements' list with the query, "" values left
// out.
func StatementsPath(q url.Values) string { return listPath(BankPath, q) }

// Masked is an account as the workspace shows it: its last four
// characters after a mask, never the whole number (ADR-0047: the API gives
// people the whole account; a page shows no more than it needs).
func Masked(account string) string {
	if r := []rune(account); len(r) > 4 {
		account = string(r[len(r)-4:])
	}
	return "•••• " + account
}

// HouseBank is a house bank as the pages name it: its code, account and
// currency, its company, and its statements' and report's paths.
type HouseBank struct {
	Code       string
	Account    string // masked
	Currency   string
	Company    string // "" when it could not be named
	Inactive   bool
	Statements string
	Report     string
}

// StatementRow is one statement of the list.
type StatementRow struct {
	Path       string
	Ref        string // the bank's statement id: data
	Sequence   string
	Bank       string // the house bank's code, "" when it could not be named
	Account    string // masked
	Company    string
	OpeningDay time.Time
	ClosingDay time.Time
	Opening    Money
	Closing    Money
	Lines      int32
	Status     Label
	Gap        bool   // its opening balance is not the previous statement's closing balance
	BankReport string // the bank's reconciliation report
}

// UploadForm imports a bank's statement file (importBankStatements): the
// house bank and the file's format chosen, the file, a fresh idempotency
// key, and what is wrong with each. The file itself is never kept: after
// a refusal it is chosen again.
type UploadForm struct {
	CSRF        string
	Key         string
	Banks       []Option
	Format      string
	BankError   i18n.Key
	FormatError i18n.Key
	FileError   i18n.Key
	Again       bool // the form was sent and is shown again: its file is chosen again
}

// UploadFormats are the file formats a statement is imported in.
var UploadFormats = []Option{{Value: "camt.053"}, {Value: "mt940"}}

// formatText names an upload's format.
var formatText = map[string]i18n.Key{"camt.053": i18n.BankFormatCamt, "mt940": i18n.BankFormatMT940}

// uploadSelect is one of the upload's selects: its id and name, and its
// error by id.
func uploadSelect(id, name string, err i18n.Key) templ.Attributes {
	a := templ.Attributes{"id": id, "name": name}
	if err != "" {
		a["aria-describedby"], a["aria-invalid"] = id+"-error", "true"
	}
	return a
}

// fileInput is the upload's file: one file, its hint and error by id.
func fileInput(err i18n.Key) templ.Attributes {
	a := templ.Attributes{"id": "upload-file", "name": FieldFile, "type": "file", "aria-describedby": "upload-file-hint"}
	if err != "" {
		a["aria-describedby"], a["aria-invalid"] = "upload-file-hint upload-file-error", "true"
	}
	return a
}

// UploadPath is where a statement file is posted.
const UploadPath = BankPath + "/statements"

// The upload form's fields.
const (
	FieldHouseBank = "house_bank_id"
	FieldFormat    = "format"
	FieldFile      = "file"
)

// StatementList is the statements' list: the house banks, the status
// tabs, the bank filter, a page of statements with the next page's path,
// the upload form for a person who may import, a notice of an import just
// done, and the API's refusal to list them.
type StatementList struct {
	Banks   []HouseBank
	Tabs    []StatusTab
	Filter  BankFilter
	Rows    []StatementRow
	More    string
	Upload  *UploadForm
	Done    i18n.Key
	Refusal *Refusal
	Invalid bool
	// UploadRefusal is the API's refusal of an import.
	UploadRefusal *Refusal
}

// BankFilter is the list's house bank filter: a plain GET form keeping
// the status.
type BankFilter struct {
	Status    string
	Banks     []Option // nil: the house banks could not be listed
	Companies []Option // nil: the companies could not be listed (T73i2)
}

// StatementHeader is a statement's summary.
type StatementHeader struct {
	Ref          string
	Sequence     string
	Format       string
	Bank         string // the house bank's code, "" when it could not be named
	Account      string // masked
	OpeningDay   time.Time
	ClosingDay   time.Time
	Opening      Money
	Closing      Money
	Lines        int32
	Status       Label
	Previous     string // the previous statement's page, "" for the bank's first
	ReconciledAt *time.Time
	ReconciledBy *Name
}

// statementRows are a statement's summary rows.
func statementRows(h StatementHeader) []Row {
	rows := []Row{{Key: i18n.StatementStatus, Value: StatementStatusTag(h.Status)}, {Key: i18n.StatementBank, Value: Data(headerBank(h.Bank, h.Account))},
		{Key: i18n.StatementSequence, Value: Data(h.Sequence)}, {Key: i18n.StatementFormat, Value: Code(h.Format)},
		{Key: i18n.StatementOpening, Value: dated(h.Opening, h.OpeningDay)}, {Key: i18n.StatementClosing, Value: dated(h.Closing, h.ClosingDay)},
		{Key: i18n.StatementLineCount, Value: Number(h.Lines)}}
	if h.Previous != "" {
		rows = append(rows, Row{Key: i18n.StatementPrevious, Value: ButtonLink(h.Previous, i18n.StatementPreviousOpen)})
	}
	if h.ReconciledAt != nil {
		rows = append(rows, Row{Key: i18n.StatementReconciledAt, Value: Date(*h.ReconciledAt)})
	}
	if h.ReconciledBy != nil {
		rows = append(rows, Row{Key: i18n.StatementReconciledBy, Value: Principal(*h.ReconciledBy)})
	}
	return rows
}

// Target is one thing a match clears: a payment (with the bank file of a
// batch booking), a posting rule, or the incoming payments clearing
// account.
type Target struct {
	Candidate int32
	Payment   string
	BankFile  string
	Rule      string // a posting rule's transaction code
	RuleText  string // what the rule posts, "" when the company's rules do not name it
	Incoming  bool
}

// MatchInfo is a line's live match: who made it (a rule, a person, the
// reconciliation agent), why, what it clears, and where its posting is.
type MatchInfo struct {
	Kind          Label
	Rule          *Label // the matching rule of a rule's match
	By            Name   // who made it; for a rule, who ran the rules
	Agent         bool   // the reconciliation agent's proposal, for a person to decide
	Proposal      Label  // what an agent proposes: a match or the exclusion
	Status        Label
	Reason        string // data: a rule's finding, a person's reason, an agent's text
	Targets       []Target
	Open          bool // candidates for a person to choose from
	PostingDay    *time.Time
	PostingReason string
	Approval      string // the posting's approval request's page, "" for none
	Entry         string // the posting's journal entry id and year, "" before it posts
}

// LineForm is one decision on a line (ADR-0052 §7): the statement's ETag
// as If-Match, a fresh idempotency key, what the person typed, and its
// errors; Open shows it unfolded (it holds what was typed).
type LineForm struct {
	Verb      string
	Line      int32
	ID        string // unique on the page: the line and the verb
	Action    string
	CSRF      string
	ETag      string
	Key       string
	Text      string
	Date      string
	TextError i18n.Key
	DateError i18n.Key
	Open      bool
}

// Choice is one thing a person may match a line to by hand: a candidate
// the rules found (its payments), a posting rule, or the incoming
// payments clearing account.
type Choice struct {
	Value    string // the field's value: payments:<ids>, rule:<code> or incoming
	Kind     string // ChoicePayments, ChoiceRule, ChoiceIncoming or ChoiceFound
	Number   int32  // a candidate's number
	Payments []string
	Rule     string
	RuleText string
	Found    *FoundPayment // a payment a person found by searching (T73g2), nil for the others
}

// The kinds of a manual match's choice. A payment found by searching is
// sent as a candidate's payments are (payments:<id>); ChoiceFound only
// shows it as found, by its payee and amount when the search shows it,
// by its id when a refused match brings it back to the statement.
const (
	ChoicePayments = "payments"
	ChoiceRule     = "rule"
	ChoiceIncoming = "incoming"
	ChoiceFound    = "found"
)

// FoundPayment is a payment a line may be matched to, as the search
// shows it: its end-to-end id, payee, the account it was paid to by its
// last characters, its amount and the journal entry that posted it.
type FoundPayment struct {
	EndToEnd string
	Supplier string
	Account  string
	Amount   Money
	Entry    string
}

// MatchForm is a line's manual match: its choices, the one chosen and
// what is wrong with the choice, besides the reason; Search is the
// line's search for a payment the rules did not find, "" when it has
// none (T73g2).
type MatchForm struct {
	LineForm
	Choices     []Choice
	Choice      string
	ChoiceError i18n.Key
	Search      string
}

// The decisions on a line, by the verb of their route.
const (
	LineMatch   = "match"
	LineExclude = "exclude"
	LinePost    = "post"
	LineAccept  = "accept"
	LineReject  = "reject"
)

// FieldChoice is the manual match's choice field; FieldLine the line a
// decision's form is for.
const (
	FieldChoice = "target"
	FieldLine   = "line"
)

// lineSpec is what a line's form asks: its button and hint, its text
// (a reason when required) and its date, "" for none.
type lineSpec struct {
	Button, Hint   i18n.Key
	Text, TextHint i18n.Key
	Required       bool
	Date, DateHint i18n.Key
	Warning        bool
}

var lineSpecs = map[string]lineSpec{
	LineAccept:  {Button: i18n.LineAccept, Hint: i18n.LineAcceptHint, Text: i18n.ActionReleaseComment, TextHint: i18n.ActionCommentHint},
	LineReject:  {Button: i18n.LineReject, Hint: i18n.LineRejectHint, Text: i18n.LineRejectReason, TextHint: i18n.ActionReasonHint, Required: true, Warning: true},
	LineMatch:   {Button: i18n.LineMatch, Hint: i18n.LineMatchHint, Text: i18n.LineMatchReason, TextHint: i18n.ActionReasonHint, Required: true},
	LineExclude: {Button: i18n.LineExclude, Hint: i18n.LineExcludeHint, Text: i18n.LineExcludeReason, TextHint: i18n.ActionReasonHint, Required: true, Warning: true},
	LinePost: {Button: i18n.LinePost, Hint: i18n.LinePostHint, Date: i18n.LinePostDate, DateHint: i18n.LinePostDateHint, Text: i18n.LinePostReason,
		TextHint: i18n.LinePostReasonHint},
}

// LineRequires reports whether the decision's text is a required reason.
func LineRequires(verb string) bool { return lineSpecs[verb].Required }

// lineButton is a decision's button: a warning for one that turns
// something down.
func lineButton(verb string) templ.Attributes {
	if lineSpecs[verb].Warning {
		return templ.Attributes{"class": "button button--warning", "type": "submit"}
	}
	return templ.Attributes{"class": "button", "type": "submit"}
}

// disclosure is a decision's fold: open when it holds what the person
// typed, so a refusal or an error is in view.
func disclosure(open bool) templ.Attributes {
	a := templ.Attributes{"class": "disclosure"}
	if open {
		a["open"] = true
	}
	return a
}

// choiceRadio is one choice of a manual match.
func choiceRadio(id string, c Choice, chosen string) templ.Attributes {
	a := templ.Attributes{"type": "radio", "id": id, "name": FieldChoice, "value": c.Value}
	if c.Value == chosen {
		a["checked"] = true
	}
	return a
}

// choiceFieldset names the choice's error, when it has one.
func choiceFieldset(f MatchForm) templ.Attributes {
	if f.ChoiceError == "" {
		return templ.Attributes{}
	}
	return templ.Attributes{"aria-describedby": f.ID + "-choice-error"}
}

// LineView is one line of a statement: the bank's booking, its status,
// its live match, the outcome of the agent's last proposal a person
// decided, and the person's decisions on it.
type LineView struct {
	No          int32
	Anchor      string
	Amount      Money
	Reversal    bool
	BookingDay  time.Time
	ValueDay    time.Time
	Code        string
	BankRef     string
	OwnerRef    string
	Text        string // the bank's text: data
	Status      Label
	Match       *MatchInfo
	LastOutcome *Label // the outcome of the agent's latest proposal a person decided
	Accept      *LineForm
	Reject      *LineForm
	Manual      *MatchForm
	Exclude     *LineForm
	Post        *LineForm
}

// lineRows are a line's facts.
func lineRows(l LineView) []Row {
	rows := []Row{{Key: i18n.LineStatus, Value: LineStatusTag(l.Status)}, {Key: i18n.LineAmountLabel, Value: Amount(l.Amount)},
		{Key: i18n.LineBooked, Value: Day(l.BookingDay)}, {Key: i18n.LineValue, Value: Day(l.ValueDay)},
		{Key: i18n.LineCode, Value: Code(l.Code)}}
	if l.Reversal {
		rows = append(rows, Row{Key: i18n.LineReversal, Value: T(i18n.LineReversalYes)})
	}
	rows = append(rows, Row{Key: i18n.LineBankRef, Value: Data(l.BankRef)}, Row{Key: i18n.LineOwnerRef, Value: Data(l.OwnerRef)})
	if l.Text != "" {
		rows = append(rows, Row{Key: i18n.LineText, Value: Data(l.Text)})
	}
	if l.LastOutcome != nil {
		rows = append(rows, Row{Key: i18n.LineLastProposal, Value: labelText(*l.LastOutcome)})
	}
	return rows
}

// decided reports whether the line offers the person no decision.
func decided(l LineView) bool {
	return l.Accept == nil && l.Reject == nil && l.Manual == nil && l.Exclude == nil && l.Post == nil
}

// StatementPage is one statement: its summary and lines, the bank's
// report, why nothing may be decided, a notice of what was just done and
// the API's refusal of what was asked.
type StatementPage struct {
	Header StatementHeader
	Gap    bool
	Report string
	Lines  []LineView
	Why    i18n.Key // why the lines offer no decision, "" when they may
	Done   i18n.Key
	// DoneLine is the line just decided, linked from the notice (0: none).
	DoneLine int32
	Refusal  *Refusal
	// Rules runs the company's matching rules again (matchBankStatement),
	// nil when the person may not or the statement is reconciled; File
	// downloads the bank's file the statement was imported from (T73g2).
	Rules *LineForm
	File  string
}

// PaymentSearch is a line's search for the payments it may be matched to
// by hand (T73g2): the statement's page, its reference, the line, the
// filter and what it found.
type PaymentSearch struct {
	Back    string
	Ref     string
	Line    LineView
	Filter  PaymentFilter
	Results PaymentResults
}

// PaymentFilter is the search's GET form: the amount paid's bounds as
// typed, in the statement's currency, and the runs' dates, with their
// errors.
type PaymentFilter struct {
	Action                                 string
	Currency                               string
	Min, Max, From, To                     string
	MinError, MaxError, FromError, ToError i18n.Key
}

// PaymentResults is what the search found: the payments as the choices
// of the line's match by hand, or why there are none to choose.
type PaymentResults struct {
	Invalid i18n.Key // what is wrong with the filter, "" when nothing is
	Refusal *Refusal
	Why     i18n.Key   // why the line takes no match now, "" when it does
	Match   *MatchForm // nil when nothing was found or the line takes no match
	Found   []Choice   // what was found, shown without a form when the line takes no match
	More    string
}

// PaymentSearchID is the search's filter and results, which htmx swaps
// when the filter is sent, so its errors show on their fields.
const PaymentSearchID = "payment-search"

// Fields of the payment search's filter.
const (
	FieldAmountMin = "amount_min"
	FieldAmountMax = "amount_max"
	FieldRunFrom   = "run_date_from"
	FieldRunTo     = "run_date_to"
)

// searchForm is the filter's attributes: a plain GET form, which htmx
// sends for the results alone and puts in the address bar.
func searchForm(path string) templ.Attributes {
	a := get(path)
	maps.Copy(a, templ.Attributes{"hx-get": string(templ.URL(path)), "hx-target": "#" + PaymentSearchID, "hx-swap": "outerHTML",
		"hx-push-url": "true"})
	return a
}

// Figures are a statement's figures on a date (ADR-0047, T49h): the
// bank's closing balance against the ledger's, and what explains the
// difference.
type Figures struct {
	AsOf        time.Time
	Closing     Money
	Ledger      *Money // nil: the house bank has no main bank account
	Excluded    Money
	InTransit   Money
	PostedLater Money
	Unexplained *Money
}

// figureRows are the figures as a summary: the bank against the book,
// then what explains the difference, then what is left unexplained.
func figureRows(f Figures) []Row {
	rows := []Row{{Key: i18n.ReconAsOf, Value: Day(f.AsOf)}, {Key: i18n.ReconClosing, Value: Amount(f.Closing)}}
	if f.Ledger == nil {
		rows = append(rows, Row{Key: i18n.ReconLedger, Value: T(i18n.ReconNoLedger)})
	} else {
		rows = append(rows, Row{Key: i18n.ReconLedger, Value: Amount(*f.Ledger)})
	}
	rows = append(rows, Row{Key: i18n.ReconExcluded, Value: Amount(f.Excluded)}, Row{Key: i18n.ReconInTransit, Value: Amount(f.InTransit)},
		Row{Key: i18n.ReconPostedLater, Value: Amount(f.PostedLater)})
	switch {
	case f.Unexplained == nil:
	case Zero(f.Unexplained.Amount):
		rows = append(rows, Row{Key: i18n.ReconUnexplained, Value: Tag(i18n.ReconExplained, "success")})
	default:
		rows = append(rows, Row{Key: i18n.ReconUnexplained, Value: unexplained(*f.Unexplained)})
	}
	return rows
}

// Zero reports whether a decimal amount as the API gives it is zero.
func Zero(amount string) bool {
	return strings.Trim(strings.TrimPrefix(amount, "-"), "0.") == ""
}

// Count is how many lines are in a status.
type Count struct {
	Status Label
	N      int32
}

// ReconLine is one line of a statement in the report.
type ReconLine struct {
	No         int32
	BookingDay time.Time
	Amount     Money
	Reversal   bool
	Code       string
	Status     Label
	Match      *Label // the live match's kind
	Rule       *Label
	PostingDay *time.Time
	Approval   string
	Outcome    *Label
}

// ReconStatement is one statement of the report.
type ReconStatement struct {
	Path       string
	Ref        string
	Sequence   string
	Status     Label
	OpeningDay time.Time
	Opening    Money
	Figures    Figures
	Counts     []Count
	Lines      []ReconLine
	Anchor     string
}

// ReconSummary is the period's: its statements, its opening balance and
// its figures on its last closing date.
type ReconSummary struct {
	Statements int32
	Opening    *Money
	OpeningDay *time.Time
	Figures    *Figures
	Counts     []Count
}

// ReconFilters are the report's period and line status as chosen, and
// what is wrong with a date.
type ReconFilters struct {
	Path       string // the report's own path
	From, To   string
	FromError  i18n.Key
	ToError    i18n.Key
	LineStatus string
	Statuses   []KeyOption
}

// KeyOption is a select's choice named by the catalog.
type KeyOption struct {
	Value    string
	Label    i18n.Key
	Selected bool
}

// ReconciliationPage is a house bank's reconciliation report.
type ReconciliationPage struct {
	Bank       HouseBank
	Filters    ReconFilters
	Summary    *ReconSummary
	Statements []ReconStatement
	More       string
	Refusal    *Refusal
}

// statementKind colours a statement's status: a gap is amber, a
// reconciled statement green; the text says it.
func statementKind(l Label) string {
	switch l.Key {
	case i18n.StatementStatusGap:
		return "warning"
	case i18n.StatementStatusReconciled:
		return "success"
	default:
		return ""
	}
}

// lineKind colours a line's status.
func lineKind(l Label) string {
	switch l.Key {
	case i18n.LineStatusPosted:
		return "success"
	case i18n.LineStatusExcluded:
		return "inactive"
	case i18n.LineStatusPending, i18n.LineStatusProposed:
		return "warning"
	default:
		return ""
	}
}

// dated is an amount on its date.
func dated(m Money, day time.Time) templ.Component { return amountOn(m, day) }

// bankText is a house bank's code and masked account.
func bankText(b HouseBank) string { return b.Code + " " + b.Account }

// headerBank is a statement's house bank: its code and masked account,
// the account alone when the bank could not be named.
func headerBank(code, account string) string {
	if code == "" {
		return account
	}
	return code + " " + account
}

// space is a space between two parts of one text (a word and a code),
// which templ writes none of between components.
func space() templ.Component { return Value(" ") }

// lineLink is a link to a line of the page.
func lineLink(no int32) templ.Attributes {
	return templ.Attributes{"href": "#line-" + strconv.Itoa(int(no))}
}
