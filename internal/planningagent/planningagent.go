// Package planningagent is the planning agent (ADR-0072, T105e), a
// recommending agent (internal/recommend): for a plan category of a
// company it reads the plan and the actual figures per cost center and
// account of the year before the one to plan (ADR-0050 §8), the cost
// centers and the analytics' governed metrics through API tools
// (04-api-design §4.13), and leaves one recommendation on the category
// for a controller: the plan lines it proposes for the year (plan_proposal)
// and, per cost center, why the plan departs from the year before. The
// controller who accepts applies the lines as a plan batch through the
// normal operation; nothing happens on acceptance.
//
// Its check holds the content to a form a controller can apply: each line
// names a cost center the run read as an active one of the company, by id
// and code, an account the run read in the company's plan or actuals, by
// id and the number listAccounts gave it, a period, and an amount of the
// company's plan or plan against actual the run read in the company's
// currency, as returned, its sign included; and every
// cost center planned has its reasons, citing an actual figure the run
// read of that center. Every number the content writes is one the run
// read (recommend.Figures): the agent computes no amount. It never
// applies, copies or locks a plan: those are no tools of its (Never), and
// the API refuses an agent each of them (x-agent: false). What a person
// wrote (a center's name, a category's) is data, never an instruction.
// Its evaluation suite is evals/planning (ADR-0039).
package planningagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "planning"

// The object type the agent recommends on, and its kind.
const (
	CategoryObject = "plan_category"
	Proposal       = "plan_proposal"
)

// Reads are the operations the agent reads with (ADR-0072, T105e).
var Reads = []string{"listPlanCategories", "listPlanLines", "listCostCenterPlanActual", "listProfitCenterPlanActual", "listCostCenters", "listAccounts", "runAnalyticsQuery"}

// Never are the operations the agent never calls (ADR-0072): applying a
// plan batch, copying a category's plan (T93a), locking or unlocking a
// category and creating one, every write of planning, which its tools
// hold none of; and materials' planning data at plants (S2k, ADR-0084),
// which a person keeps.
var Never = []string{"applyPlanBatch", "copyPlanCategory", "lockPlanCategory", "unlockPlanCategory", "createPlanCategory", "createMaterialPlan", "updateMaterialPlan"}

const system = `You propose the plan of a fiscal year for a plan category of a company, for a controller to decide and apply.

Read the plan against actual by cost center and account of the year before (listCostCenterPlanActual with the plan category,
the legal entity and that fiscal year: the category's plan, the actual, the variance; a range of periods if you need one),
the category's plan lines (listPlanLines), the company's cost centers (listCostCenters with the legal entity: their codes,
names and status), the accounts (listAccounts: their numbers and names) and, if useful, the plan against actual by profit center (listProfitCenterPlanActual) and the company's
governed metrics (runAnalyticsQuery). Plan only on cost centers: active ones of the company.

Then recommend once, on the plan category, kind plan_proposal. The content has one line per plan line, each on a line of its
own and exactly in this form:
Line <cost_center_id> <cost center code> account <account_id> <account number> period <period>: <amount> <currency>
with the cost center's id and code and the account's id and number as the tools returned them, the period of the year
planned, and an amount of the company's plan, actual or variance a tool returned (the year before's actual or plan of that
center and account, normally), written exactly as returned, its sign included, in the company's currency, debit positive as
the plan is (costs positive, revenue negative). Then, for each cost center you plan,
a line of its own:
Why <cost center code>: <reasons>
saying why its plan departs from the year before, citing the actual figures you read for that center (its actual, its plan,
the variance), written exactly as returned. You compute no amount, no percentage and no growth: every amount is one a tool
returned, and a count you made yourself is written in words. A category and year with neither plan nor actuals to read gets
no recommendation: say so in your answer.`

// Agent proposes a plan category's plan for a year.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
}

