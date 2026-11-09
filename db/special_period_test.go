package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Postings into special periods (ADR-0060 §3, T64m1, migrations 000225 and
// 000226): a special period takes manual entries and reversals only, dated
// in its dates, the year's last regular period's; a parked entry names
// only a special period.

// withSpecial adds f's special period 13 of 2026, sharing period 12's
// dates, open in its ledger.
func (f fixture) withSpecial(t *testing.T) {
	t.Helper()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, "INSERT INTO ledger.fiscal_period VALUES ($1,$2,2026,13,'2026-12-01','2026-12-31',true)", f.tenant, f.calendar); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
			VALUES ($1,gen_random_uuid(),$2,$3,$4,2026,13,'open',$5)`, f.tenant, f.entity, f.ledger, f.calendar, f.actor)
	})
}

// posting inserts a balanced entry of f into period, dated date, posted by
// source, reversing reverses (none when nil).
func (f fixture) posting(tx pgx.Tx, period int, date, source string, reverses *uuid.UUID) (uuid.UUID, error) {
	je := id()
	var year *int
	if reverses != nil {
		y := 2026
		year = &y
	}
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context,
		reversal_of_id, reversal_of_year, posted_by)
		VALUES ($1,2026,$2,$3,$4,$5,'SA',$6,$7,$7,'USD','USD','EUR',1,0.9,$8,$9,$10,$11)`,
		f.tenant, je, f.entity, f.ledger, period, je.String()[24:], date, source, reverses, year, f.actor); err != nil {
		return je, err
	}
	return je, exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
		posting_date, account_id, amount_tc, amount_lc, amount_gc) VALUES ($1,2026,$2,1,$3,$4,$5,$6,$7,10,10,9), ($1,2026,$2,2,$3,$4,$5,$6,$8,-10,-10,-9)`,
		f.tenant, je, f.entity, f.ledger, period, date, f.expense, f.bank)
}

// TestSpecialPeriodPostings: the database refuses a posting into a
// special period dated outside its dates or made by another context than
// the ledger, unless it is a reversal.
func TestSpecialPeriodPostings(t *testing.T) {
	need(t)
	f := newFixture(t)
	f.withSpecial(t)
	var invoice uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		invoice, err = f.posting(tx, 12, "2026-12-10", "payables", nil)
		return err
	})
	for _, c := range []struct {
		name, date, source string
		reverses           *uuid.UUID
		want               string // empty: allowed
	}{
		{"a manual entry dated in period 12", "2026-12-31", "ledger", nil, ""},
		{"a manual entry dated in period 11", "2026-11-30", "ledger", nil, "outside period 2026/13"},
		{"another context's posting", "2026-12-31", "payables", nil, "takes manual entries and reversals only, not a payables posting"},
		{"another context's reversal", "2026-12-31", "payables", &invoice, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				_, err := f.posting(tx, 13, c.date, c.source, c.reverses)
				return err
			})
			expect(t, err, c.want)
		})
	}
}

// TestParkedEntrySpecialPeriod: a parked entry names no period or one of
// the special periods 13 to 16.
func TestParkedEntrySpecialPeriod(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct {
		name  string
		value any
		want  string // empty: allowed
	}{
		{"none", nil, ""},
		{"13", 13, ""},
		{"16", 16, ""},
		{"a regular period", 12, "parked_entry_fiscal_period_special"},
		{"beyond the special periods", 17, "parked_entry_fiscal_period_special"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pe := f.park(t, "human")
			expect(t, f.change(t, pe, "fiscal_period = $2", c.value), c.want)
		})
	}
}
