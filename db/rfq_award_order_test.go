package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The award of an RFQ to a purchase order and the award's reason (T82f,
// migration 000361): an awarded RFQ names one document, a contract or an
// order of its company, in its currency, with the winning quotation's
// supplier; an RFQ from a requisition is awarded to an order naming the
// requisition; the reason is the award's alone, and stays with it.

const (
	orderAwardSQL = `UPDATE procurement.rfq SET status = 'awarded', purchase_order_id = $2, award_reason = $3, version = version + 1 WHERE id = $1`
	draftOrderSQL = `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, requisition_id, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-06-15', 'NT30', $7, 'draft', $8, $8)`
)

// orderAwardFixture is newAwards with draft orders of its legal entity in
// EUR, one with each supplier and one in USD, and a published RFQ from a
// requisition in sourcing (sourced, quoted by the supplier: sourcedQuote)
// with an order naming that requisition (sourcedOrder).
type orderAwardFixture struct {
	awardFixture
	order, otherOrder, usdOrder, sourced, sourcedQuote, sourcedOrder, requisition uuid.UUID
}

func newOrderAwards(t *testing.T) orderAwardFixture {
	t.Helper()
	f := orderAwardFixture{awardFixture: newAwards(t), order: id(), otherOrder: id(), usdOrder: id(), sourced: id(), sourcedQuote: id(),
		sourcedOrder: id(), requisition: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{draftOrderSQL, []any{f.tenant, f.order, "PO1", f.entity, f.supplier, "EUR", nil, f.actor}},
			{draftOrderSQL, []any{f.tenant, f.otherOrder, "PO2", f.entity, f.otherSupplier, "EUR", nil, f.actor}},
			{draftOrderSQL, []any{f.tenant, f.usdOrder, "PO3", f.entity, f.supplier, "USD", nil, f.actor}},
			{requisitionSQL, []any{f.tenant, f.requisition, "PR1", f.entity, f.actor, "EUR", "in_sourcing"}},
			{requisitionItemSQL, []any{f.tenant, f.requisition, 1, f.expense, false}},
			{sourcingSQL, []any{f.tenant, f.sourced, "RQ7", f.entity, "EUR", f.requisition, f.actor}},
			{sourcedItemSQL, []any{f.tenant, f.sourced, 1, "PR1/1", f.expense, 1}},
			{rfqSupplierSQL, []any{f.tenant, f.sourced, f.supplier, 1}},
			{`UPDATE procurement.rfq SET status = 'published', version = version + 1 WHERE id = $1`, []any{f.sourced}},
			{quotationSQL, []any{f.tenant, f.sourcedQuote, f.sourced, f.supplier, "Q-7", "recorded", f.actor}},
			{quotationItemSQL, []any{f.tenant, f.sourcedQuote, f.sourced, 1, 100, 1}},
			{draftOrderSQL, []any{f.tenant, f.sourcedOrder, "PO4", f.entity, f.supplier, "EUR", f.requisition, f.actor}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

// orderAwarding marks the quotation awarded, then awards its RFQ to the
// order with the reason, as the application does.
func orderAwarding(quotation, rfq, order uuid.UUID, reason any) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := exec(tx, outcomeSQL, quotation, "awarded"); err != nil {
			return err
		}
		return exec(tx, orderAwardSQL, rfq, order, reason)
	}
}

// TestRFQOrderAwardRules: what the database refuses of an award to an
// order and of a reason, as the application role.
func TestRFQOrderAwardRules(t *testing.T) {
	need(t)
	f := newOrderAwards(t)
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	const notOfQuotation = "is awarded to one of its quotations"
	for name, c := range map[string]struct {
		do   func(tx pgx.Tx) error
		want string
	}{
		"another supplier's order":            {orderAwarding(f.quotation, f.published, f.otherOrder, nil), notOfQuotation},
		"an order in another currency":        {orderAwarding(f.quotation, f.published, f.usdOrder, nil), notOfQuotation},
		"an order naming a requisition":       {orderAwarding(f.quotation, f.published, f.sourcedOrder, nil), notOfQuotation},
		"a requisition's RFQ to a contract":   {awarding(f.sourcedQuote, f.sourced, f.contract), notOfQuotation},
		"a requisition's RFQ to a free order": {orderAwarding(f.sourcedQuote, f.sourced, f.order, nil), notOfQuotation},
		"an order and a contract": {func(tx pgx.Tx) error {
			if err := exec(tx, outcomeSQL, f.quotation, "awarded"); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.rfq SET status = 'awarded', contract_id = $2, purchase_order_id = $3, version = version + 1 WHERE id = $1`,
				f.published, f.contract, f.order)
		}, "rfq_awarded_to_one_document"},
		"an order without an award": {one(`UPDATE procurement.rfq SET purchase_order_id = $2, version = version + 1 WHERE id = $1`, f.published, f.order),
			"rfq_awarded_to_one_document"},
		"a reason without an award": {one(`UPDATE procurement.rfq SET award_reason = 'R', version = version + 1 WHERE id = $1`, f.published),
			"rfq_award_reason_check"},
		"a blank reason":    {orderAwarding(f.quotation, f.published, f.order, " "), "rfq_award_reason_check"},
		"a reason too long": {orderAwarding(f.quotation, f.published, f.order, strings.Repeat("r", 501)), "rfq_award_reason_check"},
		"an unknown order":  {orderAwarding(f.quotation, f.published, id(), nil), notOfQuotation},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, c.do), c.want)
			if c.want == notOfQuotation { // the trigger's rule holds for the owner too
				failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
					if err := exec(tx, `SET LOCAL ROLE erp_owner`); err != nil {
						return err
					}
					return c.do(tx)
				}), c.want)
			}
		})
	}
}

// TestRFQAwardedToOrder: the awards the database accepts, and what it
// keeps of them.
func TestRFQAwardedToOrder(t *testing.T) {
	need(t)
	f := newOrderAwards(t)
	mustApp(t, f.tenant, orderAwarding(f.quotation, f.published, f.order, strings.Repeat("r", 500)))
	mustApp(t, f.tenant, orderAwarding(f.sourcedQuote, f.sourced, f.sourcedOrder, nil))
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	for name, c := range map[string]struct {
		do   func(tx pgx.Tx) error
		want string
	}{
		"the order changed": {one(`UPDATE procurement.rfq SET purchase_order_id = $2, version = version + 1 WHERE id = $1`, f.published, f.usdOrder),
			"stays awarded to its contract or purchase order"},
		"the reason changed": {one(`UPDATE procurement.rfq SET award_reason = 'Other', version = version + 1 WHERE id = $1`, f.published),
			"stays awarded to its contract or purchase order"},
		"an order awarded by a second RFQ": {orderAwarding(f.third, f.other, f.order, nil), "rfq_purchase_order_once"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, c.do), c.want)
		})
	}
	t.Run("as the owner", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `SET LOCAL ROLE erp_owner`); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.rfq SET award_reason = NULL, version = version + 1 WHERE id = $1`, f.published)
		}), "stays awarded to its contract or purchase order")
	})
}