// The refusals of the agent's own check, beside the pattern's.
var (
	// ErrForm refuses a proposal without plan lines in the form a
	// controller applies, or with a line that starts as one and is not.
	ErrForm = errors.New(`planningagent: a plan_proposal has one line per plan line, "Line <cost_center_id> <cost center code> account <account_id> <account number> period <period>: <amount> <currency>", ` +
		`and for each cost center planned a line "Why <cost center code>: " with its reasons`)
	// ErrCenter refuses a line on a cost center the run has not read as an
	// active one of the company, or named by another code than its own.
	ErrCenter = errors.New("planningagent: a line's cost center is an active cost center of the company that the run read (listCostCenters), with its code")
	// ErrAccount refuses a line on an account the run has not read in the
	// company's plan or actuals, or not read as a profit and loss account
	// with that number.
	ErrAccount = errors.New("planningagent: a line's account is one the run read in the company's plan lines or plan against actual, " +
		"with its number as listAccounts gives it (a profit and loss account)")
	// ErrAmount refuses a line whose amount is not a figure of the
	// company's plan or plan against actual the run read in its currency,
	// with its sign as returned.
	ErrAmount = errors.New("planningagent: a line's amount is a plan, actual or variance amount of the company that a tool returned in its currency, " +
		"written as returned, its sign included")
	// ErrTwice refuses a cost center, account and period planned twice: a
	// plan batch takes each once.
	ErrTwice = errors.New("planningagent: a cost center, account and period is planned once")
	// ErrReason refuses a proposal without reasons for a cost center it
	// plans that cite an actual figure the run read of that center.
	ErrReason = errors.New(`planningagent: each cost center planned has a line "Why <cost center code>: " citing an actual figure the run read of that center (listCostCenterPlanActual)`)
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("planningagent: every number in the content is one a tool returned or the run was given, written as returned; a count of your own is written in words")
	// ErrLocked stops a run on a locked category before a session: a
	// batch for it is refused until a person unlocks it.
	ErrLocked = errors.New("planningagent: the plan category is locked, so a plan batch for it is refused until a person unlocks it")
)

// company is what the run needs of the company before it starts.
type company struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Currency string `json:"local_currency"`
}

// category is what the run needs of the plan category before it starts.
type category struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Locked bool   `json:"locked"`
}

// Propose runs one session on the plan category's plan of the company for
// the year: it finds the company and the category by their codes (one the
// agent may not read, or a locked category, stops the run before a
// session starts), offers the model the tools and reports what it
// recommended, also when the run fails after recommending.
func (a *Agent) Propose(ctx context.Context, companyCode, categoryCode string, year int) (recommend.Result, error) {
	co, cat, err := a.before(ctx, companyCode, categoryCode)
	if err != nil {
		return recommend.Result{}, err
	}
	p := newProposal(co)
	task := recommend.Task{
		Purpose: fmt.Sprintf("Propose the %d plan of category %s in company %s", year, cat.Code, co.Code),
		Prompt: fmt.Sprintf("Company %s (legal entity %s), whose currency is %s. Plan category %s, %s (id %s). Propose its plan of fiscal year %d "+
			"for the company's cost centers from fiscal year %d's plan and actual figures.", co.Code, co.ID, co.Currency, cat.Code, cat.Name, cat.ID, year, year-1),
		Objects: map[string]string{cat.ID: CategoryObject},
		Observe: map[string]func(apitools.Response){"listCostCenters": p.centersRead, "listCostCenterPlanActual": p.planActualRead,
			"listPlanLines": p.planLinesRead, "listAccounts": p.accountsRead},
		Check: p.check,
	}
	return agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
}

