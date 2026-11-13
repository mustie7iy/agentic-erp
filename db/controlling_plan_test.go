package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Plan values (T56g, migration 000164): what the database keeps of plan
// categories, batches and lines (ADR-0050 §8).

// planFixture is a tenant with an open category PLAN, a locked one LOCKED,
// a batch of PLAN in 2027 with one line on a cost center and one on a
// profit center, and a second batch of PLAN in 2027 that replaces nothing.
type planFixture struct {
	fixture
	open, locked, batch, later, line, pcLine, cc, pc, account uuid.UUID
}

const (
	planBatchSQL = `INSERT INTO controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year, batch_key, digest, line_count,
		replaced_count, created_by) VALUES ($1, $2, $3, $4, $5, $6, repeat('a', 64), 1, 0, $7)`
	planLineSQL = `INSERT INTO controlling.plan_line (tenant_id, id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period, cost_center_id,
		profit_center_id, account_id, amount, quantity, uom_code, created_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, 2027, $5, $6, $7, $8, 100, $9, $10, $11)`
)

func newPlan(t *testing.T) planFixture {
	t.Helper()
	f := planFixture{fixture: newFixture(t), open: id(), locked: id(), batch: id(), later: id(), line: id(), pcLine: id(), cc: id(), pc: id(), account: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO controlling.plan_category (tenant_id, id, code, name, created_by, updated_by) VALUES ($1, $2, 'PLAN', 'Budget', $4, $4),
				($1, $3, 'LOCKED', 'Locked', $4, $4)`, []any{f.tenant, f.open, f.locked, f.actor}},
			{planBatchSQL, []any{f.tenant, f.batch, f.open, f.entity, 2027, "b1", f.actor}},
			{planBatchSQL, []any{f.tenant, f.later, f.open, f.entity, 2027, "b2", f.actor}},
			{`INSERT INTO controlling.plan_line (tenant_id, id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period, cost_center_id, account_id,
				amount, created_by) VALUES ($1, $2, $3, $4, $5, 2027, 1, $6, $7, 100, $8)`, []any{f.tenant, f.line, f.batch, f.open, f.entity, f.cc, f.account, f.actor}},
			{`INSERT INTO controlling.plan_line (tenant_id, id, batch_id, category_id, legal_entity_id, fiscal_year, fiscal_period, profit_center_id, account_id,
				amount, created_by) VALUES ($1, $2, $3, $4, $5, 2027, 1, $6, $7, 100, $8)`, []any{f.tenant, f.pcLine, f.batch, f.open, f.entity, f.pc, f.account, f.actor}},
			{`UPDATE controlling.plan_category SET locked = true, version = version + 1 WHERE id = $1`, []any{f.locked}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f planFixture) try(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
}

func TestPlanRules(t *testing.T) {
	need(t)
	f := newPlan(t)
	category := `INSERT INTO controlling.plan_category (tenant_id, id, code, name, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, $4)`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a category code SAP's form cannot hold": {category, []any{f.tenant, "plan", "P", f.actor}, "plan_category_code_check"},
		"a category without a name":              {category, []any{f.tenant, "FC", " ", f.actor}, "plan_category_name_check"},
		"a category code twice":                  {category, []any{f.tenant, "PLAN", "Again", f.actor}, "plan_category_code_once"},
		"a category deleted":                     {`DELETE FROM controlling.plan_category WHERE id = $1`, []any{f.open}, "permission denied"},
		"a batch key with surrounding space":     {planBatchSQL, []any{f.tenant, id(), f.open, f.entity, 2027, " b3", f.actor}, "plan_batch_batch_key_check"},
		"a batch key twice":                      {planBatchSQL, []any{f.tenant, id(), f.open, f.entity, 2027, "b1", f.actor}, "plan_batch_key_once"},
		"a batch of a year out of range":         {planBatchSQL, []any{f.tenant, id(), f.open, f.entity, 1800, "b3", f.actor}, "plan_batch_fiscal_year_check"},
		"a batch of an unknown category":         {planBatchSQL, []any{f.tenant, id(), id(), f.entity, 2027, "b3", f.actor}, "plan_batch_tenant_id_category_id_fkey"},
		"a batch of a locked category":           {planBatchSQL, []any{f.tenant, id(), f.locked, f.entity, 2027, "b3", f.actor}, "plan category"},
		"a batch without lines": {`INSERT INTO controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year, batch_key, digest, line_count,
			replaced_count, created_by) VALUES ($1, gen_random_uuid(), $2, $3, 2027, 'b3', repeat('a', 64), 0, 0, $4)`,
			[]any{f.tenant, f.open, f.entity, f.actor}, "plan_batch_line_count_check"},
		"a batch with a digest that is not one": {`INSERT INTO controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year, batch_key, digest,
			line_count, replaced_count, created_by) VALUES ($1, gen_random_uuid(), $2, $3, 2027, 'b3', 'x', 1, 0, $4)`,
			[]any{f.tenant, f.open, f.entity, f.actor}, "plan_batch_digest_check"},
		"a batch changed": {`UPDATE controlling.plan_batch SET line_count = 2 WHERE id = $1`, []any{f.batch}, "permission denied"},
		"a line without a center": {planLineSQL, []any{f.tenant, f.later, f.open, f.entity, 2, nil, nil, f.account, nil, nil, f.actor},
			"plan_line_one_center"},
		"a line on both centers": {planLineSQL, []any{f.tenant, f.later, f.open, f.entity, 2, f.cc, f.pc, f.account, nil, nil, f.actor},
			"plan_line_one_center"},
		"a quantity without a unit": {planLineSQL, []any{f.tenant, f.later, f.open, f.entity, 2, f.cc, nil, f.account, "1", nil, f.actor},
			"plan_line_quantity_unit"},
		"a quantity that is not positive": {planLineSQL, []any{f.tenant, f.later, f.open, f.entity, 2, f.cc, nil, f.account, "0", "H", f.actor},
			"plan_line_quantity_check"},
		"an unknown unit": {planLineSQL, []any{f.tenant, f.later, f.open, f.entity, 2, f.cc, nil, f.account, "1", "XX", f.actor},
			"plan_line_uom_code_fkey"},
		"a period out of range": {planLineSQL, []any{f.tenant, f.later, f.open, f.entity, 17, f.cc, nil, f.account, nil, nil, f.actor},
			"plan_line_fiscal_period_check"},
		"a line of another company than its batch's": {planLineSQL, []any{f.tenant, f.later, f.open, id(), 2, f.cc, nil, f.account, nil, nil, f.actor},
			"plan_line_tenant_id_batch_id_category_id_legal_entity_id_f_fkey"},
		"a second current line of a center, account and period": {planLineSQL,
			[]any{f.tenant, f.later, f.open, f.entity, 1, f.cc, nil, f.account, nil, nil, f.actor}, "plan_line_once"},
		"a second current line of a profit center, account and period": {planLineSQL,
			[]any{f.tenant, f.later, f.open, f.entity, 1, nil, f.pc, f.account, nil, nil, f.actor}, "plan_line_once"},
		"a line deleted": {`DELETE FROM controlling.plan_line WHERE id = $1`, []any{f.line}, "permission denied"},
		"a line's amount changed": {`UPDATE controlling.plan_line SET amount = 1 WHERE id = $1`, []any{f.line},
			"permission denied"},
		"a line replaced by its own batch": {`UPDATE controlling.plan_line SET replaced_by_batch_id = batch_id WHERE id = $1`, []any{f.line},
			"plan_line_replaced_later"},
		"a line replaced by an unknown batch": {`UPDATE controlling.plan_line SET replaced_by_batch_id = $2 WHERE id = $1`, []any{f.line, id()},
			"plan_line_replaced_by"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("a replaced line makes way for its center's new line", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `UPDATE controlling.plan_line SET replaced_by_batch_id = $2 WHERE id = $1`, f.line, f.later); err != nil {
				return err
			}
			return exec(tx, planLineSQL, f.tenant, f.later, f.open, f.entity, 1, f.cc, nil, f.account, "1.5", "H", f.actor)
		})
	})
}

// TestPlanLineReplacedOnce: a line is replaced once, and its other columns
// stay as written, whoever writes them.
func TestPlanLineReplacedOnce(t *testing.T) {
	need(t)
	f := newPlan(t)
	third := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, planBatchSQL, f.tenant, third, f.open, f.entity, 2027, "b3", f.actor); err != nil {
			return err
		}
		return exec(tx, `UPDATE controlling.plan_line SET replaced_by_batch_id = $2 WHERE id = $1`, f.line, f.later)
	})
	failsWith(t, f.try(t, `UPDATE controlling.plan_line SET replaced_by_batch_id = $2 WHERE id = $1`, f.line, third), "is replaced once")
	failsWith(t, f.try(t, `UPDATE controlling.plan_line SET replaced_by_batch_id = NULL WHERE id = $1`, f.line), "is replaced once")
	tx, err := testDB.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	failsWith(t, exec(tx, `UPDATE controlling.plan_line SET amount = 1, replaced_by_batch_id = $2 WHERE id = $1`, f.pcLine, f.later), "is replaced once")
}

// TestLockedPlanDoesNotMove: a locked category's lines are neither written
// nor replaced.
func TestLockedPlanDoesNotMove(t *testing.T) {
	need(t)
	f := newPlan(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE controlling.plan_category SET locked = true, version = version + 1 WHERE id = $1`, f.open)
	})
	failsWith(t, f.try(t, planLineSQL, f.tenant, f.later, f.open, f.entity, 2, f.cc, nil, f.account, nil, nil, f.actor), "is locked")
	failsWith(t, f.try(t, `UPDATE controlling.plan_line SET replaced_by_batch_id = $2 WHERE id = $1`, f.line, f.later), "is locked")
}

