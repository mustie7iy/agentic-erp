package payables

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Regression captures regression suites from the supplier invoices on
// purchase orders and replays them (configuration.Regressor). A suite is a
// stratified sample: every decision, every kind of match issue, invoices
// near the auto-post limit, then more up to the maximum, each with the
// decision it got when captured.
type Regression struct {
	LegalEntities LegalEntities
	Orders        Orders
	Rates         Rates
	Now           func() time.Time
}

func (g Regression) sources() sources { return sources{g.LegalEntities, g.Orders, g.Rates} }

// replayed is an invoice with everything its decision depends on.
type replayed struct {
	Invoice  Invoice
	Company  string
	Order    procurement.Order
	Invoiced map[int32]string
	// Conversion takes the invoice's amounts into the company's currency,
	// the currency of its configured amounts (F12); Gross is the gross
	// amount converted.
	Conversion ledger.Conversion
	Gross      money.Minor
	// Unevaluable says why the invoice cannot be compared with its
	// company's amounts (no rate on its date, a gross amount out of range
	// in the company's currency), or is "". Such an invoice is reported,
	// not a reason to fail every simulation and suite of the tenant.
	Unevaluable string
	// unevaluableCode is the stable code of Unevaluable, the issue of its
	// regression case: no_exchange_rate or amount_too_large.
	unevaluableCode string
}

// sources are the ports replaying an invoice reads through.
type sources struct {
	legalEntities LegalEntities
	orders        Orders
	rates         Rates
}

// replayCache keeps what replaying reads once: legal entities, orders and
// conversions by currency, company currency and date.
type replayCache struct {
	companies   map[uuid.UUID]tenancy.Entity
	orders      map[uuid.UUID]procurement.Order
	conversions map[string]ledger.Conversion
}

func newReplayCache() *replayCache {
	return &replayCache{companies: map[uuid.UUID]tenancy.Entity{}, orders: map[uuid.UUID]procurement.Order{},
		conversions: map[string]ledger.Conversion{}}
}

// conversion is the conversion of an invoice's amounts into its company's
// currency at the rate of its invoice date.
func (c *replayCache) conversion(ctx context.Context, rates Rates, v Invoice, to string) (ledger.Conversion, error) {
	k := v.Currency + "/" + to + "/" + v.InvoiceDate.Format(time.DateOnly)
	if conv, ok := c.conversions[k]; ok {
		return conv, nil
	}
	conv, err := rates.Conversion(ctx, v.Currency, to, v.InvoiceDate)
	if err != nil {
		return conv, err
	}
	c.conversions[k] = conv
	return conv, nil
}

