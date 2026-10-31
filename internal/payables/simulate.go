package payables

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Decisions the invoice flow takes on a matched invoice.
const (
	DecisionAutoPost  = "auto_post" // clean within the auto-post limit: the rule posts it
	DecisionReview    = "review"    // clean above the limit: a person posts it
	DecisionException = "exception" // the match found issues: held, decided on an approval request
	// DecisionNotEvaluable: the invoice cannot be compared with its
	// company's amounts (no exchange rate on its date); a regression case
	// records it so that a rate recorded later shows as a change.
	DecisionNotEvaluable = "not_evaluable"
)

// Simulation replays the supplier invoices on purchase orders of a recent
// window under two configurations, for the configuration context's change
// requests: which invoices would be decided differently (tolerances, the
// auto-post limit) and which posting rules move (account determination,
// tax codes). It reads committed data in the request's transaction and
// writes nothing.
type Simulation struct {
	LegalEntities LegalEntities
	Orders        Orders
	Rates         Rates
	Window        time.Duration // how far back invoices are replayed when the simulation is given no window; default 90 days
	Limit         int           // at most this many invoices, newest first; default 2000
	Now           func() time.Time
}

// Decision is an invoice decided differently under the proposal.
type Decision struct {
	InvoiceID      uuid.UUID `json:"invoice_id"`
	DocumentNumber string    `json:"document_number"`
	Company        string    `json:"company"`
	Base           string    `json:"base"`
	Proposed       string    `json:"proposed"`
	Issues         []string  `json:"issues,omitempty"`
}

// Broken is an invoice the proposal cannot evaluate.
type Broken struct {
	InvoiceID      uuid.UUID `json:"invoice_id"`
	DocumentNumber string    `json:"document_number"`
	Company        string    `json:"company"`
	Reason         string    `json:"reason"`
}

// CompanyReplay is what replaying one company's invoices found. The
// simulation records it per company, with no total over companies, so that
// each reader of the change request is shown the companies they may access
// alone and counts over those (Disclosure).
type CompanyReplay struct {
	Company       string         `json:"company"`
	LegalEntityID uuid.UUID      `json:"legal_entity_id"`
	Invoices      int            `json:"invoices"`
	Changed       int            `json:"changed"`
	Broken        int            `json:"broken"`
	Base          map[string]int `json:"base"`     // decisions under the base configuration by outcome
	Proposed      map[string]int `json:"proposed"` // and under the proposal
	Changes       []Decision     `json:"changes"`  // the first examples of them
	Breaks        []Broken       `json:"breaks"`   // likewise
	Postings      []Posting      `json:"postings"`
}

// Posting is a posting rule that moves, with the invoices and amount of
// the window it applies to.
type Posting struct {
	Company  string      `json:"company"`
	Key      string      `json:"key"`
	Base     string      `json:"base"`
	Proposed string      `json:"proposed"`
	Invoices int         `json:"invoices"`
	Amount   money.Minor `json:"amount"` // minor units of the company's currency
	// amounts are the invoices' amounts in the company's currency, added
	// up once, in any order, into Amount.
	amounts []money.Minor
}

// examples is how many changes and breaks the detail lists.
const examples = 50

// Simulate implements configuration.Simulator: it replays the invoices of
// the window given, that of the regression suite the change request's
// regression evidence replays, or else of Window.
func (s Simulation) Simulate(ctx context.Context, base, proposed *config.Config, window time.Duration) (configuration.Simulation, error) {
	limit := s.Limit
	if window <= 0 {
		window = s.Window
	}
	if window <= 0 {
		window = 90 * 24 * time.Hour
	}
	if limit <= 0 {
		limit = 2000
	}
	replays, err := invoicesOnOrders(ctx, sources{s.LegalEntities, s.Orders, s.Rates}, clock.Now(s.Now).Add(-window), limit)
	if err != nil {
		return configuration.Simulation{}, err
	}
	t := &tally{moves: map[string]*Posting{}}
	for _, r := range replays {
		if err := t.invoice(r, base.Company(r.Company), proposed.Company(r.Company)); err != nil {
			return configuration.Simulation{}, err
		}
	}
	return t.result(window)
}

// tally collects the outcome of replaying invoices under the base and the
// proposed configuration.
type tally struct {
	companies map[string]*CompanyReplay // by company code
	moves     map[string]*Posting
	// err is the first amount a posting rule moves that is out of range in
	// the company's currency; result returns it.
	err error
}

