package db

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The trading partner on journal lines (T68b, ADR-0059 §4; migrations
// 000216-000218): a line's trading partner is never its own company, the
// check holds on every partition, and marking an entry projected adds its
// lines with a trading partner to ledger.trading_partner_balance once.

// partnerFixture is the fixture's tenant with a second company, the
// trading partner of its lines.
type partnerFixture struct {
	fixture
	partner uuid.UUID
}

func newPartners(t *testing.T) partnerFixture {
	t.Helper()
	f := partnerFixture{fixture: newFixture(t), partner: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
			VALUES ($1, $2, '3000', 'Maple', 'CA', 'CAD', $3, 'active', $4, $4)`, f.tenant, f.partner, f.calendar, f.actor)
	})
	return f
}

// partnerLine is a line of partnerEntry: its account, amount in USD and
// trading partner (nil: none).
type partnerLine struct {
	account uuid.UUID
	amount  int64
	partner *uuid.UUID
}

// partnerEntry inserts an entry of the fixture's company in March 2026
// with the lines and returns its id.
func (f partnerFixture) partnerEntry(tx pgx.Tx, lines ...partnerLine) (uuid.UUID, error) {
	je := id()
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		VALUES ($1,2026,$2,$3,$4,3,'SA',$5,'2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,'ledger',$6)`,
		f.tenant, je, f.entity, f.ledger, je.String()[24:], f.actor); err != nil {
		return je, err
	}
	for i, l := range lines {
		if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, trading_partner_id)
			VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,$9)`,
			f.tenant, je, i+1, f.entity, f.ledger, l.account, l.amount, l.amount*9/10, l.partner); err != nil {
			return je, err
		}
	}
	return je, nil
}

func TestJournalLineTradingPartner(t *testing.T) {
	need(t)
	f := newPartners(t)
	t.Run("another company of the tenant", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			_, err := f.partnerEntry(tx, partnerLine{f.bank, 100, &f.partner}, partnerLine{f.expense, -100, nil})
			return err
		})
	})
	t.Run("never the line's own company", func(t *testing.T) {
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			_, err := f.partnerEntry(tx, partnerLine{f.bank, 100, &f.entity}, partnerLine{f.expense, -100, nil})
			return err
		})
		failsWith(t, err, "journal_line_trading_partner")
	})
	t.Run("the check is validated on the table and every partition", func(t *testing.T) {
		var tables, checked int
		if err := testDB.QueryRow(context.Background(), `SELECT (SELECT count(*) + 1 FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass),
				(SELECT count(*) FROM pg_constraint WHERE conname = 'journal_line_trading_partner' AND convalidated)`).Scan(&tables, &checked); err != nil {
			t.Fatal(err)
		}
		if checked != tables {
			t.Errorf("validated checks: got %d, want %d (the table and its partitions)", checked, tables)
		}
	})
	t.Run("the lines are indexed by trading partner", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT i.indisvalid FROM pg_index i
				WHERE i.indexrelid = 'ledger.journal_line_by_trading_partner'::regclass`).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Error("journal_line_by_trading_partner: got invalid, want valid")
		}
	})
}

// tradingPartnerBalances lists the tenant's balances as "account/partner
// debit_lc/credit_lc debit_gc/credit_gc", sorted.
func (f partnerFixture) tradingPartnerBalances(t *testing.T) []string {
	t.Helper()
	rows, _ := testDB.Query(context.Background(), `SELECT account_id, trading_partner_id, debit_lc, credit_lc, debit_gc, credit_gc
		FROM ledger.trading_partner_balance WHERE tenant_id = $1`, f.tenant)
	var out []string
	var account, partner uuid.UUID
	var dl, cl, dg, cg int64
	if _, err := pgx.ForEachRow(rows, []any{&account, &partner, &dl, &cl, &dg, &cg}, func() error {
		out = append(out, fmt.Sprintf("%s/%s %d/%d %d/%d", account, partner, dl, cl, dg, cg))
		return nil
	}); err != nil {
		t.Fatalf("read trading partner balances: %v", err)
	}
	slices.Sort(out)
	return out
}

