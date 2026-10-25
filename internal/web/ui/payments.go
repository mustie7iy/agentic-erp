package ui

import (
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// PaymentsPath is the payment runs' path (T73f): the runs by status, the
// proposal of a run, a run's payments, exclusions and bank files, its
// release or rejection, and its operations' progress.
const PaymentsPath = "/payments"

// ProposePath is where a run's proposal is posted: under the list, so a
// sign-in again comes back to it (its path's parent).
const ProposePath = PaymentsPath + "/propose"

// RunsPath is the runs' list with the query, "" values left out.
func RunsPath(q url.Values) string { return listPath(PaymentsPath, q) }

// listPath is a list's path with the query, "" values left out.
func listPath(base string, q url.Values) string {
	for k, v := range q {
		if len(v) == 0 || v[0] == "" {
			delete(q, k)
		}
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// RunRow is one payment run of the list.
type RunRow struct {
	Path       string
	Number     string
	Status     Label
	Company    string // "" when it could not be named
	Bank       string // the house bank's code; "" for a run whose suppliers' payment methods choose the bank
	RunDate    time.Time
	DueBy      time.Time
	Total      Money
	ProposedBy Name
}

// RunFilters are the list's filters as chosen: the status tab, the
// company and the document number.
type RunFilters struct {
	Status    string
	Number    string
	Companies []Option // nil: the companies could not be listed, so no choice is offered
}

// ProposeForm proposes a payment run (proposePaymentRun): the company, a
// house bank or none (the suppliers' payment methods then choose one per
// payment, in the currency given), the payment date and the due date
// the items are paid up to, a fresh idempotency key, and what is wrong.
type ProposeForm struct {
	CSRF          string
	Key           string
	Companies     []Option
	Banks         []Option // nil: the house banks could not be listed, so none is offered
	Currency      string
	RunDate       string
	DueBy         string
	CompanyError  i18n.Key
	RunDateError  i18n.Key
	DueByError    i18n.Key
	CurrencyError i18n.Key
}

// The propose form's fields; the company is FieldCompany, the house bank
// FieldHouseBank.
const (
	FieldCompany  = "legal_entity_id"
	FieldCurrency = "currency"
	FieldRunDate  = "run_date"
	FieldDueBy    = "due_by"
)

// RunList is the runs' list: the status tabs, the filters, a page of
// runs with the next page's path, the propose form for a person who may
// propose, and the API's refusals.
type RunList struct {
	Tabs           []StatusTab
	Filters        RunFilters
	Rows           []RunRow
	More           string
	Propose        *ProposeForm
	Refusal        *Refusal
	ProposeRefusal *Refusal
	Invalid        bool
}

// RunHeader is a run's summary.
type RunHeader struct {
	Status     Label
	RunDate    time.Time
	DueBy      time.Time
	Total      Money
	ProposedBy Name
	DecidedBy  *Name
	DecidedAt  *time.Time
}

// runRows are a run's summary rows.
func runRows(h RunHeader) []Row {
	rows := []Row{{Key: i18n.RunStatus, Value: RunStatusTag(h.Status)}, {Key: i18n.RunDate, Value: Day(h.RunDate)},
		{Key: i18n.RunDueBy, Value: Day(h.DueBy)}, {Key: i18n.RunTotal, Value: Amount(h.Total)}, {Key: i18n.RunProposedBy, Value: Principal(h.ProposedBy)}}
	if h.DecidedBy != nil {
		rows = append(rows, Row{Key: i18n.RunDecidedBy, Value: Principal(*h.DecidedBy)})
	}
	if h.DecidedAt != nil {
		rows = append(rows, Row{Key: i18n.RunDecidedAt, Value: Date(*h.DecidedAt)})
	}
	return rows
}

// ReleaseApproval is a run's request for a second person's approval of
// its release (two-person release, ADR-0038 as amended by T72m), decided
// in the approvals inbox.
type ReleaseApproval struct {
	Status     Label
	Requested  Money
	ApprovedBy *Name
	ApprovedAt *time.Time
	Request    string // the request's page in the inbox
	Pending    bool
}

// approvalFacts are the request's facts.
func approvalFacts(a ReleaseApproval) []Row {
	rows := []Row{{Key: i18n.RunApprovalState, Value: ApprovalStatusTag(a.Status)}, {Key: i18n.RunApprovalTotal, Value: Amount(a.Requested)}}
	if a.ApprovedBy != nil {
		rows = append(rows, Row{Key: i18n.RunApprovalBy, Value: Principal(*a.ApprovedBy)})
	}
	if a.ApprovedAt != nil {
		rows = append(rows, Row{Key: i18n.RunApprovalAt, Value: Date(*a.ApprovedAt)})
	}
	return rows
}

// PaymentView is one payment of a run: to whom (the supplier's id: the
// API names no supplier on a payment), what it pays and how, and whether
// it is posted.
type PaymentView struct {
	Supplier    string
	Amount      Money
	Discount    *Money // nil: no cash discount taken
	Withholding *Money // nil: no withholding tax deducted
	Method      string // "" for a run proposed for one house bank
	EndToEnd    string
	Items       int32
	DownPayment bool
	Posted      bool
}

// ExclusionView is one supplier, or down payment request, a run left out,
// and why.
type ExclusionView struct {
	Supplier    string
	Reason      Label
	Amount      Money
	DownPayment string // the down payment request left out, "" for the supplier's items
}

// BankFileView is one bank file of a released run: its format, payment
// method and transmission status, with its transmission's form while it
// may be sent.
type BankFileView struct {
	Format   string
	Method   string
	Status   Label
	Created  time.Time
	Transmit *RunForm
}

// The decisions on a run, by the verb of their route.
const (
	RunRelease  = "release"
	RunReject   = "reject"
	RunSubmit   = "submit"
	RunTransmit = "transmit"
)

// The run forms' fields besides the decision's own: the bank file a
// transmission is of, and the operation a cancellation is of.
const (
	FieldBankFile  = "bank_file"
	FieldOperation = "operation"
)

// RunForm is one decision on a run (ADR-0052 §7): the run's ETag (a bank
// file's for a transmission) as If-Match, a fresh idempotency key, what
// the person typed and its error.
type RunForm struct {
	Verb      string
	ID        string // unique on the page, for the fields' ids
	Action    string
	CSRF      string
	ETag      string
	Key       string
	File      string // the bank file of a transmission
	Text      string
	TextError i18n.Key
}

// runSpec is what a run's form asks: its button and hint, and its text
// (a reason when required), "" for none.
type runSpec struct {
	Button, Hint   i18n.Key
	Text, TextHint i18n.Key
	Required       bool
	Warning        bool
}

var runSpecs = map[string]runSpec{
	RunRelease:  {Button: i18n.RunReleaseButton, Hint: i18n.RunReleaseHint, Text: i18n.ActionReleaseComment, TextHint: i18n.ActionCommentHint},
	RunReject:   {Button: i18n.RunRejectButton, Hint: i18n.RunRejectHint, Text: i18n.RunRejectReason, TextHint: i18n.ActionReasonHint, Required: true, Warning: true},
	RunSubmit:   {Button: i18n.RunSubmitButton, Hint: i18n.RunSubmitHint},
	RunTransmit: {Button: i18n.FileTransmit, Hint: i18n.FileTransmitHint},
}

// RunRequires reports whether the decision's text is a required reason.
func RunRequires(verb string) bool { return runSpecs[verb].Required }

// runButton is a decision's button, a warning for one that turns the run
// down, named by id so focus stays on it across a swap.
func runButton(f RunForm) templ.Attributes {
	a := templ.Attributes{"class": "button", "type": "submit", "id": f.ID + "-button"}
	if runSpecs[f.Verb].Warning {
		a["class"] = "button button--warning"
	}
	return a
}

// RunPage is one payment run: its summary, its release's approval, its
// operation's progress, its payments, exclusions and bank files, the
// person's decisions or why there are none, a notice of what was just
// done and the API's refusal of what was asked.
type RunPage struct {
	Number          string
	Header          RunHeader
	Approval        *ReleaseApproval
	Progress        string // the progress section's path; "" when no operation is worth showing
	Building        bool   // the proposal is being built: no payments yet
	Payments        []PaymentView
	PaymentsMore    string
	PaymentsError   *Refusal
	Exclusions      []ExclusionView
	ExclusionsMore  string
	ExclusionsError *Refusal
	FilesShown      bool // the run is released or being released: it has bank files
	Files           []BankFileView
	FilesError      *Refusal
	Actions         []RunForm
	Why             i18n.Key
	Done            i18n.Key
	Refusal         *Refusal
}

// ProgressView is a run's latest operation: its proposal or its release,
// what it has done and of how much, why it stopped, and the person's
// cancellation while it runs or the release's resumption once it stopped.
type ProgressView struct {
	Run     string // the run's page
	Self    string // the progress section's own path
	Kind    Label
	Status  Label
	Done    int64
	Total   *int64 // nil while unknown
	Running bool   // queued or running: the section asks again every two seconds
	Error   *Refusal
	Cancel  *CancelForm
	Resume  *RunForm
	Refusal *Refusal // the API's refusal of the progress, or of a cancellation
}

// CancelForm cancels a run's running operation (cancelOperation): the
// operation by id, the CSRF token and a fresh idempotency key. Its
// If-Match is the operation's version as erp-web reads it when the form
// is sent, since that version moves with every step of progress.
type CancelForm struct {
	Action    string
	CSRF      string
	Key       string
	Operation string
}

// progressBar is the progress element's value and maximum, for an
// operation whose total is known.
func progressBar(v ProgressView) templ.Attributes {
	return templ.Attributes{"value": strconv.FormatInt(v.Done, 10), "max": strconv.FormatInt(*v.Total, 10), "aria-labelledby": "progress-title"}
}

// poll has htmx ask for the operation's status every two seconds and put
// it in place, while it runs; the region is a status, read out as it
// changes (ADR-0052 §8: htmx only replaces fragments).
func poll(self string) templ.Attributes {
	return templ.Attributes{"hx-get": string(templ.URL(self + "?part=status")), "hx-trigger": "every 2s", "hx-swap": "innerHTML"}
}

// runStatusKind colours a run's status; the text says it.
func runStatusKind(l Label) string {
	switch l.Key {
	case i18n.RunStatusReleased:
		return "success"
	case i18n.RunStatusProposing, i18n.RunStatusReleasing:
		return "warning"
	case i18n.RunStatusRejected, i18n.RunStatusFailed:
		return "inactive"
	default:
		return ""
	}
}

// fileStatusKind colours a bank file's status.
func fileStatusKind(l Label) string {
	switch l.Key {
	case i18n.FileStatusTransmitted, i18n.FileStatusAcknowledged:
		return "success"
	case i18n.FileStatusToSend, i18n.FileStatusSending:
		return "warning"
	case i18n.FileStatusFailed, i18n.FileStatusRejected:
		return "inactive"
	default:
		return ""
	}
}

// approvalKind colours the state of a release's approval.
func approvalKind(l Label) string {
	switch l.Key {
	case i18n.ReleaseApprovalApproved:
		return "success"
	case i18n.ReleaseApprovalPending:
		return "warning"
	case i18n.ReleaseApprovalRejected, i18n.ReleaseApprovalWithdrawn:
		return "inactive"
	default:
		return ""
	}
}

// operationKind colours an operation's state.
func operationKind(l Label) string {
	switch l.Key {
	case i18n.OperationSucceeded:
		return "success"
	case i18n.OperationQueued, i18n.OperationRunning:
		return "warning"
	case i18n.OperationFailed, i18n.OperationCancelled:
		return "inactive"
	default:
		return ""
	}
}
