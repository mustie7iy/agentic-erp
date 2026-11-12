package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Internal orders (T60a, migration 000267): what the database keeps of the
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
		"an unknown status": {`UPDATE controlling.internal_order SET status = 'archived', version = version + 1 WHERE id = $1`, []any{f.created},
			"internal_order_status_check"},
		"an order closed that was never released": {`UPDATE controlling.internal_order SET status = 'closed', version = version + 1 WHERE id = $1`,
			[]any{f.created}, "is closed from released"},
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

// An order's budget (T60c, migration 000274; ADR-0064 §3): one amount per
// order, zero or more, changed version by version and never deleted.

const orderBudgetSQL = `INSERT INTO controlling.order_budget (tenant_id, internal_order_id, amount, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`

func TestOrderBudgetRules(t *testing.T) {
	need(t)
	f := newOrders(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, orderBudgetSQL, f.tenant, f.released, 100_000, f.actor) })
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a negative budget":           {orderBudgetSQL, []any{f.tenant, f.created, -1, f.actor}, "order_budget_not_negative"},
		"a budget of no order":        {orderBudgetSQL, []any{f.tenant, id(), 100, f.actor}, "order_budget_order"},
		"a second budget of an order": {orderBudgetSQL, []any{f.tenant, f.released, 100, f.actor}, "order_budget_pkey"},
		"a budget changed to a negative": {`UPDATE controlling.order_budget SET amount = -1, version = version + 1 WHERE internal_order_id = $1`, []any{f.released},
			"order_budget_not_negative"},
		"a budget changed without its version": {`UPDATE controlling.order_budget SET amount = 5 WHERE internal_order_id = $1`, []any{f.released}, "optimistic lock"},
		"a budget deleted":                     {`DELETE FROM controlling.order_budget WHERE internal_order_id = $1`, []any{f.released}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("a budget of zero, and one replaced at its version", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, orderBudgetSQL, f.tenant, f.created, 0, f.actor); err != nil {
				return err
			}
			return exec(tx, `UPDATE controlling.order_budget SET amount = 5, version = version + 1 WHERE internal_order_id = $1`, f.released)
		})
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM controlling.order_budget`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("budgets another tenant sees: got %d, want 0", n)
		}
	})
}

// An order's settlement rule (T60d, migration 000275; ADR-0064 §3): one
// receiver per order, replaced in place and never deleted.

const settlementRuleSQL = `INSERT INTO controlling.settlement_rule (tenant_id, internal_order_id, receiver_cost_center_id, created_by, updated_by)
	VALUES ($1, $2, $3, $4, $4)`

func TestSettlementRuleRules(t *testing.T) {
	need(t)
	f := newOrders(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, settlementRuleSQL, f.tenant, f.released, f.cc, f.actor) })
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a rule of no order":        {settlementRuleSQL, []any{f.tenant, id(), f.cc, f.actor}, "settlement_rule_order"},
		"a second rule of an order": {settlementRuleSQL, []any{f.tenant, f.released, id(), f.actor}, "settlement_rule_pkey"},
		"a rule without a receiver": {settlementRuleSQL, []any{f.tenant, f.created, nil, f.actor}, "receiver_cost_center_id"},
		"a rule's receiver taken away": {`UPDATE controlling.settlement_rule SET receiver_cost_center_id = NULL, version = version + 1 WHERE internal_order_id = $1`,
			[]any{f.released}, "receiver_cost_center_id"},
		"a rule replaced without its version": {`UPDATE controlling.settlement_rule SET receiver_cost_center_id = $2 WHERE internal_order_id = $1`,
			[]any{f.released, id()}, "optimistic lock"},
		"a rule deleted": {`DELETE FROM controlling.settlement_rule WHERE internal_order_id = $1`, []any{f.released}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("a receiver replaced", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE controlling.settlement_rule SET receiver_cost_center_id = $2, version = version + 1 WHERE internal_order_id = $1`, f.released, id())
		})
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM controlling.settlement_rule`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("rules another tenant sees: got %d, want 0", n)
		}
	})
}

// An order settled and closed (T60e, migration 000273; ADR-0064 §3): a
// settlement is kept once and never changed, and a closed order stays
// closed.

const orderSettlementSQL = `INSERT INTO controlling.order_settlement (tenant_id, id, internal_order_id, legal_entity_id, fiscal_year, fiscal_period,
	posting_date, receiver_cost_center_id, journal_entry_id, document_number, amount, account_count, created_by)
	VALUES ($1, $2, $3, $4, 2026, $5, '2026-05-31', $6, $7, $8, $9, $10, $11)`

func TestOrderSettlementRules(t *testing.T) {
	need(t)
	f := newOrders(t)
	kept, entry := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, orderSettlementSQL, f.tenant, kept, f.released, f.entity, 5, f.cc, entry, "100000001", -500, 1, f.actor)
	})
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a settlement of no order": {orderSettlementSQL, []any{f.tenant, id(), id(), f.entity, 5, f.cc, id(), "1", 100, 1, f.actor}, "order_settlement_order"},
		"an entry settled twice":   {orderSettlementSQL, []any{f.tenant, id(), f.released, f.entity, 6, f.cc, entry, "1", 100, 1, f.actor}, "order_settlement_entry_once"},
		"a period that is none":    {orderSettlementSQL, []any{f.tenant, id(), f.released, f.entity, 17, f.cc, id(), "1", 100, 1, f.actor}, "order_settlement_fiscal_period_check"},
		"a settlement of no account": {orderSettlementSQL, []any{f.tenant, id(), f.released, f.entity, 5, f.cc, id(), "1", 100, 0, f.actor},
			"order_settlement_account_count_check"},
		"a settlement without its entry's number": {orderSettlementSQL, []any{f.tenant, id(), f.released, f.entity, 5, f.cc, id(), "", 100, 1, f.actor},
			"order_settlement_document_number_check"},
		"a settlement without a receiver": {orderSettlementSQL, []any{f.tenant, id(), f.released, f.entity, 5, nil, id(), "1", 100, 1, f.actor},
			"receiver_cost_center_id"},
		"a settlement changed": {`UPDATE controlling.order_settlement SET amount = 1 WHERE id = $1`, []any{kept}, "permission denied"},
		"a settlement deleted": {`DELETE FROM controlling.order_settlement WHERE id = $1`, []any{kept}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM controlling.order_settlement`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("settlements another tenant sees: got %d, want 0", n)
		}
	})
	t.Run("a released order is closed, and stays closed", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE controlling.internal_order SET status = 'closed', version = version + 1 WHERE id = $1`, f.released)
		})
		for name, status := range map[string]string{"released again": "released", "created again": "created"} {
			t.Run(name, func(t *testing.T) {
				failsWith(t, f.try(t, `UPDATE controlling.internal_order SET status = $2, version = version + 1 WHERE id = $1`, f.released, status), "stays closed")
			})
		}
	})
}

// The controlling context posts journal entries (T60e, migrations 000273
// and 000274; ADR-0064 §3): the journal takes its source context, and
// the widened check is validated on every partition.
func TestJournalSourceControlling(t *testing.T) {
	need(t)
	f := newOrders(t)
	t.Run("an entry of the controlling context", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return f.entryFrom(tx, "controlling", nil, &f.released) })
	})
	t.Run("an entry of no context", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return f.entryFrom(tx, "elsewhere", nil, &f.released) }),
			"journal_entry_source_context_check")
	})
	t.Run("the check is validated on the table and every partition", func(t *testing.T) {
		var tables, checked int
		if err := testDB.QueryRow(t.Context(), `SELECT (SELECT count(*) + 1 FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass),
				(SELECT count(*) FROM pg_constraint WHERE conname = 'journal_entry_source_context_check' AND convalidated)`).Scan(&tables, &checked); err != nil {
			t.Fatal(err)
		}
		if checked != tables {
			t.Errorf("validated checks: got %d, want %d (the table and its partitions)", checked, tables)
		}
	})
}

// The internal order on journal lines (T60b, migrations 000268-000270;
// ADR-0064 §2): a line carries a cost center or an order, never both; the
// check holds on every partition, and the order's lines are indexed.

// orderLine inserts an entry of the fixture's company in March 2026 whose
// expense line carries the cost center and the internal order given.
func (f orderFixture) orderLine(tx pgx.Tx, costCenter, order *uuid.UUID) error {
	return f.entryFrom(tx, "ledger", costCenter, order)
}

// entryFrom is orderLine's entry posted by the source context.
func (f orderFixture) entryFrom(tx pgx.Tx, source string, costCenter, order *uuid.UUID) error {
	je := id()
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		VALUES ($1,2026,$2,$3,$4,3,'SA',$5,'2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,$7,$6)`,
		f.tenant, je, f.entity, f.ledger, je.String()[24:], f.actor, source); err != nil {
		return err
	}
	const line = `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
		posting_date, account_id, amount_tc, amount_lc, amount_gc, cost_center_id, internal_order_id) VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,$9,$10)`
	if err := exec(tx, line, f.tenant, je, 1, f.entity, f.ledger, f.expense, 100, 90, costCenter, order); err != nil {
		return err
	}
	return exec(tx, line, f.tenant, je, 2, f.entity, f.ledger, f.bank, -100, -90, nil, nil)
}