func TestTradingPartnerBalance(t *testing.T) {
	need(t)
	f := newPartners(t)
	c := centerFixture{fixture: f.fixture}
	post := func(t *testing.T, lines ...partnerLine) uuid.UUID {
		t.Helper()
		var je uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			var err error
			je, err = f.partnerEntry(tx, lines...)
			return err
		})
		return je
	}
	check := func(t *testing.T, step string, want ...string) {
		t.Helper()
		got := f.tradingPartnerBalances(t)
		slices.Sort(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", step, got, want)
		}
	}
	c.enqueue(t, post(t, partnerLine{f.bank, 100, &f.partner}, partnerLine{f.bank, -30, &f.partner}, partnerLine{f.expense, -70, nil}))
	check(t, "queued, not projected")
	c.projectAll(t)
	projected := []string{fmt.Sprintf("%s/%s 100/30 90/27", f.bank, f.partner)}
	t.Run("the first projection adds the lines with a trading partner", func(t *testing.T) { check(t, "projected", projected...) })
	t.Run("a projected entry is not counted again", func(t *testing.T) {
		c.projectAll(t)
		check(t, "projected twice", projected...)
	})
	t.Run("a later entry adds to the balance of its key", func(t *testing.T) {
		c.enqueue(t, post(t, partnerLine{f.bank, 10, &f.partner}, partnerLine{f.expense, -10, &f.partner}))
		c.projectAll(t)
		check(t, "later", fmt.Sprintf("%s/%s 110/30 99/27", f.bank, f.partner), fmt.Sprintf("%s/%s 0/10 0/9", f.expense, f.partner))
	})
	t.Run("only the projection writes balances", func(t *testing.T) {
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.trading_partner_balance (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year,
				fiscal_period, currency_tc) VALUES ($1, $2, $3, $4, $5, 2026, 4, 'USD')`, f.tenant, f.ledger, f.entity, f.bank, f.partner)
		})
		failsWith(t, err, "permission denied")
	})
	t.Run("a balance's trading partner is never its own company", func(t *testing.T) {
		err := inRole(t, "erp_projector", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.trading_partner_balance (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year,
				fiscal_period, currency_tc) VALUES ($1, $2, $3, $4, $3, 2026, 4, 'USD')`, f.tenant, f.ledger, f.entity, f.bank)
		})
		failsWith(t, err, "trading_partner_balance_partner")
	})
	t.Run("another tenant reads none", func(t *testing.T) {
		other := newFixture(t)
		mustApp(t, other.tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM ledger.trading_partner_balance`).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("balances another tenant reads: got %d, want 0", n)
			}
			return nil
		})
	})
}

// TestBusinessPartnerGroupCompany: a business partner's group company
// (000219, T68c) is a legal entity id, nullable, written and cleared by
// the partner context in the tenant's own rows, and another tenant reads
// none of them (the table's row-level security).
func TestBusinessPartnerGroupCompany(t *testing.T) {
	need(t)
	f := newPartners(t)
	set := func(v any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE partner.business_partner SET group_company_id = $2, version = version + 1 WHERE id = $1`, f.supplier, v)
		})
	}
	read := func(t *testing.T) *uuid.UUID {
		t.Helper()
		var got *uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT group_company_id FROM partner.business_partner WHERE id = $1`, f.supplier).Scan(&got)
		})
		return got
	}
	t.Run("a partner created before is linked to none", func(t *testing.T) {
		if got := read(t); got != nil {
			t.Errorf("group company: got %v, want none", *got)
		}
	})
	t.Run("set to a company of the tenant", func(t *testing.T) {
		if err := set(f.partner); err != nil {
			t.Fatal(err)
		}
		if got := read(t); got == nil || *got != f.partner {
			t.Errorf("group company: got %v, want %s", got, f.partner)
		}
	})
	t.Run("another tenant reads none", func(t *testing.T) {
		other := newFixture(t)
		mustApp(t, other.tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM partner.business_partner WHERE group_company_id IS NOT NULL`).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("linked partners another tenant reads: got %d, want 0", n)
			}
			return nil
		})
	})
	t.Run("cleared", func(t *testing.T) {
		if err := set(nil); err != nil {
			t.Fatal(err)
		}
		if got := read(t); got != nil {
			t.Errorf("group company: got %v, want none", *got)
		}
	})
}
