package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A schedule line called off (T83b, migration 000280): what the database
// keeps of a call-off (ADR-0070 §4). A line is entered not called off and
// is called off once, to a purchase order against its own scheduling
// agreement; nothing else of it changes.

const (
	callOffOrderSQL = `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, contract_id, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-06-01', 'NT30', $6, 'draft', $7, $7)`
	callOffSQL = `UPDATE procurement.schedule_line SET purchase_order_id = $2, version = version + 1 WHERE id = $1`
)

// callOffFixture is newSchedule with a second line of its agreement
// (second), a draft order against the agreement (order), another against
// it (spare), one against the contract of the kind contract (elsewhere)
// and one against no contract (free).
type callOffFixture struct {
	scheduleFixture
	second, order, spare, elsewhere, free uuid.UUID
}

func newCallOffs(t *testing.T) callOffFixture {
	t.Helper()
	f := callOffFixture{scheduleFixture: newSchedule(t), second: id(), order: id(), spare: id(), elsewhere: id(), free: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{scheduleLineSQL, []any{f.tenant, f.second, f.agreement, 2, 1, "2026-07-15", "60", f.actor}},
			{callOffOrderSQL, []any{f.tenant, f.order, "PO1", f.entity, f.supplier, f.agreement, f.actor}},
			{callOffOrderSQL, []any{f.tenant, f.spare, "PO2", f.entity, f.supplier, f.agreement, f.actor}},
			{callOffOrderSQL, []any{f.tenant, f.elsewhere, "PO3", f.entity, f.supplier, f.plain, f.actor}},
			{callOffOrderSQL, []any{f.tenant, f.free, "PO4", f.entity, f.supplier, nil, f.actor}},
			{callOffSQL, []any{f.line, f.order}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

// TestScheduleLineCallOffRules: what the database refuses of a call-off,
// as the application role.
func TestScheduleLineCallOffRules(t *testing.T) {
	need(t)
	f := newCallOffs(t)
	set := func(column, value string) string {
		return `UPDATE procurement.schedule_line SET ` + column + ` = ` + value + `, purchase_order_id = $2, version = version + 1 WHERE id = $1`
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a line entered called off": {`INSERT INTO procurement.schedule_line (tenant_id, id, contract_id, line_no, contract_item_no, delivery_date,
			quantity, purchase_order_id, created_by, updated_by) VALUES ($1, $2, $3, 3, 1, '2026-08-15', 5, $4, $5, $5)`,
			[]any{f.tenant, id(), f.agreement, f.spare, f.actor}, "is entered not called off"},
		"a line called off again":                {callOffSQL, []any{f.line, f.spare}, "was called off: it is fixed"},
		"a called-off line's order taken away":   {callOffSQL, []any{f.line, nil}, "was called off: it is fixed"},
		"an order calling off a second line":     {callOffSQL, []any{f.second, f.order}, "schedule_line_order_once"},
		"an order against another contract":      {callOffSQL, []any{f.second, f.elsewhere}, "against its own scheduling agreement"},
		"an order against no contract":           {callOffSQL, []any{f.second, f.free}, "against its own scheduling agreement"},
		"an order there is not":                  {callOffSQL, []any{f.second, id()}, "against its own scheduling agreement"},
		"a change that calls nothing off":        {`UPDATE procurement.schedule_line SET version = version + 1 WHERE id = $1`, []any{f.second}, "against its own scheduling agreement"},
		"the quantity changed with the call-off": {set("quantity", "1"), []any{f.second, f.spare}, "permission denied"},
		"the date changed with the call-off":     {set("delivery_date", "'2026-09-01'"), []any{f.second, f.spare}, "permission denied"},
		"a called-off line deleted":              {`DELETE FROM procurement.schedule_line WHERE id = $1`, []any{f.line}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("the second line called off as its own order", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, callOffSQL, f.second, f.spare) })
	})
}