// invoicesOnOrders reads the invoices on purchase orders of the window,
// newest first, with their orders and what was invoiced before.
func invoicesOnOrders(ctx context.Context, src sources, since time.Time, limit int) ([]replayed, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, invoiceSelect+` WHERE i.purchase_order_id IS NOT NULL AND i.status IN ('parked', 'on_hold', 'posted') AND i.invoice_date >= $1
		ORDER BY i.invoice_date DESC, i.id DESC LIMIT $2`, since, limit)
	invoices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invoice, error) { return scanInvoice(r) })
	if err != nil {
		return nil, fmt.Errorf("payables: read invoices on orders since %s: %w", since.Format(time.DateOnly), err)
	}
	cache := newReplayCache()
	out := make([]replayed, 0, len(invoices))
	for _, v := range invoices {
		r, err := replay(ctx, tx, src, v, cache)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// replay gathers one invoice's inputs, through the cache.
func replay(ctx context.Context, tx pgx.Tx, src sources, v Invoice, c *replayCache) (replayed, error) {
	le, ok := c.companies[v.LegalEntityID]
	if !ok {
		var err error
		if le, _, err = src.legalEntities.Entity(ctx, v.LegalEntityID); err != nil {
			return replayed{}, err
		}
		c.companies[v.LegalEntityID] = le
	}
	order, ok := c.orders[*v.PurchaseOrderID]
	if !ok {
		var err error
		if order, _, err = src.orders.Order(ctx, *v.PurchaseOrderID); err != nil {
			return replayed{}, err
		}
		c.orders[*v.PurchaseOrderID] = order
	}
	r := replayed{Invoice: v, Company: le.Code, Order: order}
	if err := c.inCompany(ctx, src.rates, &r, le.LocalCurrency); err != nil {
		return replayed{}, err
	}
	invoiced, err := invoicedQuantities(ctx, tx, *v.PurchaseOrderID, v.ID)
	if err != nil {
		return replayed{}, err
	}
	r.Invoiced = invoiced
	return r, nil
}

// inCompany sets the conversion of the invoice into the company's currency
// and its gross amount converted, or why it cannot be converted: no rate
// valid on the invoice date, or a gross amount out of range there, which
// could be neither compared with the limits nor posted.
func (c *replayCache) inCompany(ctx context.Context, rates Rates, r *replayed, to string) error {
	v := r.Invoice
	conv, err := c.conversion(ctx, rates, v, to)
	var p *problem.Problem
	if errors.As(err, &p) && p.Code == "ledger.no_exchange_rate" {
		r.Unevaluable = fmt.Sprintf("no average exchange rate from %s to %s on %s", v.Currency, to, v.InvoiceDate.Format(time.DateOnly))
		r.unevaluableCode = "no_exchange_rate"
		return nil
	}
	if err != nil {
		return err
	}
	// Convert fails only for a result out of range (money.ErrOverflow), as
	// the rate and exponents come from the database; the reason carries the
	// error, whatever it is.
	gross, err := conv.Convert(v.Gross)
	if err != nil {
		r.Unevaluable = fmt.Sprintf("gross amount %s %s cannot be converted into %s: %v", v.Gross.Plain(v.Exponent), v.Currency, to, err)
		r.unevaluableCode = "amount_too_large"
		return nil
	}
	r.Conversion, r.Gross = conv, gross
	return nil
}

// outcome is a decision with its issue codes; an invoice that cannot be
// compared with its company's amounts is not evaluable, with the code of
// the reason as its issue.
func outcome(lim limits, r replayed) (string, []string) {
	if r.Unevaluable != "" {
		return DecisionNotEvaluable, []string{r.unevaluableCode}
	}
	decision, issues := decide(lim, r)
	codes := make([]string, 0, len(issues))
	for _, i := range issues {
		codes = append(codes, i.Code)
	}
	return decision, codes
}

// near is within 20% of the auto-post limit, either side, compared
// exactly in the company's currency: from l×8/10 to l×12/10.
func near(r replayed, lim limits) bool {
	if lim.autoPost.Sign() <= 0 {
		return false
	}
	gross := r.Gross.Major(r.Conversion.ToExp)
	return gross.Cmp(new(big.Rat).Mul(lim.autoPost, big.NewRat(8, 10))) >= 0 && gross.Cmp(new(big.Rat).Mul(lim.autoPost, big.NewRat(12, 10))) <= 0
}

// Capture implements configuration.Regressor: the window's invoices decided
// under doc, sampled so that every behaviour is covered, not just the
// common one.
func (g Regression) Capture(ctx context.Context, doc *config.Config, window time.Duration, maxCases int) ([]configuration.RegressionCase, error) {
	all, err := invoicesOnOrders(ctx, g.sources(), clock.Now(g.Now).Add(-window), 2000)
	if err != nil {
		return nil, err
	}
	rs := make([]decidedCase, 0, len(all))
	for _, r := range all {
		lim, err := companyLimits(doc.Company(r.Company))
		if err != nil {
			return nil, err
		}
		d, issues := outcome(lim, r)
		rs = append(rs, decidedCase{r, d, issues, near(r, lim)})
	}
	s := &sample{max: maxCases, picked: map[uuid.UUID]int{}}
	s.everyDecision(rs)
	s.everyIssue(rs)
	for _, r := range rs {
		if r.near {
			s.add(r, "near the auto-post limit")
		}
	}
	for _, r := range rs {
		s.add(r, "sample")
	}
	return s.cases, nil
}

// decidedCase is a replayed invoice with the outcome it gets under the
// captured configuration.
type decidedCase struct {
	replayed
	decision string
	issues   []string
	near     bool // near the auto-post limit
}

// sample collects regression cases up to max (0: no limit), each invoice
// once.
type sample struct {
	max    int
	cases  []configuration.RegressionCase
	picked map[uuid.UUID]int
}

// add takes an invoice into the sample for a reason, or adds the reason to
// its case when it is already in.
func (s *sample) add(r decidedCase, why string) {
	if i, ok := s.picked[r.Invoice.ID]; ok {
		// A case can cover several strata; record every reason.
		if why != "sample" && !strings.Contains(s.cases[i].Why, why) {
			s.cases[i].Why += "; " + why
		}
		return
	}
	if s.max > 0 && len(s.cases) >= s.max {
		return
	}
	s.picked[r.Invoice.ID] = len(s.cases)
	s.cases = append(s.cases, configuration.RegressionCase{CaseNo: int32(len(s.cases) + 1), InvoiceID: r.Invoice.ID, DocumentNumber: r.Invoice.DocumentNumber,
		LegalEntityID: r.Invoice.LegalEntityID, Company: r.Company, Why: why, ExpectedDecision: r.decision, ExpectedIssues: r.issues})
}

// everyDecision takes the first invoice of each decision.
func (s *sample) everyDecision(rs []decidedCase) {
	for _, decision := range []string{DecisionAutoPost, DecisionReview, DecisionException} {
		for _, r := range rs {
			if r.decision == decision {
				s.add(r, "decision "+decision)
				break
			}
		}
	}
}

// everyIssue takes the first invoice with each kind of match issue.
func (s *sample) everyIssue(rs []decidedCase) {
	seen := map[string]bool{}
	for _, r := range rs {
		for _, code := range r.issues {
			if !seen[code] {
				seen[code] = true
				s.add(r, "issue "+code)
			}
		}
	}
}

// Replay implements configuration.Regressor: each case decided again under
// doc on today's data; the cases whose outcome differs are returned.
func (g Regression) Replay(ctx context.Context, doc *config.Config, cases []configuration.RegressionCase) ([]configuration.RegressionFailure, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	cache := newReplayCache()
	failures := []configuration.RegressionFailure{}
	for _, c := range cases {
		v, err := load(ctx, tx, c.InvoiceID, false)
		if err != nil {
			return nil, err
		}
		r, err := replay(ctx, tx, g.sources(), v, cache)
		if err != nil {
			return nil, err
		}
		lim, err := companyLimits(doc.Company(r.Company))
		if err != nil {
			return nil, err
		}
		decision, issues := outcome(lim, r)
		if decision != c.ExpectedDecision || !slices.Equal(issues, c.ExpectedIssues) {
			failures = append(failures, configuration.RegressionFailure{Case: c, Decision: decision, Issues: issues})
		}
	}
	return failures, nil
}
