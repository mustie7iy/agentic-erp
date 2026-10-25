package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Account assignment on the journal line (T56d, migrations 000155-000158):
// the centers' companies by foreign key and ADR-0050 §5's table at commit.

// assignedLine is a journal line with its centers (nil: none).
type assignedLine struct {
	account     uuid.UUID
	amount      int64
	cc, pc, seg *uuid.UUID
}

// assignedEntry inserts an entry of the fixture's company in March 2026
// with the lines, reversing reversalOf when set; it returns its id.
func (f centerFixture) assignedEntry(tx pgx.Tx, reversalOf *uuid.UUID, lines ...assignedLine) (uuid.UUID, error) {
	je := id()
	var year *int16
	if reversalOf != nil {
		y := int16(2026)
		year = &y
	}
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by,
		reversal_of_id, reversal_of_year)
		VALUES ($1,2026,$2,$3,$4,3,'SA',$5,'2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,'ledger',$6,$7,$8)`,
		f.tenant, je, f.entity, f.ledger, je.String()[24:], f.actor, reversalOf, year); err != nil {
		return je, err
	}
	for i, l := range lines {
		if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, cost_center_id, profit_center_id, segment_id)
			VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,$9,$10,$11)`,
			f.tenant, je, i+1, f.entity, f.ledger, l.account, l.amount, l.amount*9/10, l.cc, l.pc, l.seg); err != nil {
			return je, err
		}
	}
	return je, nil
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
	post := func(t *testing.T, reversalOf *uuid.UUID, lines ...assignedLine) (uuid.UUID, error) {
		t.Helper()
		var je uuid.UUID
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			var err error
			je, err = f.assignedEntry(tx, reversalOf, lines...)
			return err
		})
		return je, err
	}
	bank := func(amount int64) assignedLine { return assignedLine{account: f.bank, amount: amount} }
	unassigned, err := post(t, nil, assignedLine{account: f.expense, amount: 100}, bank(-100))
	if err != nil {
		t.Fatalf("a cost line before center accounting is on: %v", err)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.company_controlling (tenant_id, legal_entity_id, cost_center_accounting_from, profit_center_accounting_from,
			default_profit_center_id, created_by, updated_by) VALUES ($1, $2, 2026, 2026, $3, $4, $4)`, f.tenant, f.entity, f.pc, f.actor)
	})
	for name, c := range map[string]struct {
		lines []assignedLine
		want  string
	}{
		"a primary cost line without a cost center": {[]assignedLine{{account: f.expense, amount: 100, pc: &f.pc}, bank(-100)},
			"a primary cost line needs a cost center"},
		"a cost center on an account that takes none": {[]assignedLine{{account: f.bank, amount: -100, cc: &f.cc}, {account: f.expense, amount: 100, cc: &f.cc, pc: &f.pc}},
			"the account takes no cost center"},
		"revenue on a category that takes none": {[]assignedLine{{account: revenue, amount: -100, cc: &f.cc, pc: &f.pc}, bank(100)},
			"category takes no revenue"},
		"a P&L line without a profit center": {[]assignedLine{{account: revenue, amount: -100}, bank(100)}, "a P&L line needs a profit center"},
		"a segment without a profit center": {[]assignedLine{{account: revenue, amount: -100, pc: &f.pc}, {account: f.bank, amount: 100, seg: &segment}},
			"the segment is not its profit center's"},
		"another segment than the profit center's": {[]assignedLine{{account: revenue, amount: -100, pc: &f.pc, seg: &segment}, bank(100)},
			"the segment is not its profit center's"},
		"another company's cost center": {[]assignedLine{{account: f.expense, amount: 100, cc: &otherCC, pc: &f.pc}, bank(-100)},
			"journal_line_cost_center_fkey"},
		"a profit center not assigned to the company": {[]assignedLine{{account: revenue, amount: -100, pc: &otherPC}, bank(100)},
			"journal_line_profit_center_fkey"},
		"an unknown segment": {[]assignedLine{{account: revenue, amount: -100, pc: &f.pc}, {account: f.bank, amount: 100, pc: &f.pc, seg: &f.cc}},
			"journal_line_segment_fkey"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := post(t, nil, c.lines...)
			failsWith(t, err, c.want)
		})
	}
	t.Run("lines as the table wants them", func(t *testing.T) {
		if _, err := post(t, nil, assignedLine{account: f.expense, amount: 100, cc: &f.cc, pc: &f.pc}, assignedLine{account: revenue, amount: -100, pc: &f.pc},
			assignedLine{account: f.bank, amount: 50, pc: &f.pc}, bank(-50)); err != nil {
			t.Errorf("got %v, want them posted", err)
		}
	})
	t.Run("every partition has the keys validated and the indexes", func(t *testing.T) {
		var partitions, keys, indexes int
		if err := testDB.QueryRow(t.Context(), `SELECT count(*),
				(SELECT count(*) FROM pg_inherits i JOIN pg_constraint c ON c.conrelid = i.inhrelid WHERE i.inhparent = 'ledger.journal_line'::regclass
					AND c.conname IN ('journal_line_cost_center_fkey', 'journal_line_profit_center_fkey', 'journal_line_segment_fkey') AND c.convalidated),
				(SELECT count(*) FROM pg_inherits i JOIN pg_index x ON x.indrelid = i.inhrelid JOIN pg_inherits a ON a.inhrelid = x.indexrelid
					WHERE i.inhparent = 'ledger.journal_line'::regclass AND x.indisvalid
					AND a.inhparent IN ('ledger.journal_line_cost_center'::regclass, 'ledger.journal_line_profit_center'::regclass))
			FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass`).Scan(&partitions, &keys, &indexes); err != nil {
			t.Fatal(err)
		}
		if keys != 3*partitions || indexes != 2*partitions {
			t.Errorf("of %d partitions: got %d validated keys and %d indexes, want %d and %d", partitions, keys, indexes, 3*partitions, 2*partitions)
		}
	})
	t.Run("a reversal carries its original's lines", func(t *testing.T) {
		if _, err := post(t, &unassigned, assignedLine{account: f.expense, amount: -100}, bank(100)); err != nil {
			t.Errorf("got %v, want it posted", err)
		}
	})
}
