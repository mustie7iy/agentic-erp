package payables

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Issue is one three-way match issue.
type Issue struct {
	Code    string `json:"code"`
	ItemNo  *int32 `json:"item_no,omitempty"`
	Message string `json:"message"`
}

// Issue codes, as in the prototype (internal/core/ap.go).
const (
	IssueTotalMismatch = "tax_or_total_mismatch"
	IssueOrderNotOpen  = "order_not_open"
	IssuePriceVariance = "price_variance"
	IssueQtyReceived   = "quantity_exceeds_received"
	// IssueOrderItemMissing is an invoice item naming an order item its
	// order does not have, which procurement's invariants should prevent.
	IssueOrderItemMissing = "order_item_missing"
)

// Match is a row of payables.match_result.
type Match struct {
	ID            uuid.UUID
	InvoiceID     uuid.UUID
	EvaluatedAt   time.Time
	IsClean       bool
	Issues        []Issue
	ConfigVersion int32
	EvaluatedBy   uuid.UUID
	// InvoiceVersion is the invoice version the match left the invoice at,
	// the only version it lets post (F13); nil for matches recorded before
	// migration 000040, which count for no version.
	InvoiceVersion *int32
	// Decision is the invoice flow's decision on a clean match, set by
	// MatchInvoice only: it is not stored, as the limit and the rate it was
	// taken with may change before the invoice is posted. A match with
	// issues has none: the invoice is held and decided on its exception
	// request.
	Decision *Verdict
}

const matchColumns = `id, supplier_invoice_id, evaluated_at, is_clean, issues, config_version, evaluated_by, invoice_version`

func scanMatch(row pgx.Row) (Match, error) {
	var m Match
	err := row.Scan(&m.ID, &m.InvoiceID, &m.EvaluatedAt, &m.IsClean, &m.Issues, &m.ConfigVersion, &m.EvaluatedBy, &m.InvoiceVersion)
	return m, errs.Wrapf(err, "scan match result")
}

