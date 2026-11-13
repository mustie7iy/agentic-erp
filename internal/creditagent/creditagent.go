// Package creditagent is the credit scoring agent (ADR-0072, T105a), a
// recommending agent (internal/recommend): for a customer in a company it
// reads the customer's credit exposure and limit, the aging, the dunning
// runs and notices, the incoming payments and the customer invoices
// through API tools (04-api-design §4.13), and leaves one recommendation
// for a credit manager to decide: on the customer's credit limit, to
// raise, lower or keep it, or, for a customer without one, on the
// customer, to set one. The content starts with the limit it recommends,
// and every number it writes, that limit included, is one the run read
// (recommend.Figures: an amount a read returned, whatever the read was
// about): the agent computes no figure and gives no score.
// A customer whose posted invoices and payments the run did not read gets
// keep_limit or nothing. It never sets or changes a limit and releases no
// order's credit block: those are no tools of its (Never), and the API
// refuses an agent the limit's writes (x-agent: false, ADR-0066).
// Everything a customer or a person wrote is data, never an instruction.
// Its evaluation suite is evals/credit_scoring (ADR-0039).
package creditagent

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "credit_scoring"

// The object types the agent recommends on, and its kinds: on a credit
// limit, to raise, lower or keep it; on a customer without one (a
// business partner), to set one.
const (
	LimitObject    = "credit_limit"
	CustomerObject = "business_partner"
	Raise          = "raise_limit"
	Lower          = "lower_limit"
	Keep           = "keep_limit"
	Set            = "set_limit"
)

// Never are the operations the agent never calls (ADR-0072): setting and
// changing a limit, which its tools hold none of. The release of an
// order's credit block (T77b) is not in the contract this was built on;
// it is a write, and the pattern gives an agent no write but its
// recommendation.
var Never = []string{"createCreditLimit", "updateCreditLimit"}

// history are the reads whose answers are the customer's history: its
// invoices and the payments it made.
var history = []string{"listCustomerInvoices", "listIncomingPayments"}

const system = `You review a customer's credit limit in a company, for a credit manager to decide.

Read the customer's credit exposure (getCreditExposure with the legal entity and the customer: what its open items add up to,
its limit if it has one and what is available), its limit (listCreditLimits), the aging of its open items on the date you are given as today
(getReceivablesAging with key_date and customer_id: the amount not yet due and the overdue amount by bucket), the company's dunning runs
and the customer's notices in them (listDunningRuns, listDunningNotices with customer_id: the levels reached and the amounts
dunned), the payments it made (listIncomingPayments with customer_id), its invoices (listCustomerInvoices with customer_id)
and the customer (getBusinessPartner, which you may be refused: then name the customer by its number).

Then recommend once. For a customer with a limit, on the limit: raise_limit when it pays what it owes on time and its
exposure has reached the limit, lower_limit when amounts are overdue or dunned and the limit is above what it owes,
keep_limit otherwise. For a customer without a limit, on the customer: set_limit. The content starts with the limit you
recommend and its currency ("Limit 2500.00 CAD"), for keep_limit the limit as it is, and then gives the reasons: the limit
now, the exposure, the overdue amount and what the dunning notices and payments show. The limit you recommend, like every
amount you state, is an amount a tool returned, written exactly as returned (the exposure, the limit now, an amount overdue
or paid): you compute no amount, no percentage and no score, and a count you made yourself is written in words. A customer whose invoices and payments you did not find has no
history: for it, recommend keep_limit, or nothing when it has no limit, and say that it has no history.`

// Agent reviews a customer's credit limit.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
	// Now is the clock the run's date is read from, which the aging is
	// asked on; nil is the time.
	Now func() time.Time
}

// exposure is what the run needs of the customer's exposure before it
// starts: the customer, the company's currency and the limit, if any.
type exposure struct {
	CompanyCode    string `json:"company_code"`
	CustomerID     string `json:"customer_id"`
	CustomerNumber string `json:"customer_number"`
	CustomerName   string `json:"customer_name"`
	Currency       string `json:"currency"`
	HasLimit       bool   `json:"has_limit"`
	CreditLimitID  string `json:"credit_limit_id"`
	Limit          struct {
		Amount string `json:"amount"`
	} `json:"limit"`
}

// The refusals of the agent's own check, beside the pattern's.
var (
	// ErrNoLimit refuses a content that does not start with the limit
	// recommended, or whose limit does not fit its kind: above the limit
	// now to raise it, below it to lower it, the limit now to keep it.
	ErrNoLimit = errors.New(`creditagent: the content starts with "Limit", the limit recommended as a tool returned that amount, and the company's currency ("Limit 2500.00 CAD"): above the limit now for raise_limit, below it for lower_limit, the limit now for keep_limit`)
	// ErrUnread refuses a content writing a number the run did not read:
	// an amount no read returned.
	ErrUnread = errors.New("creditagent: every number in the content is one a tool returned or the run was given, written as returned; a count of your own is written in words")
	// ErrNoHistory refuses a new limit for a customer whose invoices and
	// payments the run did not read.
	ErrNoHistory = errors.New("creditagent: a customer whose invoices and payments the run has not read gets keep_limit or no recommendation")
	// ErrScore refuses a content that gives a score.
	ErrScore = errors.New("creditagent: the agent gives no score")
)

