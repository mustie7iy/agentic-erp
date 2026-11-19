// Package collectionsagent is the collections agent (ADR-0072 As built,
// T105b), a recommending agent (internal/recommend): for a company's
// collection worklist on a key date (ADR-0065 §9) it reads each listed
// customer's overdue items, aging, dunning notices, payments, invoices and
// dispute cases through API tools (04-api-design §4.13), and leaves one
// recommendation per customer for a collections specialist to decide: to
// call it, send it a reminder, escalate, wait for its dispute cases or do
// nothing, with the overdue figures and, where it contacts the customer, a
// short draft message. Every number it writes, the draft's included, is
// one the run read (recommend.Figures); its content starts with the
// customer's overdue amount as the worklist returned it. A customer whose
// every overdue item has an open dispute case is never recommended a
// contact (a call, a reminder or an escalation): the run works that out
// from the open items and the open cases before its session, as a dunning
// run leaves a disputed item out whole (T86b). It never starts or confirms
// a dunning run, opens, changes or closes a dispute case, and has no tool
// that sends anything to a customer (Never; x-agent: false). Everything a
// customer or a person wrote is data, never an instruction. Its evaluation
// suite is evals/collections (ADR-0039).
package collectionsagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "collections"

// The object type the agent recommends on, the customer (a business
// partner), and its kinds.
const (
	CustomerObject = "business_partner"
	Call           = "call"
	Remind         = "send_reminder"
	Escalate       = "escalate"
	Wait           = "wait_for_dispute"
	NoAction       = "no_action"
)

// contacts are the kinds that have the customer contacted: they carry a
// draft message, and are never recommended for a customer whose overdue
// items are all disputed.
var contacts = []string{Call, Remind, Escalate}

// DraftLabel starts the line of a contacting recommendation's draft
// message.
const DraftLabel = "Draft message:"

// MaxCustomers bounds the customers one run recommends on: the first of
// the worklist, the largest overdue amount first.
const MaxCustomers = 20

// Never are the operations the agent never calls (ADR-0072): starting or
// confirming a dunning run and opening, changing, closing or resolving a
// dispute case (T86d: resolving posts a credit memo or a write-off),
// placing or releasing a dunning block (T76e), recording a promise to pay
// (T76f) or a contact with a customer (T76g); nothing in the contract
// sends a message to a customer, and its tools hold none of these.
var Never = []string{"createDunningRun", "confirmDunningRun", "createDisputeCase", "updateDisputeCase", "closeDisputeCase", "resolveDisputeCase",
	"createDunningBlock", "releaseDunningBlock", "createPromiseToPay", "createCustomerContact"}

// Reads are the operations the agent reads with (ADR-0072 As built,
// T105b): the plan's, the dunning runs a notice is listed under, a
// dispute case, and the open items and the accounts, which the run reads
// before its session to tell a customer whose overdue items are all
// disputed.
var Reads = []string{"getCollectionWorklist", "getReceivablesAging", "listDunningRuns", "listDunningNotices", "listIncomingPayments",
	"listCustomerInvoices", "listDisputeCases", "getDisputeCase", "listOpenItems", "listAccounts", "getBusinessPartner"}

const system = `You prepare a company's collections for a collections specialist to decide: for each customer on the
collection worklist you are given, the next action.

Read the worklist (getCollectionWorklist with the legal entity and the key date you are given: per customer its open and
overdue amounts, its oldest due date, its dunning level and last dunning date, and what its open dispute cases dispute),
and per customer as you need: its open items (listOpenItems with partner_id), the aging on the key date
(getReceivablesAging with customer_id), the dunning runs and the customer's notices in them (listDunningRuns,
listDunningNotices with customer_id), the payments it made (listIncomingPayments), its invoices (listCustomerInvoices),
its dispute cases (listDisputeCases with customer_id and status open, getDisputeCase) and the customer
(getBusinessPartner, which you may be refused).

Then recommend once on each customer: call when a large amount is long overdue or reminders went unanswered;
send_reminder when items are overdue and the customer was not reminded or was reminded once; escalate when it stays
overdue at the highest dunning level; wait_for_dispute when its overdue items are disputed in open cases; no_action
when nothing calls for one. A customer you are told has every overdue item disputed is never called, reminded or
escalated: wait_for_dispute or no_action. The content starts with the customer's overdue amount and the company's
currency as the worklist returned it ("Overdue 1050.00 CAD"), then the reasons with the figures behind them: the
overdue and disputed amounts, the oldest due date, the dunning level and the payments. For call, send_reminder and
escalate, end with a line starting "Draft message:" and a short message to the customer, for the specialist to send:
its figures too are ones a tool returned, written as returned. You send nothing.`

