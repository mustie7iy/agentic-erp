package payables

import (
	"context"
	"fmt"
	"sort"
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

// Decisions the invoice flow takes on a matched invoice.
const (
	DecisionAutoPost  = "auto_post" // clean within the auto-post limit: the rule posts it
	DecisionReview    = "review"    // clean above the limit: a person posts it
	DecisionException = "exception" // the match found issues: held, decided on an approval request
)

// Simulation replays the supplier invoices on purchase orders of the last
// Window under two configurations, for the configuration context's change
// requests: which invoices would be decided differently (tolerances, the
// auto-post limit) and which posting rules move (account determination,
// tax codes). It reads committed data in the request's transaction and
// writes nothing.
type Simulation struct {
	LegalEntities LegalEntities
	Orders        Orders
	Window        time.Duration // how far back invoices are replayed; default 90 days
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
	Reason         string    `json:"reason"`
}

// Posting is a posting rule that moves, with the invoices and amount of
// the window it applies to.
type Posting struct {
	Company  string `json:"company"`
	Key      string `json:"key"`
	Base     string `json:"base"`
	Proposed string `json:"proposed"`
	Invoices int    `json:"invoices"`
	Amount   int64  `json:"amount"` // minor units of the company's currency
}

// examples is how many changes and breaks the detail lists.
const examples = 50

// Simulate implements configuration.Simulator.
func (s Simulation) Simulate(ctx context.Context, base, proposed *config.Config) (configuration.Simulation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return configuration.Simulation{}, err
	}
	window, limit := s.Window, s.Limit
	if window <= 0 {
		window = 90 * 24 * time.Hour
	}
	if limit <= 0 {
		limit = 2000
	}
	rows, _ := tx.Query(ctx, invoiceSelect+` WHERE i.purchase_order_id IS NOT NULL AND i.status IN ('parked', 'on_hold', 'posted') AND i.invoice_date >= $1
		ORDER BY i.invoice_date DESC, i.id DESC LIMIT $2`, clock.Now(s.Now).Add(-window), limit)
	invoices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invoice, error) { return scanInvoice(r) })
	if err != nil {
		return configuration.Simulation{}, err
	}
	companies := map[uuid.UUID]tenancy.Entity{}
	orders := map[uuid.UUID]procurement.Order{}
	counts := map[string]map[string]int{"base": {}, "proposed": {}}
	var changes []Decision
	var breaks []Broken
	moves := map[string]*Posting{}
	move := func(co, key, from, to string, amount int64) {
		if from == to {
			return
		}
		k := co + "/" + key
		if moves[k] == nil {
			moves[k] = &Posting{Company: co, Key: key, Base: from, Proposed: to}
		}
		moves[k].Invoices++
		moves[k].Amount += amount
	}
	for _, v := range invoices {
		le, ok := companies[v.LegalEntityID]
		if !ok {
			if le, _, err = s.LegalEntities.Entity(ctx, v.LegalEntityID); err != nil {
				return configuration.Simulation{}, err
			}
			companies[v.LegalEntityID] = le
		}
		order, ok := orders[*v.PurchaseOrderID]
		if !ok {
			if order, _, err = s.Orders.Order(ctx, *v.PurchaseOrderID); err != nil {
				return configuration.Simulation{}, err
			}
			orders[*v.PurchaseOrderID] = order
		}
		invoiced, err := invoicedQuantities(ctx, tx, *v.PurchaseOrderID, v.ID)
		if err != nil {
			return configuration.Simulation{}, err
		}
		before, after := base.Company(le.Code), proposed.Company(le.Code)
		if after == nil {
			breaks = append(breaks, Broken{v.ID, v.DocumentNumber, "company " + le.Code + " is not in the proposed configuration"})
			continue
		}
		missing := ""
		for _, it := range v.Items {
			if it.TaxCode != nil && taxCode(after, *it.TaxCode) == nil {
				missing = *it.TaxCode
			}
		}
		if missing != "" {
			breaks = append(breaks, Broken{v.ID, v.DocumentNumber, "tax code " + missing + " is not in the proposed configuration"})
			continue
		}
		b, bi := decide(before, v, order, invoiced)
		a, ai := decide(after, v, order, invoiced)
		counts["base"][b]++
		counts["proposed"][a]++
		if b != a || len(bi) != len(ai) {
			issues := make([]string, 0, len(ai))
			for _, i := range ai {
				issues = append(issues, i.Code)
			}
			changes = append(changes, Decision{v.ID, v.DocumentNumber, le.Code, b, a, issues})
		}
		if before != nil {
			move(le.Code, "account_determination.input_tax", before.AccountDetermination.InputTax, after.AccountDetermination.InputTax, v.Tax)
			move(le.Code, "account_determination.ap_reconciliation", before.AccountDetermination.APReconciliation, after.AccountDetermination.APReconciliation, v.Gross)
			for _, it := range v.Items {
				if it.TaxCode == nil {
					continue
				}
				if was := taxCode(before, *it.TaxCode); was != nil {
					tc := taxCode(after, *it.TaxCode)
					move(le.Code, "tax_codes."+*it.TaxCode+".rate_percent", was.RatePercent, tc.RatePercent, it.Amount)
					move(le.Code, "tax_codes."+*it.TaxCode+".account", was.Account, tc.Account, it.Amount)
				}
			}
		}
	}
	postings := []Posting{}
	for _, p := range moves {
		postings = append(postings, *p)
	}
	sort.Slice(postings, func(i, j int) bool { return postings[i].Company+postings[i].Key < postings[j].Company+postings[j].Key })
	detail := map[string]any{
		"window_days": int(window.Hours() / 24), "invoices": len(invoices),
		"decisions": map[string]any{"changed": len(changes), "unchanged": len(invoices) - len(changes) - len(breaks), "base": counts["base"], "proposed": counts["proposed"]},
		"changes":   first(changes, examples), "breaks": first(breaks, examples), "postings": postings,
	}
	summary := fmt.Sprintf("%d invoices of the last %d days replayed: %d decided differently, %d posting rules move", len(invoices), int(window.Hours()/24), len(changes), len(postings))
	if len(breaks) > 0 {
		summary += fmt.Sprintf("; %d cannot be evaluated under the proposal", len(breaks))
	}
	return configuration.Simulation{Passed: len(breaks) == 0, Summary: summary, Detail: detail}, nil
}

func first[T any](list []T, n int) []T {
	if list == nil {
		return []T{}
	}
	return list[:min(n, len(list))]
}

func taxCode(co *config.Company, code string) *config.TaxCode {
	for i := range co.TaxCodes {
		if co.TaxCodes[i].Code == code {
			return &co.TaxCodes[i]
		}
	}
	return nil
}

// decide is the invoice flow's decision under a company's configuration.
func decide(co *config.Company, v Invoice, order procurement.Order, invoiced map[int32]string) (string, []Issue) {
	var t config.Tolerance
	limit := ""
	if co != nil {
		t, limit = co.InvoiceTolerance, co.AutoPostLimit
	}
	issues := evaluate(v, order, invoiced, t)
	if len(issues) > 0 {
		return DecisionException, issues
	}
	l, _ := money.ParseDecimal(limit, v.Exponent) // validated with the document; "" is zero
	if v.Gross > int64(l) {
		return DecisionReview, nil
	}
	return DecisionAutoPost, nil
}