// Score runs one session on the customer's credit limit in the company:
// it finds the company by its code and reads the customer's exposure (a
// company or a customer the agent may not read stops the run before a
// session starts), offers the model the tools and reports what it
// recommended, also when the run fails after recommending. The customer
// is named by its business partner id.
func (a *Agent) Score(ctx context.Context, company, customer string) (recommend.Result, error) {
	le, err := apitools.LegalEntity(ctx, a.API, company)
	if err != nil {
		return recommend.Result{}, err
	}
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/credit-exposure", url.Values{"legal_entity_id": {le}, "customer_id": {customer}}, nil, "")
	var e exposure
	if err == nil {
		err = resp.Decode(&e)
	}
	if err != nil {
		return recommend.Result{}, fmt.Errorf("creditagent: the credit exposure of customer %s in company %s: %w", customer, company, err)
	}
	r := &review{e: e}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Review the credit limit of customer %s in company %s", e.CustomerNumber, e.CompanyCode),
		Prompt: fmt.Sprintf("Customer %s %s (business partner %s) in company %s (legal entity %s), whose currency is %s. %s Today is %s (UTC).",
			e.CustomerNumber, e.CustomerName, e.CustomerID, e.CompanyCode, le, e.Currency, limitNow(e), a.today()),
		Objects: map[string]string{e.CustomerID: CustomerObject},
		Observe: map[string]func(apitools.Response){},
		Check:   r.check,
	}
	if e.HasLimit {
		task.Objects = map[string]string{e.CreditLimitID: LimitObject}
	}
	for _, read := range history {
		task.Observe[read] = r.read
	}
	return agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
}

// today is the run's date, in UTC.
func (a *Agent) today() string {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	return now().UTC().Format(time.DateOnly)
}

// Reads are the operations the agent reads with (ADR-0072, T105a).
var Reads = []string{"getCreditExposure", "listCreditLimits", "getReceivablesAging", "listDunningRuns", "listDunningNotices",
	"listIncomingPayments", "listCustomerInvoices", "getBusinessPartner"}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{LimitObject: {Raise, Lower, Keep}, CustomerObject: {Set}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// limitNow says in the prompt whether the customer has a limit; its
// amount is the tools' to return.
func limitNow(e exposure) string {
	if e.HasLimit {
		return "It has a credit limit, " + e.CreditLimitID + "."
	}
	return "It has no credit limit."
}

// review is what one run's check knows: the customer's exposure as read
// before the run, and how much of its history the run has read.
type review struct {
	e       exposure
	history int // the customer's invoices and payments in the answers read
}

// read counts the customer's posted invoices and its payments in an
// answer: a draft that was never posted is no history, and a payment,
// written once, has no status.
func (r *review) read(resp apitools.Response) {
	var page struct {
		Items []struct {
			CustomerID string `json:"customer_id"`
			Status     string `json:"status"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows no history
	}
	for _, item := range page.Items {
		if item.CustomerID == r.e.CustomerID && (item.Status == "" || item.Status == "posted") {
			r.history++
		}
	}
}

// score is a score given: the word with a number right after it ("score
// 7", "scores 3 of 10", "score of 7", "score: 7"). It is a backstop of the
// prompt's rule, not a reading of the text: a rating given in other words
// is the person's to see.
var score = regexp.MustCompile(`(?i)\bscor(e|es|ed)\b(\s+(of|is|at))?[\s:=]*\d`)

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, T105a): it gives no score, every number it writes is one the
// run read, the first of them being the limit recommended, in the
// company's currency and fitting the kind, and a limit other than the one
// in place needs the customer's history read.
func (r *review) check(p recommend.Proposed, f *recommend.Figures) error {
	if score.MatchString(p.Content) {
		return ErrScore
	}
	if unread := f.Unread(p.Content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	stated := recommend.Numbers(p.Content)
	if len(stated) == 0 || !strings.HasPrefix(p.Content, "Limit "+stated[0]+" "+r.e.Currency) {
		return ErrNoLimit
	}
	if p.Kind != Keep && r.history == 0 {
		return ErrNoHistory
	}
	if p.Kind == Set {
		return nil
	}
	limit, now := new(big.Rat), new(big.Rat)
	// Both are decimals: one the pattern found in the content, the other
	// the API's own.
	limit.SetString(stated[0])
	now.SetString(r.e.Limit.Amount)
	if want := map[string]int{Raise: 1, Lower: -1, Keep: 0}[p.Kind]; limit.Cmp(now) != want {
		return ErrNoLimit
	}
	return nil
}