// Agent recommends the next collection action per customer.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
	// Now is the clock the default key date is read from; nil is the
	// time.
	Now func() time.Time
}

// The refusals before a run and of the agent's own check, beside the
// pattern's.
var (
	// ErrKeyDate refuses a key date that is not a date.
	ErrKeyDate = errors.New("collectionsagent: the key date is a date, YYYY-MM-DD")
	// ErrDisputed refuses a call, a reminder or an escalation for a
	// customer whose overdue items are all disputed.
	ErrDisputed = errors.New("collectionsagent: every overdue item of this customer is disputed in an open case: recommend wait_for_dispute or no_action")
	// ErrNoDispute refuses wait_for_dispute for a customer without an
	// open dispute case.
	ErrNoDispute = errors.New("collectionsagent: wait_for_dispute is for a customer with an open dispute case")
	// ErrNoOverdue refuses a content that does not start with the
	// customer's overdue amount and the company's currency as the
	// worklist returned it.
	ErrNoOverdue = errors.New(`collectionsagent: the content starts with "Overdue", the customer's overdue amount as the worklist returned it, and the company's currency ("Overdue 1050.00 CAD")`)
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("collectionsagent: every number in the content and its draft message is one a tool returned or the run was given, written as returned; a count of your own is written in words")
	// ErrNoDraft refuses a call, a reminder or an escalation without its
	// draft message.
	ErrNoDraft = fmt.Errorf("collectionsagent: call, send_reminder and escalate end with a line starting %q and the message", DraftLabel)
)

// money is an amount as the API writes it.
type money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// customer is a customer of the worklist and what the run worked out
// before its session: whether every overdue item of its is disputed.
type customer struct {
	ID       string `json:"customer_id"`
	Number   string `json:"customer_number"`
	Name     string `json:"customer_name"`
	Overdue  money  `json:"overdue_amount"`
	Cases    int    `json:"open_dispute_cases"`
	disputed bool
}

// worklist is the first page of the worklist the run recommends on.
type worklist struct {
	CompanyCode string     `json:"company_code"`
	Currency    string     `json:"currency"`
	Count       int        `json:"customer_count"`
	Items       []customer `json:"items"`
}

