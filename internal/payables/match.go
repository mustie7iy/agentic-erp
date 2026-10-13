package payables

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
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
}

const matchColumns = `id, supplier_invoice_id, evaluated_at, is_clean, issues, config_version, evaluated_by`

func scanMatch(row pgx.Row) (Match, error) {
	var m Match
	err := row.Scan(&m.ID, &m.InvoiceID, &m.EvaluatedAt, &m.IsClean, &m.Issues, &m.ConfigVersion, &m.EvaluatedBy)
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

// tolerance is the company's invoice tolerance.
func tolerance(doc *config.Config, company string) config.Tolerance {
	var t config.Tolerance
	for _, c := range doc.Companies {
		if c.Code == company {
			t = c.InvoiceTolerance
		}
	}
	return t
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
// receipts and what was invoiced before, within the tolerance. It is a
// pure function of its inputs, so a simulation can run it under a proposed
// configuration.
func evaluate(v Invoice, order procurement.Order, invoiced map[int32]string, t config.Tolerance) []Issue {
	issues := []Issue{}
	add := func(code string, item *int32, format string, a ...any) {
		issues = append(issues, Issue{Code: code, ItemNo: item, Message: fmt.Sprintf(format, a...)})
	}
	format := func(m int64) string { return money.Minor(m).Plain(v.Exponent) }
	var net int64
	for _, it := range v.Items {
		net += it.Amount
	}
	if net+v.Tax != v.Gross {
		add(IssueTotalMismatch, nil, "gross %s does not equal items %s + tax %s", format(v.Gross), format(net), format(v.Tax))
	}
	if order.Status != "released" {
		add(IssueOrderNotOpen, nil, "purchase order %s is %s", order.DocumentNumber, order.Status)
	}
	absolute, _ := money.ParseDecimal(t.PriceAbsolute, v.Exponent) // validated with the document; "" is zero
	pct, abs := int64(t.PricePercent), int64(absolute)
	items := map[int32]int{}
	for i, oi := range order.Items {
		items[oi.ItemNo] = i
	}
	for _, it := range v.Items {
		if it.POItemNo == nil {
			continue
		}
		oi := order.Items[items[*it.POItemNo]]
		want := expected(it.Quantity, oi.UnitPrice, oi.PriceUnit)
		allowed := max(abs, int64(money.Percent(money.Minor(want).Abs(), pct)))
		if diff := it.Amount - want; money.Minor(diff).Abs() > money.Minor(allowed) {
			add(IssuePriceVariance, &it.ItemNo, "order item %d: invoiced %s but the order price gives %s (variance %s, tolerance %s)",
				oi.ItemNo, format(it.Amount), format(want), format(diff), format(allowed))
		}
		total := new(big.Rat).Add(rat(it.Quantity), rat(orDefault(invoiced[oi.ItemNo], "0")))
		if total.Cmp(rat(oi.Received)) > 0 {
			add(IssueQtyReceived, &it.ItemNo, "order item %d: invoiced quantity %s exceeds received %s",
				oi.ItemNo, total.FloatString(6), oi.Received)
		}
	}
	return issues
}

// expected is quantity × unit price / price unit, rounded half away from zero.
func expected(quantity string, unitPrice int64, priceUnit int32) int64 {
	q, _ := new(big.Rat).SetString(quantity) // numeric columns render as decimals
	v := new(big.Rat).Mul(q, new(big.Rat).SetFrac64(unitPrice, int64(priceUnit)))
	n, d := v.Num(), v.Denom()
	r, m := new(big.Int).QuoRem(n, d, new(big.Int))
	if new(big.Int).Mul(m.Abs(m), big.NewInt(2)).Cmp(d) >= 0 {
		r.Add(r, big.NewInt(int64(n.Sign())))
	}
	return r.Int64()
}

func rat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s) // numeric columns render as decimals
	return r
}

// MatchInvoice evaluates a PO-based invoice against its order and goods
// receipts with the active tolerances and records the result. Issues put a
// parked invoice on hold and open an invoice_exception approval request;
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
		return Match{}, problem.Unprocessable("payables.no_purchase_order", "Only invoices on a purchase order are matched.")
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return Match{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID)
	if err != nil {
		return Match{}, err
	}
	order, _, err := e.Orders.Order(ctx, *v.PurchaseOrderID)
	if err != nil {
		return Match{}, err
	}
	invoiced, err := invoicedQuantities(ctx, tx, *v.PurchaseOrderID, id)
	if err != nil {
		return Match{}, err
	}
	issues := evaluate(v, order, invoiced, tolerance(doc, le.Code))
	m, err := scanMatch(tx.QueryRow(ctx, `INSERT INTO payables.match_result (tenant_id, id, supplier_invoice_id, evaluated_at, is_clean, issues,
		config_version, evaluated_by) VALUES ($1, $2, $3, clock_timestamp(), $4, $5, $6, $7) RETURNING `+matchColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), id, len(issues) == 0, issues, version, p.ID))
	if err != nil {
		return Match{}, fmt.Errorf("payables: record the match of invoice %s: %w", id, err)
	}
	events := []outbox.Event{{Context: "payables", Name: "supplier_invoice_matched", AggregateType: "payables.match_result", AggregateID: m.ID,
		Payload: map[string]any{"match_result_id": m.ID, "supplier_invoice_id": id, "evaluated_at": m.EvaluatedAt.UTC().Format(time.RFC3339Nano),
			"is_clean": m.IsClean, "config_version": version, "issues": issues}}}
	followUp, err := e.applyMatch(ctx, tx, p, v, m, len(issues))
	if err != nil {
		return Match{}, err
	}
	events = append(events, followUp...)
	return m, record(ctx, "payables.supplier_invoice.match", id, map[string]any{"match_result_id": m.ID, "issues": len(issues)}, events...)
}

// applyMatch acts on a recorded match: issues hold the invoice and open an
// invoice_exception request (unless a held invoice already has one); a
// clean match releases a hold the match had set. It returns the events of
// the transition, if any.
func (e *Endpoints) applyMatch(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, m Match, issueCount int) ([]outbox.Event, error) {
	switch {
	case !m.IsClean && (v.Status == Parked || v.WorkflowRequestID == nil):
		held, err := e.holdForException(ctx, tx, p, v, issueCount)
		if err != nil {
			return nil, err
		}
		return []outbox.Event{heldEvent(held)}, nil
	case m.IsClean && v.Status == OnHold && len(*v.HoldReason) >= len(matchHold) && (*v.HoldReason)[:len(matchHold)] == matchHold:
		if err := e.Approvals.Withdraw(ctx, approvals.InvoiceException, v.ID, "The invoice now matches."); err != nil {
			return nil, err
		}
		released, err := transition(ctx, tx, p, v, Parked, nil, nil)
		if err != nil {
			return nil, err
		}
		return []outbox.Event{releasedEvent(released, nil)}, nil
	}
	return nil, nil
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

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