// invoice decides one invoice under both configurations and records a
// break, a changed decision and the posting rules that move.
func (t *tally) invoice(r replayed, before, after *config.Company) error {
	v, co := r.Invoice, r.Company
	c := t.company(co, v.LegalEntityID)
	c.Invoices++
	if after == nil {
		c.broken(Broken{v.ID, v.DocumentNumber, co, "company " + co + " is not in the proposed configuration"})
		return nil
	}
	if r.Unevaluable != "" {
		c.broken(Broken{v.ID, v.DocumentNumber, co, r.Unevaluable})
		return nil
	}
	missing := ""
	for _, it := range v.Items {
		if it.TaxCode != nil && taxCode(after, *it.TaxCode) == nil {
			missing = *it.TaxCode
		}
	}
	if missing != "" {
		c.broken(Broken{v.ID, v.DocumentNumber, co, "tax code " + missing + " is not in the proposed configuration"})
		return nil
	}
	was, err := companyLimits(before)
	if err != nil {
		return err
	}
	is, err := companyLimits(after)
	if err != nil {
		return err
	}
	b, bi := decide(was, r)
	a, ai := decide(is, r)
	c.Base[b.Outcome]++
	c.Proposed[a.Outcome]++
	// The reasons name the limits, so only the outcomes are compared.
	if b.Outcome != a.Outcome || len(bi) != len(ai) {
		issues := make([]string, 0, len(ai))
		for _, i := range ai {
			issues = append(issues, i.Code)
		}
		c.Changed++
		if len(c.Changes) < examples {
			c.Changes = append(c.Changes, Decision{v.ID, v.DocumentNumber, co, b.Outcome, a.Outcome, issues})
		}
	}
	if before != nil {
		t.postingMoves(co, before, after, r)
	}
	return nil
}

// postingMoves records the posting rules an invoice is booked with that
// the proposal changes: account determination and its items' tax codes,
// and for an order valued through GR/IR the price difference account its
// order items may post to, counted once with the amount of those items:
// what the change exposes, as the difference itself is known only when it
// posts (the GR/IR account never changes once activated).
func (t *tally) postingMoves(co string, before, after *config.Company, r replayed) {
	v := r.Invoice
	if r.Order.Valuation == procurement.ValuationGRIR {
		var exposed []money.Minor
		for _, it := range v.Items {
			if it.POItemNo != nil {
				exposed = append(exposed, it.Amount)
			}
		}
		total, err := money.Sum(exposed...)
		switch {
		case err != nil:
			if t.err == nil {
				t.err = fmt.Errorf("payables: simulate: amount moved by %s/account_determination.price_difference: %w", co, err)
			}
		case len(exposed) > 0:
			t.move(co, "account_determination.price_difference", before.AccountDetermination.PriceDifference, after.AccountDetermination.PriceDifference,
				total, r.Conversion)
		}
	}
	t.move(co, "account_determination.input_tax", before.AccountDetermination.InputTax, after.AccountDetermination.InputTax, v.Tax, r.Conversion)
	t.move(co, "account_determination.ap_reconciliation", before.AccountDetermination.APReconciliation, after.AccountDetermination.APReconciliation,
		v.Gross, r.Conversion)
	for _, it := range v.Items {
		if it.TaxCode == nil {
			continue
		}
		if was := taxCode(before, *it.TaxCode); was != nil {
			t.taxMoves(co, was, taxCode(after, *it.TaxCode), v.InvoiceDate, it.Amount, r.Conversion)
		}
	}
}

// taxMoves records the rate in force on the invoice date and the account
// of each component of an item's tax code (T61a) that the proposal
// changes: a rate dated after the invoice moves nothing it posted. A
// component one side lacks moves from or to nothing. The one component of
// the single-rate form keeps the code's own paths.
func (t *tally) taxMoves(co string, was, is *config.TaxCode, date time.Time, amount money.Minor, conv ledger.Conversion) {
	parts := map[string][2]config.TaxComponent{}
	var keys []string
	for i, tc := range []*config.TaxCode{was, is} {
		for _, c := range tc.Parts() {
			p, seen := parts[c.Key]
			if !seen {
				keys = append(keys, c.Key)
			}
			p[i] = c
			parts[c.Key] = p
		}
	}
	for _, k := range keys {
		path := "tax_codes." + was.Code
		if k != config.LegacyComponent {
			path += ".components." + k
		}
		before, _ := parts[k][0].RateOn(date) // "" when none is in force: a rate starting or ending is a move
		after, _ := parts[k][1].RateOn(date)
		t.move(co, path+".rate_percent", before, after, amount, conv)
		t.move(co, path+".account", parts[k][0].Account, parts[k][1].Account, amount, conv)
	}
}

// move counts an invoice and its amount, converted into the company's
// currency, against a posting rule that changes value.
func (t *tally) move(co, key, from, to string, amount money.Minor, conv ledger.Conversion) {
	if from == to {
		return
	}
	k := co + "/" + key
	if t.moves[k] == nil {
		t.moves[k] = &Posting{Company: co, Key: key, Base: from, Proposed: to}
	}
	t.moves[k].Invoices++
	v, err := conv.Convert(amount)
	if err != nil && t.err == nil {
		t.err = fmt.Errorf("payables: simulate: amount moved by %s: %w", k, err)
	}
	t.moves[k].amounts = append(t.moves[k].amounts, v)
}