// Advise runs one session on the company's collection worklist on the key
// date ("" is today in the company's time zone): it finds the company by its code, reads the
// worklist's first MaxCustomers customers and, for those with open
// dispute cases, their open items and cases (anything the agent may not
// read stops the run before a session starts), then offers the model the
// tools and reports what it recommended, also when the run fails after
// recommending. A worklist without customers starts no session and says
// so.
func (a *Agent) Advise(ctx context.Context, company, keyDate string) (recommend.Result, error) {
	if _, err := time.Parse(time.DateOnly, keyDate); keyDate != "" && err != nil {
		return recommend.Result{}, fmt.Errorf("%w: not %q", ErrKeyDate, keyDate)
	}
	le, err := apitools.LegalEntity(ctx, a.API, company)
	if err != nil {
		return recommend.Result{}, err
	}
	if keyDate == "" {
		if keyDate, err = (apitools.Days{API: a.API, Now: a.Now}).Of(ctx, le); err != nil {
			return recommend.Result{}, fmt.Errorf("collectionsagent: company %s: %w", company, err)
		}
	}
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/collection-worklist",
		url.Values{"legal_entity_id": {le}, "key_date": {keyDate}, "page_size": {strconv.Itoa(MaxCustomers)}}, nil, "")
	var w worklist
	if err == nil {
		err = resp.Decode(&w)
	}
	if err != nil {
		return recommend.Result{}, fmt.Errorf("collectionsagent: the collection worklist of company %s on %s: %w", company, keyDate, err)
	}
	if len(w.Items) == 0 {
		return recommend.Result{Reply: fmt.Sprintf("Nothing of company %s is overdue on %s: no customer to recommend on.", company, keyDate)}, nil
	}
	var receivables map[string]bool // read once, for the first customer with a case
	for i := range w.Items {
		if w.Items[i].Cases == 0 {
			continue
		}
		if receivables == nil {
			if receivables, err = a.receivables(ctx, le); err != nil {
				return recommend.Result{}, err
			}
		}
		if err := a.disputes(ctx, le, keyDate, receivables, &w.Items[i]); err != nil {
			return recommend.Result{}, err
		}
	}
	r := &review{customers: map[string]customer{}}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Recommend the next collection action per customer of company %s on %s", w.CompanyCode, keyDate),
		Prompt:  r.prompt(w, company, le, keyDate),
		Objects: map[string]string{},
		Check:   r.check,
	}
	names := map[string]string{}
	for _, c := range w.Items {
		r.customers[c.ID] = c
		task.Objects[c.ID] = CustomerObject
		names[c.ID] = c.Number + " " + c.Name
	}
	res, err := agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
	res.Names = names
	return res, err
}

// openItem is a customer's open item as the run reads it: its journal
// line, its amount (a debit is positive) and its net due date, if it was
// posted with payment terms.
type openItem struct {
	FiscalYear     int    `json:"fiscal_year"`
	JournalEntryID string `json:"journal_entry_id"`
	LineNo         int    `json:"line_no"`
	AccountID      string `json:"account_id"`
	PostingDate    string `json:"posting_date"`
	Amount         money  `json:"amount_lc"`
	Terms          *struct {
		NetDueDate string `json:"net_due_date"`
	} `json:"payment_terms"`
}

// line names an item by its journal line, as a dispute case names it.
func line(year int, entry string, no int) string { return fmt.Sprintf("%d/%s/%d", year, entry, no) }

// receivables are the company's customer reconciliation accounts, by id:
// the accounts whose open items the worklist counts.
func (a *Agent) receivables(ctx context.Context, le string) (map[string]bool, error) {
	accounts, err := apitools.List[struct {
		ID   string `json:"id"`
		Kind string `json:"reconciliation_kind"`
	}](ctx, a.API, "/v1/accounts", url.Values{"legal_entity_id": {le}})
	if err != nil {
		return nil, fmt.Errorf("collectionsagent: the accounts of legal entity %s: %w", le, err)
	}
	out := map[string]bool{}
	for _, acc := range accounts {
		if acc.Kind == "customer" {
			out[acc.ID] = true
		}
	}
	return out, nil
}