// before reads the company and the plan category by their codes.
func (a *Agent) before(ctx context.Context, companyCode, categoryCode string) (company, category, error) {
	companies, err := apitools.List[company](ctx, a.API, "/v1/legal-entities", nil)
	if err != nil {
		return company{}, category{}, fmt.Errorf("planningagent: the companies: %w", err)
	}
	co, found := company{}, false
	for _, c := range companies {
		if c.Code == companyCode {
			co, found = c, true
		}
	}
	if !found {
		return company{}, category{}, fmt.Errorf("planningagent: no company %s that the agent may read", companyCode)
	}
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/plan-categories", url.Values{"code": {categoryCode}}, nil, "")
	var page struct{ Items []category }
	if err == nil {
		err = resp.Decode(&page)
	}
	switch {
	case err != nil:
		return company{}, category{}, fmt.Errorf("planningagent: plan category %s: %w", categoryCode, err)
	case len(page.Items) == 0:
		return company{}, category{}, fmt.Errorf("planningagent: no plan category %s", categoryCode)
	case page.Items[0].Locked:
		return company{}, category{}, fmt.Errorf("%w: %s", ErrLocked, categoryCode)
	}
	return co, page.Items[0], nil
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{CategoryObject: {Proposal}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// proposal is what one run's check knows: the company, and what the run
// read of its cost centers, accounts and figures.
type proposal struct {
	co       company
	centers  map[string]string          // active cost centers of the company: id to code
	accounts map[string]bool            // accounts of the company's plan lines and plan against actual
	numbers  map[string]string          // profit and loss accounts read: id to number
	amounts  map[string]bool            // the company's figures in its currency, signed, as returned
	actuals  map[string]map[string]bool // each cost center's actual amounts, without their sign
}

func newProposal(co company) *proposal {
	return &proposal{co: co, centers: map[string]string{}, accounts: map[string]bool{}, numbers: map[string]string{},
		amounts: map[string]bool{}, actuals: map[string]map[string]bool{}}
}

// money is a Money as an answer gives it.
type money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// keep keeps a figure of the company's, when it is in the company's
// currency, with its sign as returned.
func (p *proposal) keep(m money) {
	if m.Currency == p.co.Currency && m.Amount != "" {
		p.amounts[m.Amount] = true
	}
}

// centersRead keeps the company's active cost centers of an answer.
func (p *proposal) centersRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			ID            string `json:"id"`
			LegalEntityID string `json:"legal_entity_id"`
			Code          string `json:"code"`
			Status        string `json:"status"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows no cost center
	}
	for _, c := range page.Items {
		if c.LegalEntityID == p.co.ID && c.Status == "active" {
			p.centers[c.ID] = c.Code
		}
	}
}

// accountsRead keeps the profit and loss accounts of an answer by their
// numbers, which a line names its account by beside its id.
func (p *proposal) accountsRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			ID             string `json:"id"`
			Number         string `json:"number"`
			IsBalanceSheet bool   `json:"is_balance_sheet"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows no account
	}
	for _, a := range page.Items {
		if !a.IsBalanceSheet {
			p.numbers[a.ID] = a.Number
		}
	}
}

