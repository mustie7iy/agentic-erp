package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// creditScene is returnsScene's billed and posted orders with a confirmed
// returns order RO1 of the billing document BD1 (doc) for 3 of billing
// item 1 (5 EA billed at 100) and 1 of billing item 2 (2 EA at 100), of
// which a receipt took 2 of item 1, and a credit memo request CR1
// (request) to write.
type creditScene struct {
	returnsScene
	request uuid.UUID
}

const (
	creditHeader = `INSERT INTO sales.credit_memo_request (tenant_id, id, document_number, legal_entity_id, customer_id, billing_document_id,
		sales_order_id, returns_order_id, request_date, reason, status, rejection_reason, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '2026-03-25', $9, $10, $11, $12, $12)`
	creditLine = `INSERT INTO sales.credit_memo_request_item (tenant_id, request_id, item_no, billing_document_id, billing_item_no, returns_order_id,
		quantity, net_amount) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	creditPost = `UPDATE sales.credit_memo_request SET status = 'posted', posting_date = '2026-03-26', customer_invoice_id = gen_random_uuid(),
		customer_invoice_number = 'CI1', journal_entry_year = 2026, journal_entry_id = gen_random_uuid(), version = version + 1, updated_by = tenant_id WHERE id = $1`
	creditDecision = `UPDATE sales.credit_memo_request SET status = $2, rejection_reason = $3, version = version + 1, updated_by = tenant_id WHERE id = $1`
)

func newCreditScene() creditScene { return creditScene{returnsScene: newReturnsScene(), request: id()} }

// received is the scene's returns order, confirmed with its receipt, followed by more.
func (s creditScene) received(more ...stmt) []stmt {
	receipt := id()
	return s.billed(append([]stmt{s.made(), s.line(s.returns, 1, 1, "3"), s.line(s.returns, 2, 2, "1"),
		step(`UPDATE sales.returns_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`, s.returns),
		step(`INSERT INTO sales.return_receipt (tenant_id, id, returns_order_id, receipt_no, receipt_date, created_by) VALUES ($1, $2, $3, 1, '2026-03-22', $1)`,
			s.tenant, receipt, s.returns),
		step(`INSERT INTO sales.return_receipt_item (tenant_id, receipt_id, returns_order_id, item_no, quantity) VALUES ($1, $2, $3, 1, 2)`,
			s.tenant, receipt, s.returns)}, more...)...)
}

// header is a request of the scene's billing document: id, number, its
// returns order (nil for one by amount), reason, status and rejection.
func (s creditScene) header(request uuid.UUID, number string, returns *uuid.UUID, reason any, status string, rejection any) stmt {
	return step(creditHeader, s.tenant, request, number, s.company, s.customer, s.doc, s.order, returns, reason, status, rejection, s.actor)
}

// byAmount is the scene's request by amount, open.
func (s creditScene) byAmount() stmt { return s.header(s.request, "CR1", nil, "Goodwill", "open", nil) }

// forReturns is the scene's request for its returns order, open.
func (s creditScene) forReturns() stmt {
	return s.header(s.request, "CR1", &s.returns, nil, "open", nil)
}

// amount is an item of a request by amount: its number, the billing item and the amount.
func (s creditScene) amount(request uuid.UUID, no, billingItem, net int) stmt {
	return step(creditLine, s.tenant, request, no, s.doc, billingItem, nil, nil, net)
}

// quantity is an item of a request for the returns order: its number, the billing item, the quantity and the amount.
func (s creditScene) quantity(request uuid.UUID, no, billingItem int, quantity string, net int) stmt {
	return step(creditLine, s.tenant, request, no, s.doc, billingItem, s.returns, quantity, net)
}

// TestCreditMemoRequestRules (T85c, migration 000313; ADR-0067, As built
// T85c): a credit memo request is created open against a posted billing
// document of its company and customer with that document's sales order,
// and, when it names one, a returns order of that document; it is
// numbered once per company; it has a returns order or a reason; it is
// approved or rejected once, a rejection with its reason, and never
// otherwise changed or deleted, by its owner neither; its items credit
// amounts above zero of the billing document's items, each once, and of a
// returns order's item a quantity above zero exactly when the request is
// for that returns order; the requests not rejected never ask for more of
// a billing item than it billed nor credit more of a returns order's item
// than was received; an item is never changed or deleted.
func TestCreditMemoRequestRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s creditScene) error
		want string // empty: allowed
	}{
		{"a request by amount up to what was billed, then approved", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 60), s.amount(s.request, 2, 2, 100), step(creditDecision, s.request, "approved", nil))...)
		}, ""},
		{"a request for the returns order up to what was received, then rejected", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.forReturns(), s.quantity(s.request, 1, 1, "2", 40), step(creditDecision, s.request, "rejected", "Damaged"))...)
		}, ""},
		{"a rejected request frees what it asked for", func(tx pgx.Tx, s creditScene) error {
			second := id()
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 100), step(creditDecision, s.request, "rejected", "No"),
				s.header(second, "CR2", nil, "Goodwill", "open", nil), s.amount(second, 1, 1, 100))...)
		}, ""},
		{"more than was billed", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 101))...)
		}, "credited of 100 billed"},
		{"more than is left after an earlier request", func(tx pgx.Tx, s creditScene) error {
			second := id()
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 60), s.header(second, "CR2", &s.returns, nil, "open", nil),
				s.quantity(second, 1, 1, "2", 41))...)
		}, "credited of 100 billed"},
		{"more than was billed, by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(`SET LOCAL ROLE erp_owner`), s.byAmount(), s.amount(s.request, 1, 1, 101))...)
		}, "credited of 100 billed"},
		{"more than was received", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.forReturns(), s.quantity(s.request, 1, 1, "2.000001", 40))...)
		}, "received"},
		{"more than is left received after an earlier request", func(tx pgx.Tx, s creditScene) error {
			second := id()
			return all(tx, s.received(s.forReturns(), s.quantity(s.request, 1, 1, "1", 20), s.header(second, "CR2", &s.returns, nil, "open", nil),
				s.quantity(second, 1, 1, "1.5", 30))...)
		}, "received"},
		{"an item of a request by amount naming the returns order", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.quantity(s.request, 1, 1, "1", 20))...)
		}, "does not name the returns order its request is for"},
		{"an item of a request for the returns order naming none", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.forReturns(), s.amount(s.request, 1, 1, 20))...)
		}, "does not name the returns order its request is for"},
		{"a returns order's item without a quantity", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.forReturns(), step(creditLine, s.tenant, s.request, 1, s.doc, 1, s.returns, nil, 20))...)
		}, "credit_memo_request_item_quantity"},
		{"a quantity without a returns order", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditLine, s.tenant, s.request, 1, s.doc, 1, nil, "1", 20))...)
		}, "credit_memo_request_item_quantity"},
		{"a quantity of zero", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.forReturns(), s.quantity(s.request, 1, 1, "0", 20))...)
		}, "credit_memo_request_item_quantity"},
		{"an amount of zero", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 0))...)
		}, "credit_memo_request_item_amount_positive"},
		{"an item numbered zero", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 0, 1, 1))...)
		}, "credit_memo_request_item_item_no_check"},
		{"a billing item twice in a request", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 1), s.amount(s.request, 2, 1, 1))...)
		}, "credit_memo_request_item_once"},
		{"an item the billing document does not have", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 3, 1))...)
		}, "credit_memo_request_item_billing_item_fkey"},
		{"an item of another billing document than the request's", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditLine, s.tenant, s.request, 1, s.otherDoc, 1, nil, nil, 1))...)
		}, "credit_memo_request_item_request_fkey"},
		{"a billing item the returns order does not return", func(tx pgx.Tx, s creditScene) error {
			second := id()
			return all(tx, s.received(step(returnsHeader, s.tenant, second, "RO2", s.company, s.customer, s.doc, s.order, "other", nil, "draft", s.actor),
				s.line(second, 1, 2, "1"), s.header(s.request, "CR1", &second, nil, "open", nil),
				step(creditLine, s.tenant, s.request, 1, s.doc, 1, second, "1", 1))...)
		}, "credit_memo_request_item_returns_item_fkey"},
		{"an item changed", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 1), step(`UPDATE sales.credit_memo_request_item SET net_amount = 2`))...)
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 1), step(`DELETE FROM sales.credit_memo_request_item`))...)
		}, "permission denied"},
		{"an item changed by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 1), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.credit_memo_request_item SET net_amount = 2`))...)
		}, "insert-only"},
		{"an item deleted by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 1), step(`SET LOCAL ROLE erp_owner`),
				step(`DELETE FROM sales.credit_memo_request_item`))...)
		}, "insert-only"},
		{"a billing document that is not posted", func(tx pgx.Tx, s creditScene) error {
			return all(tx, append(s.billedOrder(s.order, s.doc, "1", "5", "2"), s.byAmount())...)
		}, "its billing document is not a posted one"},
		{"another customer than the billing document's", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(creditHeader, s.tenant, s.request, "CR1", s.company, id(), s.doc, s.order, nil, "x", "open", nil, s.actor))...)
		}, "its billing document is not a posted one"},
		{"another sales order than the billing document's", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(creditHeader, s.tenant, s.request, "CR1", s.company, s.customer, s.doc, s.otherOrder, nil, "x", "open", nil, s.actor))...)
		}, "credit_memo_request_billing_document_fkey"},
		{"a returns order of another billing document", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(creditHeader, s.tenant, s.request, "CR1", s.company, s.customer, s.otherDoc, s.otherOrder, s.returns, nil, "open", nil, s.actor))...)
		}, "credit_memo_request_returns_order_fkey"},
		{"created approved, by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(`SET LOCAL ROLE erp_owner`), s.header(s.request, "CR1", nil, "x", "approved", nil))...)
		}, "it is created open"},
		{"more than was received, by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(`SET LOCAL ROLE erp_owner`), s.forReturns(), s.quantity(s.request, 1, 1, "2.000001", 40))...)
		}, "received"},
		{"an item of a request by amount naming the returns order, by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(`SET LOCAL ROLE erp_owner`), s.byAmount(), s.quantity(s.request, 1, 1, "1", 20))...)
		}, "does not name the returns order its request is for"},
		{"created approved", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, "CR1", nil, "x", "approved", nil))...)
		}, "it is created open"},
		{"neither a returns order nor a reason", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, "CR1", nil, nil, "open", nil))...)
		}, "credit_memo_request_basis"},
		{"a reason of white space", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, "CR1", nil, " ", "open", nil))...)
		}, "credit_memo_request_reason_check"},
		{"a reason of 501 characters", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, "CR1", nil, strings.Repeat("x", 501), "open", nil))...)
		}, "credit_memo_request_reason_check"},
		{"a number of 41 characters", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, strings.Repeat("1", 41), nil, "x", "open", nil))...)
		}, "credit_memo_request_document_number_check"},
		{"a number issued twice in the company", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.header(id(), "CR1", nil, "x", "open", nil))...)
		}, "credit_memo_request_number_once"},
		{"an unknown status", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, "CR1", nil, "x", "cancelled", nil))...)
		}, "credit_memo_request_status_check"},
		{"an open request with a rejection reason", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.header(s.request, "CR1", nil, "x", "open", "No"))...)
		}, "credit_memo_request_rejection"},
		{"rejected without a reason", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "rejected", nil))...)
		}, "credit_memo_request_rejection"},
		{"a rejection reason of white space", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "rejected", " "))...)
		}, "credit_memo_request_rejection_reason_check"},
		{"approved twice", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "approved", nil), step(creditDecision, s.request, "approved", nil))...)
		}, "only an open one changes"},
		{"rejected once approved", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "approved", nil), step(creditDecision, s.request, "rejected", "No"))...)
		}, "only an open one changes"},
		{"an open request's version moved without a decision", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(`UPDATE sales.credit_memo_request SET version = version + 1 WHERE id = $1`, s.request))...)
		}, "only an open one changes"},
		{"reopened by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "approved", nil), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.credit_memo_request SET status = 'open' WHERE id = $1`, s.request))...)
		}, "only an open one changes"},
		{"an approved request posted (T85d, migration 000314)", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 10), step(creditDecision, s.request, "approved", nil), step(creditPost, s.request))...)
		}, ""},
		{"an open request posted", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditPost, s.request))...)
		}, "only an open one changes"},
		{"a rejected request posted", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "rejected", "No"), step(creditPost, s.request))...)
		}, "only an open one changes"},
		{"a posted request changed again, by the tables' owner", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "approved", nil), step(creditPost, s.request), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.credit_memo_request SET status = 'approved', posting_date = NULL, customer_invoice_id = NULL, customer_invoice_number = NULL,
					journal_entry_year = NULL, journal_entry_id = NULL WHERE id = $1`, s.request))...)
		}, "only an open one changes"},
		{"posted without its credit memo", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "approved", nil),
				step(`UPDATE sales.credit_memo_request SET status = 'posted', posting_date = '2026-03-26', version = version + 1 WHERE id = $1`, s.request))...)
		}, "credit_memo_request_posted"},
		{"an open request naming a credit memo", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(step(`INSERT INTO sales.credit_memo_request (tenant_id, id, document_number, legal_entity_id, customer_id, billing_document_id,
				sales_order_id, request_date, reason, status, customer_invoice_id, created_by, updated_by)
				VALUES ($1, $2, 'CR1', $3, $4, $5, $6, '2026-03-25', 'x', 'open', gen_random_uuid(), $7, $7)`, s.tenant, s.request, s.company, s.customer, s.doc, s.order, s.actor))...)
		}, "credit_memo_request_posted"},
		{"one credit memo for two requests", func(tx pgx.Tx, s creditScene) error {
			second := id()
			return all(tx, s.received(s.byAmount(), step(creditDecision, s.request, "approved", nil), step(creditPost, s.request),
				s.header(second, "CR2", nil, "x", "open", nil), step(creditDecision, second, "approved", nil),
				step(`UPDATE sales.credit_memo_request SET status = 'posted', posting_date = '2026-03-26', journal_entry_year = 2026, journal_entry_id = gen_random_uuid(),
					customer_invoice_number = 'CI2', customer_invoice_id = (SELECT customer_invoice_id FROM sales.credit_memo_request WHERE id = $2),
					version = version + 1 WHERE id = $1`, second, s.request))...)
		}, "credit_memo_request_invoice_once"},
		{"the reason changed", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(`UPDATE sales.credit_memo_request SET reason = 'other'`))...)
		}, "permission denied"},
		{"a request deleted", func(tx pgx.Tx, s creditScene) error {
			return all(tx, s.received(s.byAmount(), step(`DELETE FROM sales.credit_memo_request`))...)
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newCreditScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return c.run(tx, s) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("another tenant sees none", func(t *testing.T) {
		s := newCreditScene()
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, s.received(s.byAmount(), s.amount(s.request, 1, 1, 1), step(`SELECT set_config('app.tenant_id', $1, true)`, id().String()))...); err != nil {
				return err
			}
			var requests, items int
			if err := tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM sales.credit_memo_request), (SELECT count(*) FROM sales.credit_memo_request_item)`).Scan(&requests, &items); err != nil {
				return err
			}
			if requests != 0 || items != 0 {
				return fmt.Errorf("another tenant reads %d requests and %d items, want none", requests, items)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