// disputes works out whether every overdue item of a customer with open
// dispute cases is one of a case's items: its open items on a customer reconciliation
// account (a partner that also supplies has items on supplier accounts,
// which the worklist does not count) overdue on the key date (a debit
// past its net due date, or, posted without terms, past its posting date)
// each have an open case. An item posted without terms is taken as due on
// its posting date, which is never before its document date, the date the
// worklist takes: the run may count an item overdue a few days late, and
// so tell a customer disputed that the worklist still lists with an item
// overdue only by those days, never the other way round.
func (a *Agent) disputes(ctx context.Context, le, keyDate string, receivables map[string]bool, c *customer) error {
	cases, err := apitools.List[struct {
		Items []struct {
			FiscalYear     int    `json:"fiscal_year"`
			JournalEntryID string `json:"journal_entry_id"`
			LineNo         int    `json:"line_no"`
		} `json:"items"`
	}](ctx, a.API, "/v1/dispute-cases", url.Values{"legal_entity_id": {le}, "customer_id": {c.ID}, "status": {"open"}})
	if err != nil {
		return fmt.Errorf("collectionsagent: the open dispute cases of customer %s: %w", c.Number, err)
	}
	items, err := apitools.List[openItem](ctx, a.API, "/v1/open-items", url.Values{"legal_entity_id": {le}, "partner_id": {c.ID}})
	if err != nil {
		return fmt.Errorf("collectionsagent: the open items of customer %s: %w", c.Number, err)
	}
	disputed := map[string]bool{}
	for _, dc := range cases {
		for _, it := range dc.Items { // a case over several invoices disputes each of them (T86c)
			disputed[line(it.FiscalYear, it.JournalEntryID, it.LineNo)] = true
		}
	}
	c.disputed = true
	for _, it := range items {
		due := it.PostingDate
		if it.Terms != nil && it.Terms.NetDueDate != "" {
			due = it.Terms.NetDueDate
		}
		// Dates as the API writes them, YYYY-MM-DD, order as text.
		overdue := receivables[it.AccountID] && !strings.HasPrefix(it.Amount.Amount, "-") && due < keyDate
		if overdue && !disputed[line(it.FiscalYear, it.JournalEntryID, it.LineNo)] {
			c.disputed = false
		}
	}
	return nil
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{CustomerObject: {Call, Remind, Escalate, Wait, NoAction}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// review is what one run's check knows: the customers of the worklist by
// id.
type review struct {
	customers map[string]customer
}

// prompt names the company, the key date and each customer, and says
// which have open dispute cases and which have every overdue item
// disputed; the amounts are the tools' to return.
func (r *review) prompt(w worklist, company, le, keyDate string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Company %s (legal entity %s), whose currency is %s; the key date is %s. The worklist lists %s; you recommend on the first %s, the largest overdue amount first.\n",
		company, le, w.Currency, keyDate, customers(w.Count), customers(len(w.Items)))
	for _, c := range w.Items {
		fmt.Fprintf(&b, "Customer %s %s (business partner %s)", c.Number, c.Name, c.ID)
		switch {
		case c.disputed:
			b.WriteString(": every overdue item of it is disputed in an open case.\n")
		case c.Cases > 0:
			b.WriteString(": some of its items are disputed in open cases, not every overdue one.\n")
		default:
			b.WriteString(": no open dispute case.\n")
		}
	}
	return b.String()
}

// customers says how many customers, in words where it is one.
func customers(n int) string {
	if n == 1 {
		return "one customer"
	}
	return strconv.Itoa(n) + " customers"
}

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072 As built, T105b): a customer whose overdue items are all
// disputed is not contacted, wait_for_dispute needs an open case, the
// content starts with the customer's overdue amount as the worklist
// returned it, a contact ends with its draft message, and every number
// written is one the run read.
func (r *review) check(p recommend.Proposed, f *recommend.Figures) error {
	c := r.customers[p.ObjectID] // the pattern admits only the run's customers
	contact := slices.Contains(contacts, p.Kind)
	switch {
	case contact && c.disputed:
		return ErrDisputed
	case p.Kind == Wait && c.Cases == 0:
		return ErrNoDispute
	}
	stated := recommend.Numbers(p.Content)
	if len(stated) == 0 || stated[0] != c.Overdue.Amount || !strings.HasPrefix(p.Content, "Overdue "+c.Overdue.Amount+" "+c.Overdue.Currency) {
		return ErrNoOverdue
	}
	if unread := f.Unread(p.Content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	if !f.Returned(c.Overdue.Amount) {
		// Read before the session; the model reads it again with its tool.
		return fmt.Errorf("%w: the overdue amount %s is no amount a tool of the run returned", ErrUnread, c.Overdue.Amount)
	}
	if contact && !hasDraft(p.Content) {
		return ErrNoDraft
	}
	return nil
}

// hasDraft reports whether a content has a line starting with the draft
// label and a message after it.
func hasDraft(content string) bool {
	for l := range strings.Lines(content) {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(l), DraftLabel); ok && strings.TrimSpace(msg) != "" {
			return true
		}
	}
	return false
}