// GetMatch returns a match result of an invoice the caller may see.
func GetMatch(ctx context.Context, id uuid.UUID) (Match, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Match{}, err
	}
	m, err := scanMatch(tx.QueryRow(ctx, `SELECT `+matchColumns+` FROM payables.match_result WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Match{}, problem.NotFound("No match result with this id.")
	}
	if err != nil {
		return Match{}, fmt.Errorf("payables: read match result %s: %w", id, err)
	}
	if _, err := get(ctx, tx, m.InvoiceID, false); err != nil {
		if problem.From(err).Status == 404 {
			err = problem.NotFound("No match result with this id.")
		}
		return Match{}, err
	}
	return m, nil
}

// limits are a company's configured amounts for the invoice flow. The
// absolute ones are in the company's currency (F12) and read exactly:
// validated with that currency's digits, they are compared with an
// invoice's amounts converted into it, never read at the invoice's digits.
type limits struct {
	pricePercent  int64
	priceAbsolute *big.Rat // major units of the company's currency
	absoluteText  string   // as configured, for messages
	autoPost      *big.Rat // major units of the company's currency
	autoPostText  string   // as configured, for reasons
}

// companyLimits reads a company's tolerances and auto-post limit; a
// company the configuration does not have has none.
func companyLimits(co *config.Company) (limits, error) {
	if co == nil {
		return limits{priceAbsolute: new(big.Rat), absoluteText: "0", autoPost: new(big.Rat), autoPostText: "0"}, nil
	}
	absolute, err := money.ParseDecimalRat(co.InvoiceTolerance.PriceAbsolute)
	if err != nil {
		return limits{}, fmt.Errorf("payables: company %s: invoice_tolerance.price_absolute: %w", co.Code, err)
	}
	autoPost, err := money.ParseDecimalRat(co.AutoPostLimit)
	if err != nil {
		return limits{}, fmt.Errorf("payables: company %s: auto_post_limit: %w", co.Code, err)
	}
	return limits{pricePercent: int64(co.InvoiceTolerance.PricePercent), priceAbsolute: absolute,
		absoluteText: strings.TrimSpace(co.InvoiceTolerance.PriceAbsolute), autoPost: autoPost,
		autoPostText: strings.TrimSpace(co.AutoPostLimit)}, nil
}

// invoicedQuantities sums, per order item, what other posted invoices
// already invoiced on the order.
func invoicedQuantities(ctx context.Context, tx pgx.Tx, order, exclude uuid.UUID) (map[int32]string, error) {
	rows, _ := tx.Query(ctx, `SELECT t.po_item_no, trim_scale(sum(t.quantity))::text FROM payables.supplier_invoice_item t
		JOIN payables.supplier_invoice i ON i.tenant_id = t.tenant_id AND i.id = t.supplier_invoice_id
		WHERE t.purchase_order_id = $1 AND i.status = 'posted' AND i.id <> $2 AND NOT t.is_removed GROUP BY 1`, order, exclude)
	invoiced := map[int32]string{}
	var no int32
	var qty string
	if _, err := pgx.ForEachRow(rows, []any{&no, &qty}, func() error { invoiced[no] = qty; return nil }); err != nil {
		return nil, fmt.Errorf("payables: read quantities invoiced on order %s: %w", order, err)
	}
	return invoiced, nil
}

// evaluate is the three-way match: the invoice against its order, the
// receipts and what was invoiced before, within the company's tolerances,
// conv converting the invoice's amounts into the company's currency. It is
// a pure function of its inputs, so a simulation can run it under a
// proposed configuration.
func evaluate(v Invoice, order procurement.Order, invoiced map[int32]string, lim limits, conv ledger.Conversion) []Issue {
	issues := []Issue{}
	add := func(code string, item *int32, format string, a ...any) {
		issues = append(issues, Issue{Code: code, ItemNo: item, Message: fmt.Sprintf(format, a...)})
	}
	format := func(m money.Minor) string { return m.Plain(v.Exponent) }
	if why := totalMismatch(v, format); why != "" {
		add(IssueTotalMismatch, nil, "%s", why)
	}
	if order.Status != "released" {
		add(IssueOrderNotOpen, nil, "purchase order %s is %s", order.DocumentNumber, order.Status)
	}
	items := map[int32]int{}
	for i, oi := range order.Items {
		items[oi.ItemNo] = i
	}
	for _, it := range v.Items {
		if it.POItemNo == nil {
			continue
		}
		// An item with an order item is on the invoice's order (the invoice
		// is for one order, and replay refuses one without it), and its
		// order item was checked to exist when the invoice was written. A
		// released order's items should no longer change, but that is
		// procurement's invariant: an item missing here is an issue, never
		// a comparison with another order item.
		i, ok := items[*it.POItemNo]
		if !ok {
			add(IssueOrderItemMissing, &it.ItemNo, "order item %d is not on purchase order %s", *it.POItemNo, order.DocumentNumber)
			continue
		}
		oi := order.Items[i]
		if why := priceVariance(it, oi, lim, conv, format); why != "" {
			add(IssuePriceVariance, &it.ItemNo, "order item %d: %s", oi.ItemNo, why)
		}
		total := new(big.Rat).Add(money.ParseRat(it.Quantity), money.ParseRat(cmp.Or(invoiced[oi.ItemNo], "0")))
		if total.Cmp(money.ParseRat(oi.Received)) > 0 {
			add(IssueQtyReceived, &it.ItemNo, "order item %d: invoiced quantity %s exceeds received %s",
				oi.ItemNo, total.FloatString(6), oi.Received)
		}
	}
	return issues
}

// totalMismatch says why the items plus tax do not equal the gross amount,
// or "" when they do. Sums out of range never equal it.
func totalMismatch(v Invoice, format func(money.Minor) string) string {
	n := len(v.Items)
	amounts := make([]money.Minor, n+1)
	for i, it := range v.Items {
		amounts[i] = it.Amount
	}
	amounts[n] = v.Tax
	if total, err := money.Sum(amounts...); err == nil && total == v.Gross {
		return ""
	}
	net, err := money.Sum(amounts[:n]...)
	if err != nil {
		return "the items add up to more than an amount holds"
	}
	return fmt.Sprintf("gross %s does not equal items %s + tax %s", format(v.Gross), format(net), format(v.Tax))
}

// priceVariance says why the invoiced amount of an item is outside the
// tolerance of what the order price gives for its quantity (quantity ×
// unit price / price unit, rounded half away from zero), or "" when it is
// within. The variance is within when it is within the percentage of the
// expected amount, or when its value in the company's currency is within
// the absolute tolerance. An expected amount or variance out of range is
// outside.
func priceVariance(it Item, oi procurement.OrderItem, lim limits, conv ledger.Conversion, format func(money.Minor) string) string {
	want, err := money.Scale(oi.UnitPrice, it.Quantity, int64(oi.PriceUnit))
	if err != nil {
		return fmt.Sprintf("invoiced %s but the order price gives an amount out of range", format(it.Amount))
	}
	diff, err := money.Diff(it.Amount, want)
	if err != nil {
		return fmt.Sprintf("invoiced %s but the order price gives %s, a variance out of range", format(it.Amount), format(want))
	}
	// Only a percentage above 100 of an amount near the range fails; the
	// item is then flagged rather than let through.
	byPct, err := money.Percent(want.Abs(), lim.pricePercent)
	if err != nil {
		return fmt.Sprintf("invoiced %s but the order price gives %s, whose percentage tolerance is out of range", format(it.Amount), format(want))
	}
	if diff <= byPct && diff >= -byPct {
		return ""
	}
	// The variance's exact value in the company's currency is compared:
	// rounded to its digits first, 5.004 EUR would pass 5.00 EUR (F13).
	inCompany, err := conv.Exact(diff)
	if err != nil {
		return fmt.Sprintf("invoiced %s but the order price gives %s, a variance that cannot be converted into %s", format(it.Amount), format(want), conv.To)
	}
	if inCompany.Abs(inCompany).Cmp(lim.priceAbsolute) <= 0 {
		return ""
	}
	return fmt.Sprintf("invoiced %s but the order price gives %s (variance %s, tolerance %s or %s %s)",
		format(it.Amount), format(want), format(diff), format(byPct), lim.absoluteText, conv.To)
}

// MatchInvoice evaluates a PO-based invoice against its order and goods
// receipts with the active tolerances and records the result. Issues put a
// parked invoice on hold and open an invoice_exception approval request,
// unless its exception was approved at its current version (applyMatch);
// a clean result releases a hold the match had set.
func (e *Endpoints) MatchInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Match, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Match{}, err
	}
	v, err := get(ctx, tx, id, true)
	if err != nil {
		return Match{}, err
	}
	if err := ifMatch(v.Version); err != nil {
		return Match{}, err
	}
	if err := requireStatus(v, "match", Parked, OnHold); err != nil {
		return Match{}, err
	}
	if v.PurchaseOrderID == nil {
		return Match{}, noPurchaseOrder()
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return Match{}, err
	}
	// The inputs and the decision are those a simulation or regression
	// suite replays, but a missing rate refuses the match (422
	// ledger.no_exchange_rate) rather than recording one.
	r, err := replay(ctx, tx, sources{e.LegalEntities, e.Orders, e.Rates}, v, newReplayCache())
	if err != nil {
		return Match{}, err
	}
	if r.noRate != nil {
		return Match{}, r.noRate
	}
	lim, err := companyLimits(doc.Company(r.Company))
	if err != nil {
		return Match{}, err
	}
	decision, issues := decide(lim, r)
	// The hold or release the match causes comes first, so the result
	// records the version the invoice is left at: the one it lets post.
	after, followUp, err := e.applyMatch(ctx, tx, p, v, len(issues))
	if err != nil {
		return Match{}, err
	}
	m, err := scanMatch(tx.QueryRow(ctx, `INSERT INTO payables.match_result (tenant_id, id, supplier_invoice_id, evaluated_at, is_clean, issues,
		config_version, evaluated_by, invoice_version) VALUES ($1, $2, $3, clock_timestamp(), $4, $5, $6, $7, $8) RETURNING `+matchColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), id, len(issues) == 0, issues, version, p.ID, after.Version))
	if err != nil {
		return Match{}, fmt.Errorf("payables: record the match of invoice %s: %w", id, err)
	}
	events := append([]outbox.Event{{Context: "payables", Name: "supplier_invoice_matched", AggregateType: "payables.match_result", AggregateID: m.ID,
		Payload: map[string]any{"match_result_id": m.ID, "supplier_invoice_id": id, "evaluated_at": m.EvaluatedAt.UTC().Format(time.RFC3339Nano),
			"is_clean": m.IsClean, "config_version": version, "issues": issues}}}, followUp...)
	detail := map[string]any{"match_result_id": m.ID, "issues": len(issues)}
	if m.IsClean {
		m.Decision = &decision
		detail["decision"] = decision.Outcome
	}
	return m, record(ctx, "payables.supplier_invoice.match", id, detail, events...)
}

// applyMatch acts on a match with issueCount issues: issues hold the
// invoice and open an invoice_exception request (unless a held invoice
// already has one); a clean match releases a hold the match had set. It
// returns the invoice as the match leaves it and the events of the
// transition, if any. A parked invoice whose exception was approved at its
// current version is left as it is: the approval covers the invoice as it
// stands, which :post accepts whatever a later match finds (F16), so
// holding it would only ask the approvers again, on every match (F18).
func (e *Endpoints) applyMatch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, issueCount int) (Invoice, []outbox.Event, error) {
	if issueCount > 0 && v.Status == Parked && v.WorkflowRequestID != nil {
		var approved *int32
		if err := tx.QueryRow(ctx, `SELECT exception_version FROM payables.supplier_invoice WHERE id = $1`, v.ID).Scan(&approved); err != nil {
			return Invoice{}, nil, fmt.Errorf("payables: read the approval of invoice %s: %w", v.ID, err)
		}
		if approvedAsIs(v, approved) {
			return v, nil, nil
		}
	}
	switch {
	case issueCount > 0 && (v.Status == Parked || v.WorkflowRequestID == nil):
		held, err := e.holdForException(ctx, tx, p, v, issueCount)
		if err != nil {
			return Invoice{}, nil, err
		}
		return held, []outbox.Event{heldEvent(held)}, nil
	// An on-hold invoice always has a hold reason: the table's CHECK
	// ((status = 'on_hold') = (hold_reason IS NOT NULL)), migration 000007.
	case issueCount == 0 && v.Status == OnHold && strings.HasPrefix(*v.HoldReason, matchHold):
		if err := e.Approvals.Withdraw(ctx, approvals.InvoiceException, v.ID, "The invoice now matches."); err != nil {
			return Invoice{}, nil, err
		}
		released, err := transition(ctx, tx, p, v, Parked, nil, nil)
		if err != nil {
			return Invoice{}, nil, err
		}
		return released, []outbox.Event{releasedEvent(released, nil)}, nil
	}
	return v, nil, nil
}

// holdForException opens an invoice_exception approval request and puts
// the invoice on hold, linked to the request when one is needed.
func (e *Endpoints) holdForException(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, issueCount int) (Invoice, error) {
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.InvoiceException, ObjectID: v.ID,
		Title: "Invoice exception " + v.DocumentNumber + " (" + v.SupplierReference + ")", Amount: &v.Gross, Currency: &v.Currency})
	if err != nil {
		return Invoice{}, err
	}
	reason := fmt.Sprintf("%s%d issue(s)", matchHold, issueCount)
	var r *uuid.UUID
	if needed {
		r = &request
	}
	return transition(ctx, tx, p, v, OnHold, &reason, r)
}
