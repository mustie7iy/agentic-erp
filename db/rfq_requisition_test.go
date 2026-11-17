package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RFQs from requisitions (T82e, migration 000359): an RFQ names a
// requisition in sourcing, of its company and currency, the one open RFQ
// of it; each item of such an RFQ names a live item of that requisition,
// and an item of another RFQ names none.

const (
	requisitionSQL = `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, status,
		requisition_date, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, '2026-06-15', $5, $5)`
	requisitionItemSQL = `INSERT INTO procurement.requisition_item (tenant_id, requisition_id, item_no, description, quantity, uom_code, unit_price,
		account_id, is_removed) VALUES ($1, $2, $3, 'Paper', 1, 'EA', 100, $4, $5)`
	sourcingSQL = `INSERT INTO procurement.rfq (tenant_id, id, document_number, legal_entity_id, currency, due_date, status, requisition_id,
		created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '2026-07-31', 'draft', $6, $7, $7)`
	sourcedItemSQL = `INSERT INTO procurement.rfq_item (tenant_id, rfq_id, item_no, material_key, description, quantity, uom_code, account_id,
		requisition_item_no) VALUES ($1, $2, $3, $4, 'Paper', 1, 'EA', $5, $6)`
)

// sourcingFixture is newRFQs with requisitions of its legal entity: one in
// sourcing in EUR (item 1 live, item 2 removed) with a draft RFQ from it
// naming item 1, and one approved.
type sourcingFixture struct {
	rfqFixture
	inSourcing, approved, sourcing uuid.UUID
}

func newSourcing(t *testing.T) sourcingFixture {
	t.Helper()
	f := sourcingFixture{rfqFixture: newRFQs(t), inSourcing: id(), approved: id(), sourcing: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{requisitionSQL, []any{f.tenant, f.inSourcing, "PR1", f.entity, f.actor, "EUR", "in_sourcing"}},
			{requisitionItemSQL, []any{f.tenant, f.inSourcing, 1, f.expense, false}},
			{requisitionItemSQL, []any{f.tenant, f.inSourcing, 2, f.expense, true}},
			{requisitionSQL, []any{f.tenant, f.approved, "PR2", f.entity, f.actor, "EUR", "approved"}},
			{requisitionItemSQL, []any{f.tenant, f.approved, 1, f.expense, false}},
			{sourcingSQL, []any{f.tenant, f.sourcing, "RQ5", f.entity, "EUR", f.inSourcing, f.actor}},
			{sourcedItemSQL, []any{f.tenant, f.sourcing, 1, "PR1/1", f.expense, 1}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

// TestRFQFromRequisitionRules: what the database refuses of an RFQ from a
// requisition, as the application role and, for the triggers, as the
// owner too.
func TestRFQFromRequisitionRules(t *testing.T) {
	need(t)
	f := newSourcing(t)
	const (
		inSourcing = "is created from a requisition in sourcing"
		itemOf     = "names an item of the RFQ's requisition exactly when the RFQ has one"
	)
	for name, c := range map[string]struct {
		sql   string
		args  []any
		want  string
		owner bool
	}{
		"an approved requisition sourced": {sourcingSQL, []any{f.tenant, id(), "RQ9", f.entity, "EUR", f.approved, f.actor}, inSourcing, true},
		"in another currency":             {sourcingSQL, []any{f.tenant, id(), "RQ9", f.entity, "USD", f.inSourcing, f.actor}, inSourcing, true},
		"for another company":             {sourcingSQL, []any{f.tenant, id(), "RQ9", id(), "EUR", f.inSourcing, f.actor}, inSourcing, true},
		"an unknown requisition":          {sourcingSQL, []any{f.tenant, id(), "RQ9", f.entity, "EUR", id(), f.actor}, inSourcing, true},
		"a second open RFQ of it": {sourcingSQL, []any{f.tenant, id(), "RQ9", f.entity, "EUR", f.inSourcing, f.actor},
			"is sourced by one open RFQ at a time", true},
		"its requisition taken away": {`UPDATE procurement.rfq SET requisition_id = NULL, version = version + 1 WHERE id = $1`, []any{f.sourcing},
			"keeps its company, number, currency and due date, and its requisition", true},
		"a requisition given later": {`UPDATE procurement.rfq SET requisition_id = $2, version = version + 1 WHERE id = $1`, []any{f.draft, f.approved},
			"keeps its company, number, currency and due date, and its requisition", true},
		"an item naming no requisition item": {sourcedItemSQL, []any{f.tenant, f.sourcing, 2, "PR1/2", f.expense, nil}, itemOf, true},
		"a removed requisition item":         {sourcedItemSQL, []any{f.tenant, f.sourcing, 2, "PR1/2", f.expense, 2}, itemOf, true},
		"an unknown requisition item":        {sourcedItemSQL, []any{f.tenant, f.sourcing, 2, "PR1/9", f.expense, 9}, itemOf, true},
		"a requisition item of an own RFQ":   {sourcedItemSQL, []any{f.tenant, f.draft, 2, "PR1/1", f.expense, 1}, itemOf, true},
		"an unknown requisition status": {`UPDATE procurement.requisition SET status = 'sourcing', version = version + 1 WHERE id = $1`, []any{f.approved},
			"requisition_status_check", false},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
			if c.owner {
				failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
					if err := exec(tx, `SET LOCAL ROLE erp_owner`); err != nil {
						return err
					}
					return exec(tx, c.sql, c.args...)
				}), c.want)
			}
		})
	}
	t.Run("an RFQ's own dated item and a sourced one", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO procurement.rfq_item (tenant_id, rfq_id, item_no, material_key, description, quantity, uom_code, account_id,
				delivery_date) VALUES ($1, $2, 2, 'DESK', 'Desks', 1, 'EA', $3, '2026-08-01')`, f.tenant, f.draft, f.expense); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.requisition SET status = 'in_sourcing', version = version + 1 WHERE id = $1`, f.approved)
		})
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			r := id()
			if err := exec(tx, sourcingSQL, f.tenant, r, "RQ8", f.entity, "EUR", f.approved, f.actor); err != nil {
				return err
			}
			return exec(tx, sourcedItemSQL, f.tenant, r, 1, "PR2/1", f.expense, 1)
		})
	})
}
