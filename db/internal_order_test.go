package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Internal orders (T60a, migration 000265): what the database keeps of the
// order master (ADR-0064 §1).

const internalOrderSQL = `INSERT INTO controlling.internal_order (tenant_id, id, legal_entity_id, code, name, responsible_cost_center_id, status,
	created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`

// orderFixture is a tenant with a created order FAIR and a released one
// LIVE of its legal entity.
type orderFixture struct {
	fixture
	created, released, cc uuid.UUID
}

func newOrders(t *testing.T) orderFixture {
	t.Helper()
	f := orderFixture{fixture: newFixture(t), created: id(), released: id(), cc: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{internalOrderSQL, []any{f.tenant, f.created, f.entity, "FAIR", "Trade fair", f.cc, "created", f.actor}},
			{internalOrderSQL, []any{f.tenant, f.released, f.entity, "LIVE", "Released", f.cc, "created", f.actor}},
			{`UPDATE controlling.internal_order SET status = 'released', version = version + 1 WHERE id = $1`, []any{f.released}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func TestInternalOrderRules(t *testing.T) {
	need(t)
	f := newOrders(t)
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a code the form cannot hold":   {internalOrderSQL, []any{f.tenant, id(), f.entity, "fair", "F", f.cc, "created", f.actor}, "internal_order_code_check"},
		"a code of thirteen characters": {internalOrderSQL, []any{f.tenant, id(), f.entity, "ABCDEFGHIJKLM", "F", f.cc, "created", f.actor}, "internal_order_code_check"},
		"an order without a name":       {internalOrderSQL, []any{f.tenant, id(), f.entity, "NEW", " ", f.cc, "created", f.actor}, "internal_order_name_check"},
		"a code twice in a company":     {internalOrderSQL, []any{f.tenant, id(), f.entity, "FAIR", "Again", f.cc, "created", f.actor}, "internal_order_code_once"},
		"an unknown status": {`UPDATE controlling.internal_order SET status = 'closed', version = version + 1 WHERE id = $1`, []any{f.created},
			"internal_order_status_check"},
		"an order born released":         {internalOrderSQL, []any{f.tenant, id(), f.entity, "NEW", "N", f.cc, "released", f.actor}, "starts as created"},
		"an order without a cost center": {internalOrderSQL, []any{f.tenant, id(), f.entity, "NEW", "N", nil, "created", f.actor}, "responsible_cost_center_id"},
		"an order moved to another company": {`UPDATE controlling.internal_order SET legal_entity_id = $2, version = version + 1 WHERE id = $1`,
			[]any{f.created, id()}, "keeps its company and code"},
		"an order's code changed": {`UPDATE controlling.internal_order SET code = 'OTHER', version = version + 1 WHERE id = $1`, []any{f.created},
			"keeps its company and code"},
		"a release taken back": {`UPDATE controlling.internal_order SET status = 'created', version = version + 1 WHERE id = $1`, []any{f.released},
			"stays released"},
		"an order deleted": {`DELETE FROM controlling.internal_order WHERE id = $1`, []any{f.created}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("the same code in another company", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, internalOrderSQL, f.tenant, id(), id(), "FAIR", "Another company's", f.cc, "created", f.actor)
		})
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM controlling.internal_order`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("orders another tenant sees: got %d, want 0", n)
		}
	})
}

func (f orderFixture) try(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
}