// company is the replay of a company's invoices, started on its first.
func (t *tally) company(code string, legalEntity uuid.UUID) *CompanyReplay {
	if t.companies == nil {
		t.companies = map[string]*CompanyReplay{}
	}
	c := t.companies[code]
	if c == nil {
		c = &CompanyReplay{Company: code, LegalEntityID: legalEntity, Base: map[string]int{}, Proposed: map[string]int{},
			Changes: []Decision{}, Breaks: []Broken{}, Postings: []Posting{}}
		t.companies[code] = c
	}
	return c
}

// broken counts an invoice the proposal cannot evaluate, listed among the
// first examples.
func (c *CompanyReplay) broken(b Broken) {
	c.Broken++
	if len(c.Breaks) < examples {
		c.Breaks = append(c.Breaks, b)
	}
}

// result is the simulation's verdict, summary and detail; it passes when
// every invoice can be evaluated under the proposal. An amount moved out of
// range fails the simulation. The detail is by company and the summary
// carries no count: both reach readers of narrower company access than the
// requester's (the summary the operation's result and the audit trail too),
// who are each shown counts over their own companies (Disclosure).
func (t *tally) result(window time.Duration) (configuration.Simulation, error) {
	if t.err != nil {
		return configuration.Simulation{}, t.err
	}
	keys := slices.Sorted(maps.Keys(t.moves))
	for _, k := range keys {
		p := t.moves[k]
		sum, err := money.Sum(p.amounts...)
		if err != nil {
			return configuration.Simulation{}, fmt.Errorf("payables: simulate: amount moved by %s/%s: %w", p.Company, p.Key, err)
		}
		p.Amount = sum
		// A rule moves only for an invoice, which started its company's
		// replay; the legal entity is that replay's.
		c := t.company(p.Company, uuid.Nil)
		c.Postings = append(c.Postings, *p)
	}
	companies := make([]CompanyReplay, 0, len(t.companies))
	passed := true
	for _, code := range slices.Sorted(maps.Keys(t.companies)) {
		companies = append(companies, *t.companies[code])
		passed = passed && t.companies[code].Broken == 0
	}
	days := int(window.Hours() / 24)
	return configuration.Simulation{Passed: passed, Summary: recordedSummary(days),
		Detail: map[string]any{"window_days": days, byCompanyKey: companies}}, nil
}

func taxCode(co *config.Company, code string) *config.TaxCode {
	for i := range co.TaxCodes {
		if co.TaxCodes[i].Code == code {
			return &co.TaxCodes[i]
		}
	}
	return nil
}

// Verdict is the invoice flow's decision on a matched invoice and why.
type Verdict struct {
	Outcome string // DecisionAutoPost, DecisionReview, DecisionException or DecisionNotEvaluable
	// Reason is the converted gross and the limit; for not_evaluable the
	// stable code (no_exchange_rate, amount_too_large), a regression case's
	// issue.
	Reason string
}

// decide is the invoice flow's decision under a company's limits, the one
// rule the match, simulations and regression suites apply (F12b), with the
// match issues. An invoice that cannot be converted into the company's
// currency is not evaluable, and without a rate not matched either; one
// with issues is an exception; a clean one
// is posted automatically when its gross amount in the company's currency
// is within the auto-post limit, compared exactly, and reviewed above it.
func decide(lim limits, r replayed) (Verdict, []Issue) {
	// Without a rate there is no conversion to evaluate the variances with,
	// so the invoice is not matched at all. A gross amount out of range has
	// a conversion: its items are matched, as a match records them (F12).
	if r.noRate != nil {
		return Verdict{DecisionNotEvaluable, r.unevaluableCode}, nil
	}
	issues := evaluate(r.Invoice, r.Order, r.Invoiced, lim, r.Conversion)
	if r.Unevaluable != "" {
		return Verdict{DecisionNotEvaluable, r.unevaluableCode}, issues
	}
	if len(issues) > 0 {
		return Verdict{DecisionException, fmt.Sprintf("%d match issue(s)", len(issues))}, issues
	}
	c := r.Conversion
	gross, limit := r.Gross.Plain(c.ToExp)+" "+c.To, lim.autoPostText+" "+c.To
	if r.grossExact.Cmp(lim.autoPost) > 0 {
		return Verdict{DecisionReview, "gross " + gross + " is above the auto-post limit " + limit}, issues
	}
	reason := "gross " + gross + " is within the auto-post limit " + limit
	if near(r, lim) {
		reason += ", near it"
	}
	return Verdict{DecisionAutoPost, reason}, issues
}
