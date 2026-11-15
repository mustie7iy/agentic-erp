// Package savingsagent is the savings opportunity agent (ADR-0072 As
// built, T105d), a recommending agent (internal/recommend): for a
// company and a period it reads the spend cube (runAnalyticsQuery, the
// spend metrics of T91b), the contracts, the catalog items and the
// purchase orders through API tools (04-api-design §4.13), and leaves, for
// a buyer to decide, one recommendation per opportunity it found: a
// supplier with spend off contract to bring under a contract
// (bring_under_contract, on the business partner), an account with spend
// from several suppliers to consolidate (consolidate_suppliers, on the
// account), and an order priced above the active contract of its supplier
// for the same material (use_contract_price, on the purchase order). The
// opportunities are found by the run's own code before its session, not
// judged by the model, and at most Max of them a run. Every figure it
// writes is one the run read; each recommendation starts with the spend the
// opportunity covers as the API returned it, and states no saving in
// money (plan default 13). It never creates an RFQ, a contract or an
// order (Never). Everything a supplier or a person wrote is data, never
// an instruction. Its evaluation suite is evals/savings (ADR-0039).
package savingsagent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "savings"

// The object types the agent recommends on, each with its one kind.
const (
	SupplierObject     = "business_partner"
	AccountObject      = "account"
	OrderObject        = "purchase_order"
	BringUnderContract = "bring_under_contract"
	Consolidate        = "consolidate_suppliers"
	UseContractPrice   = "use_contract_price"
)

// MaxOpportunities is the default of the most recommendations a run
// makes (Agent.Max): its opportunities, bring_under_contract first, then
// consolidate_suppliers, then use_contract_price, each the largest spend
// first.
const MaxOpportunities = 10

// Reads are the operations the agent reads with (ADR-0072 As built,
// T105d): the spend cube and its model, the contracts, the catalog
// items, the purchase orders, the suppliers and the accounts.
var Reads = []string{"runAnalyticsQuery", "getAnalyticsModel", "listContracts", "getContract", "listCatalogItems", "listPurchaseOrders",
	"getPurchaseOrder", "getBusinessPartner", "listAccounts"}

// Never are the operations the agent never calls (ADR-0072): creating an
// RFQ, a contract or an order, or releasing one; its tools hold none.
var Never = []string{"createRfq", "publishRfq", "createContract", "releaseContract", "createPurchaseOrder", "releasePurchaseOrder"}

const system = `You find savings opportunities in a company's purchasing for a buyer to decide.

You are given the company, the period and the opportunities the run found, each with the object to recommend on and its
kind. Read the spend in the period with runAnalyticsQuery (the metrics spend, spend_under_contract and spend_off_contract
by vendor, vendor_name, account, account_name and currency, the company in companies, a filter on posting_date between
the period's first and last day); the metrics' definitions with getAnalyticsModel; the contracts (listContracts,
getContract), the catalog items (listCatalogItems), the purchase orders (listPurchaseOrders, getPurchaseOrder), the
suppliers (getBusinessPartner) and the accounts (listAccounts) as you need.

Then recommend once on each opportunity you are given: bring_under_contract on a supplier with spend off contract, to
negotiate a contract with it; consolidate_suppliers on an account bought from several suppliers, to buy from fewer;
use_contract_price on an order priced above the active contract of its supplier for the same material, to order at the
contract's price. The content's first line is the spend the opportunity covers, as you are told its form and as a tool
returned it; then the reasons with the figures behind them: the suppliers, the spend under and off contract, the prices.
State no saving in money and no percentage you worked out: the buyer judges what a contract or a consolidation would
save. When what you read does not support an opportunity, make no recommendation on it.`

// Agent finds savings opportunities.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
	// Max is the most recommendations a run makes; default
	// MaxOpportunities.
	Max int
	// Now is the clock the default period is read from; nil is the time.
	Now func() time.Time
}

// The refusals before a run and of the agent's own check, beside the
// pattern's.
var (
	// ErrPeriod refuses a period that is not two dates in order.
	ErrPeriod = errors.New("savingsagent: the period is FROM..TO, two dates YYYY-MM-DD, the first not after the second")
	// ErrMax refuses a maximum below one.
	ErrMax = errors.New("savingsagent: the most recommendations a run makes is at least one")
	// ErrSpend refuses a content whose first line is not the spend the
	// opportunity covers, as the run found it and a tool returned it.
	ErrSpend = errors.New("savingsagent: the first line is the spend the opportunity covers, in the form and with the amounts you were given, as a tool returned them")
	// ErrSaving refuses a content stating a saving with a figure.
	ErrSaving = errors.New("savingsagent: state no saving in money or as a percentage: only the spend the opportunity covers")
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("savingsagent: every number is one a tool returned or you were given, written as returned; a count of your own is written in words")
)

