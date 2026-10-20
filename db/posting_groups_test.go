package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// copyOf is the posting group and ledger group of a journal entry: the
// first entry's id and the group named, nil for none.
type copyOf struct {
	group, ledgerGroup *uuid.UUID
}

// posted inserts a balanced entry of f in March 2026 in ledger, numbered
// number, as part of g; it returns the entry's id.
func (f fixture) posted(tx pgx.Tx, ledger uuid.UUID, number string, g copyOf) (uuid.UUID, error) {
	je := id()
	if g.group == nil {
		g.group = &je
	}
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by,
		posting_group_id, ledger_group_id)
		VALUES ($1,2026,$2,$3,$4,3,'SA',$5,'2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,'ledger',$6,$7,$8)`,
		f.tenant, je, f.entity, ledger, number, f.actor, g.group, g.ledgerGroup); err != nil {
		return je, err
	}
	return je, exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
		posting_date, account_id, amount_tc, amount_lc, amount_gc) VALUES ($1,2026,$2,1,$3,$4,3,'2026-03-10',$5,100,100,90), ($1,2026,$2,2,$3,$4,3,'2026-03-10',$6,-100,-100,-90)`,
		f.tenant, je, f.entity, ledger, f.bank, f.expense)
}

// parallelLedger adds ledger 2L with an open March 2026 for f's company,
// assigned to it with 0L when assign is set, and a group G1 of 0L alone.
func (f fixture) parallelLedger(tx pgx.Tx, assign bool) (local, g1 uuid.UUID, err error) {
	local, g1 = id(), id()
	steps := []stmt{
		step(`INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1, $2, '2L', 'Local', false, 'HGB', $3)`,
			f.tenant, local, f.actor),
		step(`INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
			VALUES ($1, gen_random_uuid(), $2, $3, $4, 2026, 3, 'open', $5)`, f.tenant, f.entity, local, f.calendar, f.actor),
		step(`INSERT INTO ledger.ledger_group (tenant_id, id, code, name, representative_ledger_id, created_by, updated_by) VALUES ($1, $2, 'G1', 'Group', $3, $4, $4)`,
			f.tenant, g1, f.ledger, f.actor),
		step(`INSERT INTO ledger.ledger_group_member (tenant_id, group_id, ledger_id, created_by) VALUES ($1, $2, $3, $4)`, f.tenant, g1, f.ledger, f.actor),
	}
	if assign {
		const ins = `INSERT INTO ledger.company_ledger (tenant_id, legal_entity_id, ledger_id, is_leading, created_by) VALUES ($1, $2, $3, $4, $5)`
		steps = append(steps, step(ins, f.tenant, f.entity, f.ledger, true, f.actor), step(ins, f.tenant, f.entity, local, false, f.actor))
	}
	return local, g1, all(tx, steps...)
}

// Posting groups (T52b, migrations 000092-000095): an entry goes to a
// ledger its company keeps (the leading one before it is assigned any), a
// member of the ledger group it names, and a copy repeats its posting
// group's first entry in another ledger, under the same document number,
// unique per ledger.
func TestPostingGroupRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name   string
		assign bool
		run    func(tx pgx.Tx, f fixture, local, g1 uuid.UUID) error
		want   string // empty: allowed
	}{
		{"the leading ledger of a company not assigned ledgers", false, func(tx pgx.Tx, f fixture, _, _ uuid.UUID) error {
			_, err := f.posted(tx, f.ledger, "1", copyOf{})
			return err
		}, ""},
		{"another ledger of a company not assigned ledgers", false, func(tx pgx.Tx, f fixture, local, _ uuid.UUID) error {
			_, err := f.posted(tx, local, "1", copyOf{})
			return err
		}, "does not keep ledger"},
		{"a copy in each ledger under one number", true, func(tx pgx.Tx, f fixture, local, _ uuid.UUID) error {
			first, err := f.posted(tx, f.ledger, "1", copyOf{})
			if err != nil {
				return err
			}
			_, err = f.posted(tx, local, "1", copyOf{group: &first})
			return err
		}, ""},
		{"a ledger the company does not keep", true, func(tx pgx.Tx, f fixture, _, _ uuid.UUID) error {
			other := id()
			if err := exec(tx, `INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1, $2, '3L', 'Tax', false, 'TAX', $3)`,
				f.tenant, other, f.actor); err != nil {
				return err
			}
			_, err := f.posted(tx, other, "1", copyOf{})
			return err
		}, "does not keep ledger"},
		{"a number twice in a ledger", true, func(tx pgx.Tx, f fixture, _, _ uuid.UUID) error {
			if _, err := f.posted(tx, f.ledger, "1", copyOf{}); err != nil {
				return err
			}
			_, err := f.posted(tx, f.ledger, "1", copyOf{})
			return err
		}, "journal_entry_document_y2026"},
		{"a copy under another number", true, func(tx pgx.Tx, f fixture, local, _ uuid.UUID) error {
			first, err := f.posted(tx, f.ledger, "1", copyOf{})
			if err != nil {
				return err
			}
			_, err = f.posted(tx, local, "2", copyOf{group: &first})
			return err
		}, "is no copy of the first entry"},
		{"a copy in its first entry's ledger", true, func(tx pgx.Tx, f fixture, _, _ uuid.UUID) error {
			first, err := f.posted(tx, f.ledger, "1", copyOf{})
			if err != nil {
				return err
			}
			_, err = f.posted(tx, f.ledger, "2", copyOf{group: &first})
			return err
		}, "is no copy of the first entry"},
		{"a copy of a copy", true, func(tx pgx.Tx, f fixture, local, _ uuid.UUID) error {
			first, err := f.posted(tx, f.ledger, "1", copyOf{})
			if err != nil {
				return err
			}
			second, err := f.posted(tx, local, "1", copyOf{group: &first})
			if err != nil {
				return err
			}
			_, err = f.posted(tx, f.ledger, "1", copyOf{group: &second})
			return err
		}, "is no copy of the first entry"},
		{"a member of the group named", true, func(tx pgx.Tx, f fixture, _, g1 uuid.UUID) error {
			_, err := f.posted(tx, f.ledger, "1", copyOf{ledgerGroup: &g1})
			return err
		}, ""},
		{"a ledger outside the group named", true, func(tx pgx.Tx, f fixture, local, g1 uuid.UUID) error {
			_, err := f.posted(tx, local, "1", copyOf{ledgerGroup: &g1})
			return err
		}, "is not a member of ledger group"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				local, g1, err := f.parallelLedger(tx, c.assign)
				if err != nil {
					return err
				}
				return c.run(tx, f, local, g1)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// A parked entry names the ledger it posts to alone or a ledger group, not
// both, and what it posts to does not change.
func TestParkedEntryLedgers(t *testing.T) {
	need(t)
	f := newFixture(t)
	var g1 uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		_, g1, err = f.parallelLedger(tx, false)
		return err
	})
	pe := f.park(t, "human")
	t.Run("parked with a ledger and a group", func(t *testing.T) {
		expect(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
				currency, created_by_kind, owner_id, created_by, updated_by, ledger_specific, ledger_group_id)
				VALUES ($1, $2, $3, $4, 'SA', '2026-05-04', '2026-05-04', 'USD', 'human', $5, $5, $5, true, $6)`, f.tenant, id(), f.entity, f.ledger, f.actor, g1)
		}), "parked_entry_ledger_or_group")
	})
	for _, c := range []struct {
		name, set string
		args      []any
		want      string
	}{
		{"to its ledger alone", "ledger_specific = true", nil, "posts to do not change"},
		{"to a group", "ledger_group_id = $2", []any{g1}, "posts to do not change"},
		{"its header", "header_text = 'x'", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) { expect(t, f.change(t, pe, c.set, c.args...), c.want) })
	}
}
