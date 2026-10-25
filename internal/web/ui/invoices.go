package ui

import (
	"context"
	"net/url"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// InvoicesPath is the supplier invoices' path (T73e).
const InvoicesPath = "/invoices"

// Supplier is an invoice's supplier as the API's expansion names it: its
// number and name, both "" when it did not.
type Supplier struct {
	ID, Number, Name string
}

// supplierText is a supplier's name and number, or its id when the API's
// expansion named it not.
func supplierText(s Supplier) string {
	if s.Name == "" {
		return s.ID
	}
	return s.Name + " (" + s.Number + ")"
}

// InvoiceRow is one invoice of the queue. Its texts are data: the
// supplier's reference and name may come from a captured document.
type InvoiceRow struct {
	Path       string // the invoice's own page
	Number     string
	Reference  string
	Supplier   Supplier
	FilterPath string // the queue of this supplier's invoices alone
	Company    string // "" when the company could not be named
	Date       time.Time
	Gross      Money
	Status     Label
	Kind       Label
	HoldReason string
	Agent      *Name // the agent that entered it (a capture agent's proposal); nil for a person
}

// Option is one choice of a select.
type Option struct {
	Value, Text string
	Selected    bool
}

// StatusTab is one status of the queue, a link keeping the other filters.
type StatusTab struct {
	Label   i18n.Key
	Path    string
	Current bool
}

// InvoiceFilters are the queue's filters as the person chose them (the
// API's own filters, ADR-0052 §8), and what is wrong with a date.
type InvoiceFilters struct {
	Status        string
	Supplier      Supplier // its ID "" for every supplier
	SupplierClear string   // the queue without the supplier filter
	Number        string
	From, To      string // as typed, year-month-day
	FromError     i18n.Key
	ToError       i18n.Key
}

// statusOnly is the query of the queue in status, without the filters.
func statusOnly(status string) url.Values { return url.Values{"status": {status}} }

// InvoiceList is the queue: the status tabs, the filters, a page of
// invoices with the next page's path ("" on the last), and the API's
// refusal to list them, or a filter in the address that is not valid.
type InvoiceList struct {
	Tabs      []StatusTab
	Filters   InvoiceFilters
	Companies []Option // nil: the companies could not be listed, so no choice is offered
	Rows      []InvoiceRow
	More      string
	Refusal   *Refusal
	Invalid   bool
}

// QueuePath is the queue's path with the query, "" values left out.
func QueuePath(q url.Values) string { return listPath(InvoicesPath, q) }

// InvoiceHeader is an invoice's summary.
type InvoiceHeader struct {
	Supplier   Supplier
	Reference  string
	Company    string // "" when it could not be named
	Kind       Label
	Status     Label
	HoldReason string
	Date       time.Time
	PostingDay *time.Time
	TaxDay     *time.Time
	Gross, Tax Money
	Source     Label
	EnteredBy  Name
	Exception  string // the exception's approval request, "" for none
	NoOrder    bool   // the invoice is on accounts, not on a purchase order
}

// invoiceRows are an invoice's summary rows.
func invoiceRows(h InvoiceHeader) []Row {
	rows := []Row{{Key: i18n.InvoiceStatus, Value: InvoiceStatusTag(h.Status)}}
	if h.HoldReason != "" {
		rows = append(rows, Row{Key: i18n.InvoiceHold, Value: Data(h.HoldReason)})
	}
	rows = append(rows, Row{Key: i18n.InvoiceSupplier, Value: SupplierName(h.Supplier)},
		Row{Key: i18n.InvoiceReference, Value: Data(h.Reference)})
	if h.Company != "" {
		rows = append(rows, Row{Key: i18n.InvoiceCompany, Value: Data(h.Company)})
	}
	rows = append(rows, Row{Key: i18n.InvoiceKind, Value: labelText(h.Kind)}, Row{Key: i18n.InvoiceDate, Value: Day(h.Date)})
	if h.NoOrder {
		rows = append(rows, Row{Key: i18n.InvoiceOrder, Value: T(i18n.InvoiceNoOrder)})
	}
	if h.PostingDay != nil {
		rows = append(rows, Row{Key: i18n.InvoicePosting, Value: Day(*h.PostingDay)})
	}
	if h.TaxDay != nil {
		rows = append(rows, Row{Key: i18n.InvoiceTaxDate, Value: Day(*h.TaxDay)})
	}
	rows = append(rows, Row{Key: i18n.InvoiceGross, Value: Amount(h.Gross)}, Row{Key: i18n.InvoiceTax, Value: Amount(h.Tax)},
		Row{Key: i18n.InvoiceSource, Value: labelText(h.Source)}, Row{Key: i18n.InvoiceEnteredBy, Value: Principal(h.EnteredBy)})
	if h.Exception != "" {
		rows = append(rows, Row{Key: i18n.InvoiceException, Value: ButtonLink(h.Exception, i18n.InvoiceExceptionOpen)})
	}
	return rows
}

// Line is one invoice line.
type Line struct {
	No          int32
	OrderItem   int32 // 0: not on an order item
	Description string
	Quantity    string
	Unit        string
	Amount      Money
	TaxCode     string // "" for none
}

// Issue is one issue of a match: its code's words, the invoice line it
// concerns (0: none), and the API's explanation, shown as data.
type Issue struct {
	Label   Label
	Line    int32
	Message string
}

// MatchView is an invoice's latest match.
type MatchView struct {
	When   time.Time
	By     Name
	Config int32
	Clean  bool
	Issues []Issue
}

// RecommendationView is one agent's recommendation on the invoice, with
// the form to decide it while it is open and the person may.
type RecommendationView struct {
	Kind      Label
	Content   string // the agent's text: untrusted (ADR-0052 §7)
	Agent     Name
	At        time.Time
	Outcome   Label
	DecidedBy *Name
	Decide    *RecommendationForm
}

// RecommendationForm decides a recommendation: its path, ETag and a fresh
// idempotency key, what the person chose and wrote, and what is wrong.
type RecommendationForm struct {
	Action       string
	ID           string // unique on the page, for the fields' ids
	CSRF         string
	ETag         string
	Key          string
	Outcome      string
	Comment      string
	OutcomeError i18n.Key
	CommentError i18n.Key
}

// RecommendationOutcomes are the outcomes a person may record.
var RecommendationOutcomes = []Option{{Value: "accepted"}, {Value: "overridden"}, {Value: "ignored"}}

// outcomeText is an outcome's choice in the decision form.
var outcomeText = map[string]i18n.Key{"accepted": i18n.RecDecideAccepted, "overridden": i18n.RecDecideOverridden, "ignored": i18n.RecDecideIgnored}

// fieldsetAttrs names the outcome's error, when it has one.
func fieldsetAttrs(f RecommendationForm) templ.Attributes {
	if f.OutcomeError == "" {
		return templ.Attributes{}
	}
	return templ.Attributes{"aria-describedby": f.ID + "-outcome-error"}
}

// The actions a person may take on an invoice, by the verb of their route.
const (
	ActMatch   = "match"
	ActPost    = "post"
	ActHold    = "hold"
	ActRelease = "release"
	ActCancel  = "cancel"
	ActReverse = "reverse"
)

// ActionForm is one action's form (ADR-0052 §7): the invoice's ETag as
// If-Match and a fresh idempotency key, its text (a reason or a comment)
// and date as typed, and what is wrong with them.
type ActionForm struct {
	Verb      string
	Action    string // the path it posts to
	CSRF      string
	ETag      string
	Key       string
	Text      string
	Date      string
	TextError i18n.Key
	DateError i18n.Key
}

// actionSpec is what an action's form asks: its button and hint, its text
// field (a reason when required) and its date field, "" for none.
type actionSpec struct {
	Button, Hint   i18n.Key
	Text, TextHint i18n.Key
	Required       bool
	Date, DateHint i18n.Key
	Warning        bool // it stops or undoes the invoice
}

var actionSpecs = map[string]actionSpec{
	ActMatch:   {Button: i18n.ActionMatch, Hint: i18n.ActionMatchHint},
	ActPost:    {Button: i18n.ActionPost, Date: i18n.ActionPostDate, DateHint: i18n.ActionPostDateHint},
	ActHold:    {Button: i18n.ActionHold, Text: i18n.ActionHoldReason, TextHint: i18n.ActionReasonHint, Required: true, Warning: true},
	ActRelease: {Button: i18n.ActionRelease, Text: i18n.ActionReleaseComment, TextHint: i18n.ActionCommentHint},
	ActCancel:  {Button: i18n.ActionCancel, Text: i18n.ActionCancelReason, TextHint: i18n.ActionReasonHint, Required: true, Warning: true},
	ActReverse: {Button: i18n.ActionReverse, Text: i18n.ActionReverseReason, TextHint: i18n.ActionReasonHint, Required: true, Warning: true,
		Date: i18n.ActionReverseDate, DateHint: i18n.ActionReverseDateHint},
}

// Requires reports whether the action's text is a required reason.
func Requires(verb string) bool { return actionSpecs[verb].Required }

// buttonClass is an action's button: a warning for one that stops or
// undoes the invoice.
func buttonClass(verb string) templ.Attributes {
	if actionSpecs[verb].Warning {
		return templ.Attributes{"class": "button button--warning", "type": "submit"}
	}
	return templ.Attributes{"class": "button", "type": "submit"}
}

// InvoicePage is one invoice: its summary and lines, the latest match,
// the document beside it, the agents' recommendations, the person's
// actions or why there are none, a notice of what was just done and the
// API's refusal of what was asked.
type InvoicePage struct {
	Number     string
	Header     InvoiceHeader
	AgentMade  bool // an agent entered it: a proposal for a person to check
	Lines      []Line
	HasOrder   bool
	Match      *MatchView
	MatchError *Refusal
	Order      string // the order section's path; "" without an order or the permissions to read it
	Document   string // the document's path; "" without one
	Recs       []RecommendationView
	RecsShown  bool // the person may read recommendations
	RecsError  *Refusal
	RecsWhy    i18n.Key // why the open recommendations offer no decision
	Actions    []ActionForm
	Why        i18n.Key // why Actions is empty
	Pending    bool     // an exception's request is linked to the invoice on hold: waiting, or approved before a hold by hand
	Done       i18n.Key
	Refusal    *Refusal
}

// OrderHeader is a purchase order's summary.
type OrderHeader struct {
	Number string
	Status Label
	Date   time.Time
	Total  Money
}

// orderRows are an order's summary rows.
func orderRows(h OrderHeader) []Row {
	return []Row{{Key: i18n.OrderNumber, Value: Code(h.Number)}, {Key: i18n.OrderStatus, Value: labelText(h.Status)},
		{Key: i18n.OrderDate, Value: Day(h.Date)}, {Key: i18n.OrderTotal, Value: Amount(h.Total)}}
}

// OrderView is an invoice's purchase order and its goods receipts, or the
// API's refusal to show them.
type OrderView struct {
	Header   OrderHeader
	Items    []OrderItemView
	Receipts []ReceiptView
	Refusal  *Refusal
	Back     string // the invoice's page
}

// OrderItemView is one order item.
type OrderItemView struct {
	No          int32
	Description string
	Ordered     string
	Received    string
	Unit        string
	Price       Money
	TaxCode     string
}

// ReceiptView is one goods receipt item of the order.
type ReceiptView struct {
	Number    string
	Date      time.Time
	OrderItem int32
	Quantity  string
	Unit      string
}

// FieldDate is the date field of the invoice forms; their text is
// FieldComment, their choice FieldOutcome.
const (
	FieldDate    = "date"
	FieldOutcome = "outcome"
)

// get is a GET form's attributes.
func get(path string) templ.Attributes {
	return templ.Attributes{"method": "get", "action": string(templ.URL(path))}
}

// post is a form posting to path.
func post(path string) templ.Attributes {
	return templ.Attributes{"method": "post", "action": string(templ.URL(path))}
}

// inputAttrs are an input's attributes: its id, name, type and value, its
// hint and error by id, and aria-invalid with an error.
func inputAttrs(id, name, kind, value string, err i18n.Key) templ.Attributes {
	a := templ.Attributes{"id": id, "name": name, "type": kind, "value": value, "aria-describedby": id + "-hint"}
	if err != "" {
		a["aria-describedby"], a["aria-invalid"] = id+"-hint "+id+"-error", "true"
	}
	return a
}

// option is a select's option.
func option(o Option) templ.Attributes {
	a := templ.Attributes{"value": o.Value}
	if o.Selected {
		a["selected"] = true
	}
	return a
}

// radio is one choice of the outcome.
func radio(id, value, chosen string) templ.Attributes {
	a := templ.Attributes{"type": "radio", "id": id, "name": FieldOutcome, "value": value}
	if value == chosen {
		a["checked"] = true
	}
	return a
}

// tab is a status tab's link, marked as the current one.
func tab(t StatusTab) templ.Attributes {
	a := templ.Attributes{"href": string(templ.URL(t.Path)), "class": "tabs__link"}
	if t.Current {
		a["aria-current"] = "page"
	}
	return a
}

// lazy has htmx load the order section in place of its link once the
// page loaded; the link is the section for a browser without htmx
// (ADR-0052 §8).
func lazy(path string) templ.Attributes {
	return templ.Attributes{"hx-get": string(templ.URL(path)), "hx-trigger": "load", "hx-swap": "outerHTML"}
}

// frame is the document's frame: sandboxed, so nothing in it runs or
// reaches the workspace, besides the document's own sandboxing policy
// (ADR-0052 §7).
func frame(ctx context.Context, path string) templ.Attributes {
	return templ.Attributes{"src": string(templ.URL(path)), "sandbox": "", "title": i18n.T(ctx, i18n.DocumentFrame)}
}

// invoiceStatusKind colours an invoice's status; the text says it.
func invoiceStatusKind(l Label) string {
	switch l.Key {
	case i18n.InvoiceStatusPosted:
		return "success"
	case i18n.InvoiceStatusOnHold:
		return "warning"
	case i18n.InvoiceStatusCancelled, i18n.InvoiceStatusReversed, i18n.InvoiceStatusRejected:
		return "inactive"
	default:
		return ""
	}
}

// outcomeTag colours a recommendation's outcome.
func outcomeTag(l Label) string {
	switch l.Key {
	case i18n.RecAccepted:
		return "success"
	case i18n.RecOpen:
		return ""
	default:
		return "inactive"
	}
}

// dayAttr is a business date's datetime.
func dayAttr(t time.Time) templ.Attributes {
	return templ.Attributes{"datetime": t.Format(time.DateOnly)}
}

// isoDay is a business date (format: date), as ISO 8601 until T73l.
func isoDay(t time.Time) templ.Component { return Value(t.Format(time.DateOnly)) }

// quantity is a quantity in the request's locale with its unit; outside a
// request with a locale, as the API gives it.
func quantity(value, unit string) templ.Component {
	return Text(func(ctx context.Context) string { return decimal(ctx, value) + " " + unit })
}
