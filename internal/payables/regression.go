package payables

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
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
	Now           func() time.Time
}

// replayed is an invoice with everything its decision depends on.
type replayed struct {
	Invoice  Invoice
	Company  string
	Order    procurement.Order
	Invoiced map[int32]string
}

// invoicesOnOrders reads the invoices on purchase orders of the window,
// newest first, with their orders and what was invoiced before.
func invoicesOnOrders(ctx context.Context, les LegalEntities, orders Orders, since time.Time, limit int) ([]replayed, error) {
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
	companies := map[uuid.UUID]tenancy.Entity{}
	known := map[uuid.UUID]procurement.Order{}
	out := make([]replayed, 0, len(invoices))
	for _, v := range invoices {
		r, err := replay(ctx, tx, les, orders, v, companies, known)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// replay gathers one invoice's inputs, through the caches.
func replay(ctx context.Context, tx pgx.Tx, les LegalEntities, orders Orders, v Invoice, companies map[uuid.UUID]tenancy.Entity,
	known map[uuid.UUID]procurement.Order) (replayed, error) {
	le, ok := companies[v.LegalEntityID]
	if !ok {
		var err error
		if le, _, err = les.Entity(ctx, v.LegalEntityID); err != nil {
			return replayed{}, err
		}
		companies[v.LegalEntityID] = le
	}
	order, ok := known[*v.PurchaseOrderID]
	if !ok {
		var err error
		if order, _, err = orders.Order(ctx, *v.PurchaseOrderID); err != nil {
			return replayed{}, err
		}
		known[*v.PurchaseOrderID] = order
	}
	invoiced, err := invoicedQuantities(ctx, tx, *v.PurchaseOrderID, v.ID)
	if err != nil {
		return replayed{}, err
	}
	return replayed{Invoice: v, Company: le.Code, Order: order, Invoiced: invoiced}, nil
}

// outcome is a decision with its issue codes.
func outcome(co *config.Company, r replayed) (string, []string) {
	decision, issues := decide(co, r.Invoice, r.Order, r.Invoiced)
	codes := make([]string, 0, len(issues))
	for _, i := range issues {
		codes = append(codes, i.Code)
	}
	return decision, codes
}

// near is within 20% of the auto-post limit, either side.
func near(gross int64, co *config.Company, exp int) bool {
	if co == nil {
		return false
	}
	limit, _ := money.ParseDecimal(co.AutoPostLimit, exp) // validated with the document
	l := int64(limit)
	return l > 0 && gross >= l*8/10 && gross <= l*12/10
}

// Capture implements configuration.Regressor: the window's invoices decided
// under doc, sampled so that every behaviour is covered, not just the
// common one.
func (g Regression) Capture(ctx context.Context, doc *config.Config, window time.Duration, maxCases int) ([]configuration.RegressionCase, error) {
	all, err := invoicesOnOrders(ctx, g.LegalEntities, g.Orders, clock.Now(g.Now).Add(-window), 2000)
	if err != nil {
		return nil, err
	}
	rs := make([]decidedCase, 0, len(all))
	for _, r := range all {
		d, issues := outcome(doc.Company(r.Company), r)
		rs = append(rs, decidedCase{r, d, issues})
	}
	s := &sample{max: maxCases, picked: map[uuid.UUID]int{}}
	s.everyDecision(rs)
	s.everyIssue(rs)
	for _, r := range rs {
		if near(r.Invoice.Gross, doc.Company(r.Company), r.Invoice.Exponent) {
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
	companies := map[uuid.UUID]tenancy.Entity{}
	known := map[uuid.UUID]procurement.Order{}
	failures := []configuration.RegressionFailure{}
	for _, c := range cases {
		v, err := load(ctx, tx, c.InvoiceID, false)
		if err != nil {
			return nil, err
		}
		r, err := replay(ctx, tx, g.LegalEntities, g.Orders, v, companies, known)
		if err != nil {
			return nil, err
		}
		decision, issues := outcome(doc.Company(r.Company), r)
		if decision != c.ExpectedDecision || !slices.Equal(issues, c.ExpectedIssues) {
			failures = append(failures, configuration.RegressionFailure{Case: c, Decision: decision, Issues: issues})
		}
	}
	return failures, nil
}