// TestPlanBatchCopyRules (T93a, migration 000309; ADR-0050 §8 As built
// T93a): a batch written by a copy names the category copied from and the
// percentage, both or neither; the percentage is above 0 and at most
// 1000; the category copied from exists and is another than the batch's.
func TestPlanBatchCopyRules(t *testing.T) {
	need(t)
	f := newPlan(t)
	const copySQL = `INSERT INTO controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year, batch_key, digest, line_count,
		replaced_count, created_by, copied_from_category_id, copy_percentage) VALUES ($1, gen_random_uuid(), $2, $3, 2027, $4, repeat('a', 64), 1, 0, $5, $6, $7)`
	for name, c := range map[string]struct {
		from       any
		percentage any
		want       string
	}{
		"a category copied from without a percentage": {f.locked, nil, "plan_batch_copy_origin"},
		"a percentage without a category copied from": {nil, "105", "plan_batch_copy_origin"},
		"a percentage of zero":                        {f.locked, "0", "plan_batch_copy_percentage"},
		"a percentage above 1000":                     {f.locked, "1000.0001", "plan_batch_copy_percentage"},
		"a category copied into itself":               {f.open, "105", "plan_batch_copy_other_category"},
		"an unknown category copied from":             {id(), "105", "plan_batch_copied_from"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, copySQL, f.tenant, f.open, f.entity, "c1", f.actor, c.from, c.percentage), c.want)
		})
	}
	t.Run("a copy of a locked category at the largest percentage, and an uploaded batch", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, copySQL, f.tenant, f.open, f.entity, "c1", f.actor, f.locked, "1000"); err != nil {
				return err
			}
			return exec(tx, copySQL, f.tenant, f.open, f.entity, "c2", f.actor, nil, nil)
		})
		var kept string
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT copy_percentage::text FROM controlling.plan_batch WHERE batch_key = 'c1'`).Scan(&kept)
		})
		if kept != "1000.0000" {
			t.Errorf("the percentage kept: got %s, want 1000.0000", kept)
		}
	})
}
