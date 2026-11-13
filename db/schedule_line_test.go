package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Scheduling agreements (T83a, migration 000301): what the database
// keeps of a contract's kind and of a scheduling agreement's delivery
// schedule (ADR-0070 §1-§3).

const (
	kindSQL = `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from,
		valid_to, target_value, status, kind, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-01-01', '2026-12-31', 100000, 'draft', $6, $7, $7)`
	contractItemSQL = `INSERT INTO procurement.contract_item (tenant_id, contract_id, item_no, material_key, description, unit_price, price_unit,
		uom_code, account_id) VALUES ($1, $2, $3, $4, 'Chairs', 41250, 1, 'EA', $5)`
	releaseSQL      = `UPDATE procurement.contract SET status = 'active', version = version + 1 WHERE id = $1`
	scheduleLineSQL = `INSERT INTO procurement.schedule_line (tenant_id, id, contract_id, line_no, contract_item_no, delivery_date, quantity,
		created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`
)

// scheduleFixture is a tenant with an active scheduling agreement of 2026
// (item 1 live, item 2 removed while it was a draft) holding one schedule
// line, a draft scheduling agreement and an active contract of the kind
// contract, each with an item 1.
type scheduleFixture struct {
	fixture
	agreement, draft, plain, line uuid.UUID
}

func newSchedule(t *testing.T) scheduleFixture {
	t.Helper()
	f := scheduleFixture{fixture: newFixture(t), agreement: id(), draft: id(), plain: id(), line: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{kindSQL, []any{f.tenant, f.agreement, "SA1", f.entity, f.supplier, "scheduling_agreement", f.actor}},
			{contractItemSQL, []any{f.tenant, f.agreement, 1, "CHAIR", f.expense}},
			{contractItemSQL, []any{f.tenant, f.agreement, 2, "DESK", f.expense}},
			{`UPDATE procurement.contract_item SET is_removed = true WHERE contract_id = $1 AND item_no = 2`, []any{f.agreement}},
			{releaseSQL, []any{f.agreement}},
			{kindSQL, []any{f.tenant, f.draft, "SA2", f.entity, f.supplier, "scheduling_agreement", f.actor}},
			{contractItemSQL, []any{f.tenant, f.draft, 1, "CHAIR", f.expense}},
			{kindSQL, []any{f.tenant, f.plain, "CT1", f.entity, f.supplier, "contract", f.actor}},
			{contractItemSQL, []any{f.tenant, f.plain, 1, "CHAIR", f.expense}},
			{releaseSQL, []any{f.plain}},
			{scheduleLineSQL, []any{f.tenant, f.line, f.agreement, 1, 1, "2026-06-15", "40", f.actor}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f scheduleFixture) try(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
}

// TestScheduleLineRules: what the database refuses of a contract's kind
// and of a schedule line, as the application role.
func TestScheduleLineRules(t *testing.T) {
	need(t)
	f := newSchedule(t)
	line := func(contract uuid.UUID, no, item int, date, quantity string) []any {
		return []any{f.tenant, id(), contract, no, item, date, quantity, f.actor}
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"an unknown kind": {kindSQL, []any{f.tenant, id(), "CT9", f.entity, f.supplier, "blanket", f.actor}, "contract_kind_check"},
		"a kind changed": {`UPDATE procurement.contract SET kind = 'scheduling_agreement', version = version + 1 WHERE id = $1`,
			[]any{f.plain}, "keeps its kind"},
		"a line of a contract":          {scheduleLineSQL, line(f.plain, 1, 1, "2026-06-15", "40"), "is not a scheduling agreement"},
		"a line of a draft agreement":   {scheduleLineSQL, line(f.draft, 1, 1, "2026-06-15", "40"), "only an active one takes schedule lines"},
		"a line before the validity":    {scheduleLineSQL, line(f.agreement, 2, 1, "2025-12-31", "40"), "a schedule line is due within it"},
		"a line after the validity":     {scheduleLineSQL, line(f.agreement, 2, 1, "2027-01-01", "40"), "a schedule line is due within it"},
		"a line of a removed item":      {scheduleLineSQL, line(f.agreement, 2, 2, "2026-06-15", "40"), "was removed"},
		"a line of an item it lacks":    {scheduleLineSQL, line(f.agreement, 2, 3, "2026-06-15", "40"), "schedule_line_item_fkey"},
		"a line of no contract":         {scheduleLineSQL, line(id(), 1, 1, "2026-06-15", "40"), "schedule_line_item_fkey"},
		"a line numbered past the next": {scheduleLineSQL, line(f.agreement, 3, 1, "2026-06-15", "40"), "the next is 2"},
		"a line numbered as the last":   {scheduleLineSQL, line(f.agreement, 1, 1, "2026-06-15", "40"), "the next is 2"},
		"a quantity of zero":            {scheduleLineSQL, line(f.agreement, 2, 1, "2026-06-15", "0"), "schedule_line_quantity_check"},
		"a line changed":                {`UPDATE procurement.schedule_line SET quantity = 1, version = version + 1 WHERE id = $1`, []any{f.line}, "permission denied"},
		"a line deleted":                {`DELETE FROM procurement.schedule_line WHERE id = $1`, []any{f.line}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("lines numbered in the order entered, on the first and the last day", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, scheduleLineSQL, line(f.agreement, 2, 1, "2026-12-31", "0.000001")...); err != nil {
				return err
			}
			return exec(tx, scheduleLineSQL, line(f.agreement, 3, 1, "2026-01-01", "12.5")...)
		})
	})
	// The trigger numbers lines one after another only for writers that
	// queue on the contract; two that read it FOR SHARE at once both find
	// the same last number, and the unique key refuses the second. With
	// the triggers off, as the owner, the key is what is left.
	t.Run("a number used twice", func(t *testing.T) {
		tx, err := testDB.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		if err := exec(tx, `SET LOCAL session_replication_role = replica`); err != nil {
			t.Fatal(err)
		}
		failsWith(t, exec(tx, scheduleLineSQL, line(f.agreement, 1, 1, "2026-06-15", "40")...), "schedule_line_numbered_once")
	})
	t.Run("a contract written without a kind is a contract", func(t *testing.T) {
		var kind string
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			c, err := f.contract(tx, "draft", "2026-01-01", "2026-12-31", nil)
			if err != nil {
				return err
			}
			return tx.QueryRow(t.Context(), `SELECT kind FROM procurement.contract WHERE id = $1`, c).Scan(&kind)
		})
		if kind != "contract" {
			t.Errorf("kind of a contract written without one: got %q, want contract", kind)
		}
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM procurement.schedule_line`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("rows of procurement.schedule_line another tenant sees: got %d, want 0", n)
		}
	})
}
