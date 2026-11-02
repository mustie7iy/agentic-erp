package ui

import (
	"context"
	"net/url"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// Name is a principal as listPrincipalNames named it (ADR-0052 G3): its
// display name ("" when the API named none: an id of no principal the
// person may know of), and whether it is an agent, which the page badges.
type Name struct {
	Text  string
	Agent bool
}

// Label is an open enumeration's value (ADR-0052 §7): Key when the catalog
// names it, else Raw, shown as it is, never as an error.
type Label struct {
	Key i18n.Key
	Raw string
}

// Money is an amount as the API gives it: a decimal string with the
// currency's minor digits, and its ISO 4217 code.
type Money struct{ Amount, Currency string }

// Approval is an approval request as the inbox and its page show it.
type Approval struct {
	Path      string // the request's own page
	Title     string // the API's title: data, perhaps an agent's text
	Kind      Label  // the object's type
	Status    Label
	Amount    *Money // nil when no amount chose the tier
	Requester Name
	Requested time.Time
	Step      int32 // the current step
	StepCount int32
	ObjectID  string
}

// statusKind colours a request's status tag; the text says it.
func statusKind(status Label) string {
	switch status.Key {
	case i18n.StatusApproved:
		return "success"
	case i18n.StatusRejected, i18n.StatusCancelled:
		return "inactive"
	default:
		return ""
	}
}

// Refusal is the API's refusal of what the person asked (ADR-0052 §8):
// the code's message in the person's language, or, for a code the catalog
// does not hold, the API's own title; the code and the trace id for
// support; for a segregation-of-duties refusal, the permissions the
// person holds but may not use (getCurrentPrincipal's barred_permissions);
// and the figures and issues a refusal carries as fields (decision D3).
type Refusal struct {
	ID       string // its element's id, unique on the page; "" for "refusal"
	Title    i18n.Key
	Message  i18n.Key // "" when the catalog does not hold the code
	Fallback string   // the API's title, shown when Message is ""
	Code     string
	Trace    string
	Barred   []string
	Tax      *TaxFigures // payables.tax_mismatch's figures, when the API gave them
	Issues   []Issue     // payables.match_outdated's issues, when the API gave them
}

// The approvals' lists (T73d2), by the view query parameter: the requests
// the person may decide now (the inbox, decidable_by=me), those they asked
// for (requested_by=me) and all they may see.
const (
	ViewDecide = ""
	ViewMine   = "mine"
	ViewAll    = "all"
	FieldView  = "view"
	// FieldStatus and FieldObjectKind are the lists' filters, the API's own
	// (listApprovalRequests' status and object_type).
	FieldStatus     = "status"
	FieldObjectKind = "object_type"
)

// LabelOption is one choice of a select whose text is the catalog's, or
// the value as it is for one the catalog does not name.
type LabelOption struct {
	Value    string
	Label    Label
	Selected bool
}

// ApprovalFilters are a list's filters as the person chose them: the
// status, offered on every list but the inbox (which holds pending
// requests alone), and the kind of object; Clear is the list without them.
type ApprovalFilters struct {
	View     string
	Statuses []LabelOption // nil on the inbox
	Kinds    []LabelOption
	Clear    string
}

// InboxPage is one of the person's lists of approval requests (View): the
// lists as tabs, the filters, the requests, the next page's path ("" on
// the last), a decision just recorded (Done), the permissions a
// segregation-of-duties rule bars, and the API's refusal to list them.
type InboxPage struct {
	View      string
	Tabs      []StatusTab
	Filters   ApprovalFilters
	Approvals []Approval
	More      string
	Done      i18n.Key
	Barred    []string
	Refusal   *Refusal
}

// ViewTitle is the list's title.
func ViewTitle(view string) i18n.Key {
	switch view {
	case ViewMine:
		return i18n.MineTitle
	case ViewAll:
		return i18n.AllTitle
	}
	return i18n.InboxTitle
}

// viewTexts are the list's lead, the text of an empty list and the list's
// name.
func viewTexts(view string) (lead, empty, list i18n.Key) {
	switch view {
	case ViewMine:
		return i18n.MineLead, i18n.MineEmpty, i18n.MineListLabel
	case ViewAll:
		return i18n.AllLead, i18n.AllEmpty, i18n.AllListLabel
	}
	return i18n.InboxLead, i18n.InboxEmpty, i18n.InboxListLabel
}

func leadText(view string) i18n.Key {
	lead, _, _ := viewTexts(view)
	return lead
}

func emptyText(view string) i18n.Key {
	_, empty, _ := viewTexts(view)
	return empty
}

func listText(view string) i18n.Key {
	_, _, list := viewTexts(view)
	return list
}

// ObjectSummary is what the request's object says of itself, read from
// its own operation (getRequisition, getSupplierInvoice, …): its rows,
// and its own page in the workspace, where there is one.
type ObjectSummary struct {
	Rows     []Row
	Link     string
	LinkText i18n.Key
}

// Step is one step of a request.
type Step struct {
	No, Required, Approvals int32
	Current                 bool
}

// Decision is one decision recorded on a request.
type Decision struct {
	Label   Label
	By      Name
	At      time.Time
	Comment string // data, perhaps an agent's text
}

// DecideForms are a request's approve and reject forms, and the
// requester's withdrawal (ADR-0052 §7): the request's ETag, sent as
// If-Match, and a fresh idempotency key each, so a second click or a
// resubmit replays the first answer; what the person typed, kept when the
// API refuses or a step-up sign-in came between; and the fields' errors.
type DecideForms struct {
	Action        string // the request's page; the forms post to its approve, reject and cancel
	CSRF          string // the session's CSRF token
	ETag          string
	ApproveKey    string
	RejectKey     string
	WithdrawKey   string
	Comment       string
	Reason        string
	Withdrawal    string // why the requester withdraws it
	CommentError  i18n.Key
	ReasonError   i18n.Key
	WithdrawError i18n.Key
}

// RequestPage is one request: its summary, its object's own summary, its
// steps and decisions, the person's decision forms or why there are none,
// the requester's withdrawal, and the API's refusal of a decision.
type RequestPage struct {
	Approval
	Object        *ObjectSummary
	ObjectNote    i18n.Key // why Object is nil when ObjectRefusal is too
	ObjectRefusal *Refusal
	Steps         []Step
	Decisions     []Decision
	Decide        *DecideForms
	Why           i18n.Key     // why Decide is nil: DecideOwn, DecideNotYours or DecideClosed
	Withdraw      *DecideForms // the requester's own pending request; nil otherwise
	Kept          bool         // what the person typed came back after a step-up sign-in, not sent yet
	Unsent        string       // what came back after a step-up sign-in for a decision the request no longer takes
	Refusal       *Refusal
}

// The fields of the decision forms.
const (
	FieldIfMatch     = "if_match"
	FieldIdempotency = "idempotency_key"
	FieldComment     = "comment"
)

// The decisions on a request, the last segment of their forms' paths.
const (
	VerbApprove  = "approve"
	VerbReject   = "reject"
	VerbWithdraw = "cancel"
)

// labelOption is a select's option whose text is the catalog's.
func labelOption(o LabelOption) templ.Attributes {
	a := templ.Attributes{"value": o.Value}
	if o.Selected {
		a["selected"] = true
	}
	return a
}

// selectAttrs are a select's id and name.
func selectAttrs(id, name string) templ.Attributes { return templ.Attributes{"id": id, "name": name} }

// labelText is a Label's text.
func labelText(l Label) templ.Component {
	if l.Key == "" {
		return Code(l.Raw)
	}
	return T(l.Key)
}

// amount is an amount in the request's locale (i18n.Locale.Money); outside
// a request with a locale, as the API gives it.
func amount(m Money) templ.Component {
	return Text(func(ctx context.Context) string {
		l, ok := i18n.From(ctx)
		if !ok {
			return m.Amount + " " + m.Currency
		}
		return l.Money(m.Amount, m.Currency)
	})
}

// date is a time's date, as ISO 8601 until T73l formats dates per locale.
func date(t time.Time) templ.Attributes {
	return templ.Attributes{"datetime": t.UTC().Format(time.RFC3339)}
}

// dateText is the date's text.
func dateText(t time.Time) templ.Component { return Value(t.UTC().Format(time.DateOnly)) }

// refusalID is the id of a refusal's title.
func refusalID(r Refusal) string {
	if r.ID == "" {
		return "refusal-title"
	}
	return r.ID + "-title"
}

// approvalRows are a request's summary.
func approvalRows(a Approval) []Row {
	rows := []Row{{Key: i18n.ApprovalKind, Value: labelText(a.Kind)}, {Key: i18n.ApprovalStatus, Value: StatusTag(a.Status)}}
	if a.Amount != nil {
		rows = append(rows, Row{Key: i18n.ApprovalAmount, Value: Amount(*a.Amount)})
	}
	return append(rows, Row{Key: i18n.ApprovalRequester, Value: Principal(a.Requester)},
		Row{Key: i18n.ApprovalRequested, Value: Date(a.Requested)},
		Row{Key: i18n.ApprovalObject, Value: Code(a.ObjectID)})
}

// field is a form field's attributes: its id, its hint and error by id,
// and aria-invalid with an error.
func field(id string, err i18n.Key) templ.Attributes {
	a := templ.Attributes{"id": id, "name": FieldComment, "aria-describedby": id + "-hint"}
	if err != "" {
		a["aria-describedby"], a["aria-invalid"] = id+"-hint "+id+"-error", "true"
	}
	return a
}

// forAttr names a label's field.
func forAttr(id string) templ.Attributes { return templ.Attributes{"for": id} }

// action is a decision form's target.
func action(base, verb string) templ.Attributes {
	return templ.Attributes{"method": "post", "action": string(templ.URL(base + "/" + verb))}
}

// ListPath is a list's path with the query, "" values left out.
func ListPath(base string, q url.Values) string { return listPath(base, q) }
