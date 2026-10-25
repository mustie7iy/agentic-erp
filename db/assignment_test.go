package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Account assignment on the journal line (T56d, migrations 000155-000158):
// the centers' companies by foreign key, on every partition. ADR-0050 §5's
// table is checked at commit from T56d3.

// assignedLine is a journal line with its centers (nil: none).
type assignedLine struct {
	account     uuid.UUID
	amount      int64
	cc, pc, seg *uuid.UUID
}

// assignedEntry inserts an entry of the fixture's company in March 2026
// with the lines.
func (f centerFixture) assignedEntry(tx pgx.Tx, lines ...assignedLine) error {
	je := id()
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		VALUES ($1,2026,$2,$3,$4,3,'SA',$5,'2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,'ledger',$6)`,
		f.tenant, je, f.entity, f.ledger, je.String()[24:], f.actor); err != nil {
		return err
	}
	for i, l := range lines {
		if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, cost_center_id, profit_center_id, segment_id)
			VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,$9,$10,$11)`,
			f.tenant, je, i+1, f.entity, f.ledger, l.account, l.amount, l.amount*9/10, l.cc, l.pc, l.seg); err != nil {
			return err
		}
	}
	return nil
}

func TestJournalLineAssignment(t *testing.T) {
	need(t)
	f := newCenters(t)
	revenue, segment, otherCC, otherPC := id(), id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []stmt{
			step(`UPDATE ledger.gl_account SET cost_element = 'primary_cost', version = version + 1 WHERE id = $1`, f.expense),
			step(`INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, cost_element, status, created_by, updated_by)
				VALUES ($1, $2, $3, '400000', 'Revenue', 'revenue', 'revenue', 'active', $4, $4)`, f.tenant, revenue, f.chart, f.actor),
			step(`INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`,
				f.tenant, f.entity, revenue, f.actor),
			step(`INSERT INTO ledger.segment (tenant_id, id, code, name, created_by, updated_by) VALUES ($1, $2, 'SEG', 'S', $3, $3)`, f.tenant, segment, f.actor),
			step(`INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, $2, $3, 'OTHER', 'CC', 'J', 'pending', $4, $4)`, f.tenant, otherCC, f.other, f.actor),
			step(`INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, $2, 'OTHER', 'PC', 'J', 'pending', $3, $3)`, f.tenant, otherPC, f.actor),
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	post := func(t *testing.T, lines ...assignedLine) error {
		t.Helper()
		return asApp(t, f.tenant, func(tx pgx.Tx) error { return f.assignedEntry(tx, lines...) })
	}
	bank := func(amount int64) assignedLine { return assignedLine{account: f.bank, amount: amount} }
	for name, c := range map[string]struct {
		lines []assignedLine
		want  string
	}{
		"another company's cost center": {[]assignedLine{{account: f.expense, amount: 100, cc: &otherCC, pc: &f.pc}, bank(-100)},
			"journal_line_cost_center_fkey"},
		"a profit center not assigned to the company": {[]assignedLine{{account: revenue, amount: -100, pc: &otherPC}, bank(100)},
			"journal_line_profit_center_fkey"},
		"an unknown segment": {[]assignedLine{{account: revenue, amount: -100, pc: &f.pc}, {account: f.bank, amount: 100, pc: &f.pc, seg: &f.cc}},
			"journal_line_segment_fkey"},
	} {
		t.Run(name, func(t *testing.T) {
			err := post(t, c.lines...)
			failsWith(t, err, c.want)
		})
	}
	t.Run("centers of the company", func(t *testing.T) {
		if err := post(t, assignedLine{account: f.expense, amount: 100, cc: &f.cc, pc: &f.pc}, assignedLine{account: revenue, amount: -100, pc: &f.pc},
			assignedLine{account: f.bank, amount: 50, pc: &f.pc, seg: &segment}, bank(-50)); err != nil {
			t.Errorf("got %v, want them posted", err)
		}
	})
	t.Run("every partition has the keys validated and the indexes", func(t *testing.T) {
		var partitions, keys, indexes int
		if err := testDB.QueryRow(t.Context(), `SELECT count(*),
				(SELECT count(*) FROM pg_inherits i JOIN pg_constraint c ON c.conrelid = i.inhrelid WHERE i.inhparent = 'ledger.journal_line'::regclass
					AND c.conname IN ('journal_line_cost_center_fkey', 'journal_line_profit_center_fkey', 'journal_line_segment_fkey') AND c.convalidated
					AND c.conparentid <> 0),
				(SELECT count(*) FROM pg_inherits i JOIN pg_index x ON x.indrelid = i.inhrelid JOIN pg_inherits a ON a.inhrelid = x.indexrelid
					WHERE i.inhparent = 'ledger.journal_line'::regclass AND x.indisvalid
					AND a.inhparent IN ('ledger.journal_line_cost_center'::regclass, 'ledger.journal_line_profit_center'::regclass))
			FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass`).Scan(&partitions, &keys, &indexes); err != nil {
			t.Fatal(err)
		}
		if keys != 3*partitions || indexes != 2*partitions {
			t.Errorf("of %d partitions: got %d validated keys taken over by the parent and %d indexes, want %d and %d", partitions, keys, indexes, 3*partitions, 2*partitions)
		}
	})
}

// TestParkedLineAssignment: a parked line's entered centers are the
// tenant's (T56d2, migrations 000159-000160); posting checks the rest.
func TestParkedLineAssignment(t *testing.T) {
	need(t)
	f := newCenters(t)
	unknown := id()
	for name, c := range map[string]struct {
		column string
		center uuid.UUID
		want   string
	}{
		"an unknown cost center":     {"cost_center_id", unknown, "parked_entry_line_cost_center_fkey"},
		"an unknown profit center":   {"profit_center_id", unknown, "parked_entry_line_profit_center_fkey"},
		"an unknown segment":         {"segment_id", unknown, "parked_entry_line_segment_fkey"},
		"the tenant's cost center":   {"cost_center_id", f.cc, ""},
		"the tenant's profit center": {"profit_center_id", f.pc, ""},
	} {
		t.Run(name, func(t *testing.T) {
			pe := f.park(t, "human")
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `UPDATE ledger.parked_entry_line SET `+pgx.Identifier{c.column}.Sanitize()+` = $2 WHERE parked_entry_id = $1 AND line_no = 1`,
					pe, c.center)
			})
			expect(t, err, c.want)
		})
	}
	t.Run("the keys are validated", func(t *testing.T) {
		var valid int
		if err := testDB.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint WHERE conrelid = 'ledger.parked_entry_line'::regclass
			AND conname IN ('parked_entry_line_cost_center_fkey', 'parked_entry_line_profit_center_fkey', 'parked_entry_line_segment_fkey')
			AND convalidated`).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if valid != 3 {
			t.Errorf("validated keys: got %d, want 3", valid)
		}
	})
}