func TestJournalLineInternalOrder(t *testing.T) {
	need(t)
	f := newOrders(t)
	center := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, $2, $3, 'CC1', 'Center', 'J', 'pending', $4, $4)`, f.tenant, center, f.entity, f.actor)
	})
	t.Run("an order without a cost center", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return f.orderLine(tx, nil, &f.released) })
	})
	t.Run("a cost center without an order", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return f.orderLine(tx, &center, nil) })
	})
	t.Run("never both", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return f.orderLine(tx, &center, &f.released) }), "journal_line_one_cost_object")
	})
	t.Run("the check is validated on the table and every partition", func(t *testing.T) {
		var tables, checked int
		if err := testDB.QueryRow(t.Context(), `SELECT (SELECT count(*) + 1 FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass),
				(SELECT count(*) FROM pg_constraint WHERE conname = 'journal_line_one_cost_object' AND convalidated)`).Scan(&tables, &checked); err != nil {
			t.Fatal(err)
		}
		if checked != tables {
			t.Errorf("validated checks: got %d, want %d (the table and its partitions)", checked, tables)
		}
	})
	t.Run("the lines are indexed by order", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(t.Context(), `SELECT i.indisvalid FROM pg_index i
				WHERE i.indexrelid = 'ledger.journal_line_by_internal_order'::regclass`).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Error("journal_line_by_internal_order: got invalid, want valid")
		}
	})
}
