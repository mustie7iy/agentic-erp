package db

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Balances by account assignment (T56f, migration 000163): marking an
// entry projected adds its assigned lines to ledger.assignment_balance in
// the same statement, once.

// enqueue queues an entry for the projector, as posting does.
func (f centerFixture) enqueue(t *testing.T, je uuid.UUID) {
	t.Helper()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.balance_projection (tenant_id, event_id, fiscal_year, journal_entry_id) VALUES ($1, $2, 2026, $3)`,
			f.tenant, id(), je)
	})
}

// projectAll marks every queued entry of the tenant projected, as the
// projector's round does; a set projected_at is set again, which a
// projector never does, to prove it counts nothing twice.
func (f centerFixture) projectAll(t *testing.T) {
	t.Helper()
	if err := inRole(t, "erp_projector", func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.balance_projection SET projected_at = now() WHERE tenant_id = $1`, f.tenant)
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
}

// assignmentBalances lists the tenant's balances as
// "cc/pc/seg debit_lc/credit_lc debit_gc/credit_gc", sorted; "-" for no
// center.
func (f centerFixture) assignmentBalances(t *testing.T) []string {
	t.Helper()
	rows, _ := testDB.Query(context.Background(), `SELECT coalesce(cost_center_id::text, '-'), coalesce(profit_center_id::text, '-'),
			coalesce(segment_id::text, '-'), debit_lc, credit_lc, debit_gc, credit_gc
		FROM ledger.assignment_balance WHERE tenant_id = $1`, f.tenant)
	var out []string
	var cc, pc, seg string
	var dl, cl, dg, cg int64
	if _, err := pgx.ForEachRow(rows, []any{&cc, &pc, &seg, &dl, &cl, &dg, &cg}, func() error {
		out = append(out, fmt.Sprintf("%s/%s/%s %d/%d %d/%d", cc, pc, seg, dl, cl, dg, cg))
		return nil
	}); err != nil {
		t.Fatalf("read assignment balances: %v", err)
	}
	slices.Sort(out)
	return out
}

func TestAssignmentBalance(t *testing.T) {
	need(t)
	f := newCenters(t)
	segment := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.segment (tenant_id, id, code, name, created_by, updated_by) VALUES ($1, $2, 'SEG', 'S', $3, $3)`, f.tenant, segment, f.actor)
	})
	post := func(t *testing.T, lines ...assignedLine) uuid.UUID {
		t.Helper()
		var je uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			var err error
			je, err = f.assignedEntry(tx, lines...)
			return err
		})
		return je
	}
	cc, pc := f.cc.String(), f.pc.String()
	first := post(t, assignedLine{account: f.expense, amount: 100, cc: &f.cc, pc: &f.pc}, assignedLine{account: f.expense, amount: -40, pc: &f.pc},
		assignedLine{account: f.bank, amount: 50, pc: &f.pc, seg: &segment}, assignedLine{account: f.bank, amount: -110})
	f.enqueue(t, first)
	check := func(t *testing.T, step string, want ...string) {
		t.Helper()
		got := f.assignmentBalances(t)
		slices.Sort(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", step, got, want)
		}
	}
	check(t, "queued, not projected")
	f.projectAll(t)
	projected := []string{cc + "/" + pc + "/- 100/0 90/0", "-/" + pc + "/- 0/40 0/36", "-/" + pc + "/" + segment.String() + " 50/0 45/0"}
	t.Run("the first projection adds the assigned lines", func(t *testing.T) { check(t, "projected", projected...) })
	t.Run("a projected entry is not counted again", func(t *testing.T) {
		f.projectAll(t)
		check(t, "projected twice", projected...)
	})
	t.Run("a later entry adds to the balances of its key", func(t *testing.T) {
		f.enqueue(t, post(t, assignedLine{account: f.expense, amount: 7, cc: &f.cc, pc: &f.pc}, assignedLine{account: f.bank, amount: -7}))
		f.projectAll(t)
		var debit int64
		if err := testDB.QueryRow(context.Background(), `SELECT debit_lc FROM ledger.assignment_balance WHERE tenant_id = $1 AND cost_center_id = $2`,
			f.tenant, f.cc).Scan(&debit); err != nil {
			t.Fatal(err)
		}
		if debit != 107 {
			t.Errorf("cost center's debit: got %d, want 107", debit)
		}
	})
	t.Run("only the projection writes balances", func(t *testing.T) {
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.assignment_balance (tenant_id, ledger_id, legal_entity_id, account_id, profit_center_id, fiscal_year,
				fiscal_period, currency_tc) VALUES ($1, $2, $3, $4, $5, 2026, 3, 'USD')`, f.tenant, f.ledger, f.entity, f.expense, f.pc)
		})
		failsWith(t, err, "permission denied")
	})
	t.Run("a balance has a center", func(t *testing.T) {
		err := inRole(t, "erp_projector", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.assignment_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc)
				VALUES ($1, $2, $3, $4, 2026, 3, 'USD')`, f.tenant, f.ledger, f.entity, f.expense)
		})
		failsWith(t, err, "assignment_balance_assigned")
	})
}