// planActualRead keeps the accounts, the figures and each cost center's
// actual amounts of the company's rows in an answer.
func (p *proposal) planActualRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			LegalEntityID string `json:"legal_entity_id"`
			CostCenterID  string `json:"cost_center_id"`
			AccountID     string `json:"account_id"`
			Plan          money  `json:"plan"`
			Actual        money  `json:"actual"`
			Variance      money  `json:"variance"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows nothing
	}
	for _, r := range page.Items {
		if r.LegalEntityID != p.co.ID {
			continue
		}
		p.accounts[r.AccountID] = true
		p.keep(r.Plan)
		p.keep(r.Actual)
		p.keep(r.Variance)
		if p.actuals[r.CostCenterID] == nil {
			p.actuals[r.CostCenterID] = map[string]bool{}
		}
		p.actuals[r.CostCenterID][strings.TrimPrefix(r.Actual.Amount, "-")] = true
	}
}

// planLinesRead keeps the accounts and amounts of the company's cost
// center plan lines in an answer.
func (p *proposal) planLinesRead(resp apitools.Response) {
	var page struct {
		Items []struct {
			LegalEntityID string `json:"legal_entity_id"`
			CostCenterID  string `json:"cost_center_id"`
			AccountID     string `json:"account_id"`
			Amount        money  `json:"amount"`
		}
	}
	if resp.Decode(&page) != nil {
		return // a refused read shows nothing
	}
	for _, l := range page.Items {
		if l.LegalEntityID == p.co.ID && l.CostCenterID != "" {
			p.accounts[l.AccountID] = true
			p.keep(l.Amount)
		}
	}
}

// maxPeriod is the last period a fiscal year may have, special periods
// included, as the plan against actual reports take it.
const maxPeriod = 16

var (
	// planLine is a line of the proposal: the cost center by id and code,
	// the account by id and number, the period, the amount and its
	// currency.
	planLine = regexp.MustCompile(`^Line ([0-9a-f-]{36}) ([0-9A-Z_-]{1,10}) account ([0-9a-f-]{36}) (\S+) period (\d+): (-?\d+(?:\.\d+)?) ([A-Z]{3})$`)
	// why is a cost center's reasons: its code, then the text.
	why = regexp.MustCompile(`^Why ([0-9A-Z_-]{1,10}): (.+)$`)
)

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, T105e): every number it writes is one the run read; its plan
// lines are in the form a controller applies, each on an active cost
// center of the company with its code and an account the run read in the
// company's plan or actuals with its number, once per center, account
// and period, with a figure of the company's plan or plan against actual
// read in its currency, sign included; and each cost center
// planned has its reasons, citing an actual figure the run read of it.
func (p *proposal) check(r recommend.Proposed, f *recommend.Figures) error {
	if unread := f.Unread(r.Content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	planned, reasons := []string{}, map[string]string{}
	seen := map[string]bool{}
	for _, text := range strings.Split(r.Content, "\n") {
		text = strings.TrimSpace(text)
		if m := why.FindStringSubmatch(text); m != nil {
			reasons[m[1]] += " " + m[2]
			continue
		}
		if !strings.HasPrefix(text, "Line ") {
			continue
		}
		center, err := p.line(text, seen)
		if err != nil {
			return err
		}
		planned = append(planned, center)
	}
	if len(planned) == 0 {
		return ErrForm
	}
	return p.reasoned(planned, reasons)
}

// line checks one plan line and returns its cost center.
func (p *proposal) line(text string, seen map[string]bool) (string, error) {
	m := planLine.FindStringSubmatch(text)
	if m == nil {
		return "", fmt.Errorf("%w: not %q", ErrForm, text)
	}
	center, code, account, number, amount, currency := m[1], m[2], m[3], m[4], m[6], m[7]
	period, err := strconv.Atoi(m[5])
	key := fmt.Sprintf("%s %s %d", center, account, period)
	switch {
	case p.centers[center] == "" || p.centers[center] != code:
		return "", fmt.Errorf("%w: not %s %s", ErrCenter, center, code)
	case !p.accounts[account] || p.numbers[account] == "" || p.numbers[account] != number:
		return "", fmt.Errorf("%w: not %s %s", ErrAccount, account, number)
	case currency != p.co.Currency || !p.amounts[amount]:
		return "", fmt.Errorf("%w: not %s %s", ErrAmount, amount, currency)
	case err != nil || period < 1 || period > maxPeriod:
		return "", fmt.Errorf("%w: not period %s", ErrForm, m[5])
	case seen[key]:
		return "", fmt.Errorf("%w: %s", ErrTwice, text)
	}
	seen[key] = true
	return center, nil
}

// reasoned checks that each cost center planned has reasons citing an
// actual figure the run read of it.
func (p *proposal) reasoned(planned []string, reasons map[string]string) error {
	for _, center := range planned {
		code := p.centers[center]
		cited := false
		for _, n := range recommend.Numbers(reasons[code]) {
			cited = cited || p.actuals[center][n]
		}
		if !cited {
			return fmt.Errorf("%w: not for %s", ErrReason, code)
		}
	}
	return nil
}