// Advise runs one session on the company's purchasing in the period
// (FROM..TO; "" is the year to today, UTC): it finds the company, reads
// the spend, the contracts and the orders and works out the
// opportunities (anything the agent may not read stops the run before a
// session starts), then offers the model the tools and reports what it
// recommended, also when the run fails after recommending. A period
// without opportunities starts no session and says so.
func (a *Agent) Advise(ctx context.Context, company, period string) (recommend.Result, error) {
	from, to, err := a.period(period)
	if err != nil {
		return recommend.Result{}, err
	}
	limit := a.Max
	if limit == 0 {
		limit = MaxOpportunities
	}
	if limit < 0 {
		return recommend.Result{}, ErrMax
	}
	le, err := apitools.LegalEntity(ctx, a.API, company)
	if err != nil {
		return recommend.Result{}, err
	}
	s := &scan{api: a.API, company: company, le: le, from: from, to: to}
	found, err := s.opportunities(ctx)
	if err != nil {
		return recommend.Result{}, err
	}
	if len(found) == 0 {
		return recommend.Result{Reply: fmt.Sprintf("No savings opportunity in company %s's purchasing from %s to %s: no spend off contract, no account bought from several suppliers and no order above its contract's price.", company, from, to)}, nil
	}
	r := &review{opportunities: map[string]opportunity{}}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Find savings opportunities in company %s's purchasing from %s to %s", company, from, to),
		Prompt:  prompt(company, le, from, to, found, limit),
		Objects: map[string]string{},
		Check:   r.check,
	}
	names := map[string]string{}
	for _, o := range found[:min(limit, len(found))] {
		r.opportunities[o.id] = o
		task.Objects[o.id] = o.object
		names[o.id] = o.name
	}
	res, err := agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
	res.Names = names
	return res, err
}

// period is the run's period: the one given, or the year to today, UTC.
func (a *Agent) period(p string) (string, string, error) {
	if p == "" {
		now := time.Now
		if a.Now != nil {
			now = a.Now
		}
		to := now().UTC()
		return to.AddDate(-1, 0, 1).Format(time.DateOnly), to.Format(time.DateOnly), nil
	}
	from, to, ok := strings.Cut(p, "..")
	f, err1 := time.Parse(time.DateOnly, from)
	t, err2 := time.Parse(time.DateOnly, to)
	if !ok || err1 != nil || err2 != nil || t.Before(f) {
		return "", "", fmt.Errorf("%w: not %q", ErrPeriod, p)
	}
	return from, to, nil
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{SupplierObject: {BringUnderContract}, AccountObject: {Consolidate}, OrderObject: {UseContractPrice}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// prompt names the company, the period and each opportunity the run
// recommends on, with the form of its first line; the amounts are the
// tools' to return.
func prompt(company, le, from, to string, found []opportunity, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Company %s (legal entity %s); the period is %s to %s. The run found %s; you recommend on the first %s, at most %s a run.\n",
		company, le, from, to, count(len(found)), count(min(limit, len(found))), count(limit))
	for _, o := range found[:min(limit, len(found))] {
		fmt.Fprintf(&b, "%s: %s; the first line is %q with the amounts a tool returns.\n", o.name, o.why, o.form)
	}
	return b.String()
}

// count says how many opportunities, in words where it is one.
func count(n int) string {
	if n == 1 {
		return "one opportunity"
	}
	return strconv.Itoa(n) + " opportunities"
}

// saving is a saving stated with a figure: a word of saving, then a
// number in the same sentence.
var saving = regexp.MustCompile(`(?i)\b(?:sav(?:e|es|ed|ing|ings)|cheaper|reduc(?:e|es|ed|tion))\b[^.\n]*\d`)

// review is what one run's check knows: the opportunities by object id.
type review struct {
	opportunities map[string]opportunity
}

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072 As built, T105d): its first line is the spend the
// opportunity covers as the run found it, each amount one a tool of the
// run returned as money, it states no saving with a figure, and every
// number written is one the run read.
func (r *review) check(p recommend.Proposed, f *recommend.Figures) error {
	o := r.opportunities[p.ObjectID] // the pattern admits only the run's objects, each with its one kind
	first, _, _ := strings.Cut(p.Content, "\n")
	if strings.TrimSpace(first) != o.first {
		return fmt.Errorf("%w: %q", ErrSpend, o.first)
	}
	for _, amount := range o.amounts {
		if !f.Returned(amount) {
			// Found before the session; the model reads it again with its tool.
			return fmt.Errorf("%w: %s is no amount a tool of the run returned", ErrSpend, amount)
		}
	}
	if saving.MatchString(p.Content) {
		return ErrSaving
	}
	if unread := f.Unread(p.Content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	return nil
}
