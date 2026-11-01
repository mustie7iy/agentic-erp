package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Parked manual entries (T35a, migration 000052): the rules the database
// keeps for a manual entry between parking and posting.

// submit moves a parked entry to pending approval on a new request.
const submit = "status = 'pending_approval', approval_request_id = gen_random_uuid()"

// park parks an entry of f with two lines, created by a principal of kind
// (a person owns what they park), and returns its id.
func (f fixture) park(t *testing.T, kind string) uuid.UUID {
	t.Helper()
	pe := id()
	var owner *uuid.UUID
	if kind == "human" {
		owner = &f.actor
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
			currency, created_by_kind, owner_id, created_by, updated_by) VALUES ($1,$2,$3,$4,'SA','2026-05-04','2026-05-04','USD',$5,$6,$7,$7)`,
			f.tenant, pe, f.entity, f.ledger, kind, owner, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, line_no, account_id, amount)
			VALUES ($1,$2,1,$3,100), ($1,$2,2,$4,-100)`, f.tenant, pe, f.expense, f.bank)
	})
	return pe
}

// change updates parked entry pe as the application, setting set.
func (f fixture) change(t *testing.T, pe uuid.UUID, set string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.parked_entry SET `+set+`, version = version + 1 WHERE id = $1`, append([]any{pe}, args...)...)
	})
}

// mustChange applies the updates in order, each of which must succeed.
func (f fixture) mustChange(t *testing.T, pe uuid.UUID, sets ...string) {
	t.Helper()
	for _, set := range sets {
		if err := f.change(t, pe, set); err != nil {
			t.Fatalf("%s: got %v, want no error", set, err)
		}
	}
}

// expect checks err against want: a database error containing want, or
// none when want is empty.
func expect(t *testing.T, err error, want string) {
	t.Helper()
	if want != "" {
		failsWith(t, err, want)
		return
	}
	if err != nil {
		t.Fatalf("got %v, want no error", err)
	}
}

func TestParkedEntryTransitions(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name  string
		after []string // updates applied first
		set   string
		want  string // empty: allowed
	}{
		{"edited while parked", nil, "header_text = 'Accrual'", ""},
		{"submitted", nil, submit, ""},
		{"cancelled", nil, "status = 'cancelled'", ""},
		{"rejected unsubmitted", nil, "status = 'rejected', approval_request_id = gen_random_uuid()", "cannot go from parked to rejected"},
		{"changed as it is submitted", nil, submit + ", posting_date = '2026-05-05'", "changes only while it is parked"},
		{"edited while pending", []string{submit}, "header_text = 'Accrual'", "cannot go from pending_approval to pending_approval"},
		{"withdrawn", []string{submit}, "status = 'parked', approval_request_id = NULL", ""},
		{"withdrawn naming the request", []string{submit}, "status = 'parked'", "parked_entry_request_when_decided"},
		{"rejected", []string{submit}, "status = 'rejected'", ""},
		{"rejected on another request", []string{submit}, "status = 'rejected', approval_request_id = gen_random_uuid()", "decided only on the request it names"},
		{"rejected entry reopened", []string{submit, "status = 'rejected'"}, "status = 'parked', approval_request_id = NULL", "cannot go from rejected to parked"},
		{"cancelled entry reopened", []string{"status = 'cancelled'"}, "status = 'parked'", "cannot go from cancelled to parked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			pe := f.park(t, "human")
			f.mustChange(t, pe, c.after...)
			expect(t, f.change(t, pe, c.set), c.want)
		})
	}
	t.Run("parked as anything else", func(t *testing.T) {
		f := newFixture(t)
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
				currency, status, created_by_kind, owner_id, created_by, updated_by) VALUES ($1,$2,$3,$4,'SA','2026-05-04','2026-05-04','USD','cancelled','human',$5,$5,$5)`,
				f.tenant, id(), f.entity, f.ledger, f.actor)
		})
		failsWith(t, err, "an entry is parked first")
	})
}

// post posts a journal entry dated date for source (none when nil) and
// links parked entry pe to it, applying set too.
func (f fixture) post(t *testing.T, pe uuid.UUID, source *uuid.UUID, date, set string) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error {
		je, err := f.entryFor(tx, source, 5, date, line{account: f.expense, amount: 100}, line{account: f.bank, amount: -100})
		if err != nil {
			return err
		}
		return exec(tx, `UPDATE ledger.parked_entry SET `+set+`journal_fiscal_year = 2026, journal_entry_id = $2, version = version + 1
			WHERE id = $1`, pe, je)
	})
}

func TestParkedEntryPosting(t *testing.T) {
	need(t)
	itself := func(pe uuid.UUID) *uuid.UUID { return &pe }
	other := func(uuid.UUID) *uuid.UUID { return nil }
	for _, c := range []struct {
		name   string
		after  []string
		source func(uuid.UUID) *uuid.UUID
		date   string
		set    string
		want   string
	}{
		{"directly", nil, itself, "2026-05-04", "status = 'posted', ", ""},
		{"directly naming a request", nil, itself, "2026-05-04", "status = 'posted', approval_request_id = gen_random_uuid(), ", "names no approval request"},
		{"on its request", []string{submit}, itself, "2026-05-04", "status = 'posted', ", ""},
		{"on another request", []string{submit}, itself, "2026-05-04", "status = 'posted', approval_request_id = gen_random_uuid(), ", "decided only on the request it names"},
		{"to an entry posted for something else", nil, other, "2026-05-04", "status = 'posted', ", "was not posted for parked entry"},
		{"to an entry of another date", nil, itself, "2026-05-05", "status = 'posted', ", "was not posted for parked entry"},
		{"naming a journal entry while parked", nil, itself, "2026-05-04", "", "parked_entry_journal_when_posted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			pe := f.park(t, "human")
			f.mustChange(t, pe, c.after...)
			expect(t, f.post(t, pe, c.source(pe), c.date, c.set), c.want)
		})
	}
	t.Run("without a journal entry", func(t *testing.T) {
		f := newFixture(t)
		pe := f.park(t, "human")
		failsWith(t, f.change(t, pe, "status = 'posted'"), "parked_entry_journal_when_posted")
		failsWith(t, f.change(t, pe, "journal_fiscal_year = 2026"), "parked_entry_journal_year")
	})
	t.Run("one entry per request", func(t *testing.T) {
		f := newFixture(t)
		first, second := f.park(t, "human"), f.park(t, "human")
		request := id()
		f.mustChange(t, first, "status = 'pending_approval', approval_request_id = '"+request.String()+"'")
		failsWith(t, f.change(t, second, "status = 'pending_approval', approval_request_id = $2", request), "parked_entry_one_per_request")
	})
	t.Run("changed once posted", func(t *testing.T) {
		f := newFixture(t)
		pe := f.park(t, "human")
		expect(t, f.post(t, pe, &pe, "2026-05-04", "status = 'posted', "), "")
		failsWith(t, f.change(t, pe, "header_text = 'Accrual'"), "cannot go from posted to posted")
	})
	t.Run("to the reversal of its entry", func(t *testing.T) {
		f := newFixture(t)
		pe := f.park(t, "human")
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			je, err := f.entryFor(tx, &pe, 5, "2026-05-04", line{account: f.expense, amount: 100}, line{account: f.bank, amount: -100})
			if err != nil {
				return err
			}
			rev := id()
			if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
				document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context,
				source_document_id, reversal_of_id, reversal_of_year, posted_by)
				SELECT tenant_id, fiscal_year, $2, legal_entity_id, ledger_id, fiscal_period, document_type, document_number || 'R', document_date,
					posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, id, fiscal_year, posted_by
				FROM ledger.journal_entry WHERE id = $1`, je, rev); err != nil {
				return err
			}
			if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id,
				fiscal_period, posting_date, account_id, amount_tc, amount_lc, amount_gc)
				SELECT tenant_id, fiscal_year, $2, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date, account_id, -amount_tc, -amount_lc, -amount_gc
				FROM ledger.journal_line WHERE journal_entry_id = $1`, je, rev); err != nil {
				return err
			}
			return exec(tx, `UPDATE ledger.parked_entry SET status = 'posted', journal_fiscal_year = 2026, journal_entry_id = $2, version = version + 1
				WHERE id = $1`, pe, rev)
		})
		failsWith(t, err, "was not posted for parked entry")
	})
}

func TestParkedEntryOwners(t *testing.T) {
	need(t)
	t.Run("a person's entry is theirs", func(t *testing.T) {
		f := newFixture(t)
		pe := f.park(t, "human")
		failsWith(t, f.change(t, pe, "owner_id = $2", id()), "the owner of parked entry")
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
				currency, created_by_kind, owner_id, created_by, updated_by) VALUES ($1,$2,$3,$4,'SA','2026-05-04','2026-05-04','USD','human',$5,$6,$6)`,
				f.tenant, id(), f.entity, f.ledger, id(), f.actor)
		})
		failsWith(t, err, "parked_entry_person_owns")
	})
	for _, c := range []struct {
		name  string
		after []string
		set   string
		want  string
	}{
		{"an agent's entry submitted unowned", nil, submit, "parked_entry_person_owns"},
		{"an agent's entry cancelled unowned", nil, "status = 'cancelled'", ""},
		{"an agent's entry adopted", nil, "owner_id = gen_random_uuid()", ""},
		{"an adopted entry submitted", []string{"owner_id = gen_random_uuid()"}, submit, ""},
		{"an adopted entry adopted again", []string{"owner_id = gen_random_uuid()"}, "owner_id = gen_random_uuid()", "the owner of parked entry"},
		{"an adopted entry disowned", []string{"owner_id = gen_random_uuid()"}, "owner_id = NULL", "the owner of parked entry"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			pe := f.park(t, "agent")
			f.mustChange(t, pe, c.after...)
			expect(t, f.change(t, pe, c.set), c.want)
		})
	}
}

func TestParkedEntryLines(t *testing.T) {
	need(t)
	setLine := func(f fixture, set string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.parked_entry_line SET `+set+` WHERE line_no = 1`)
		})
	}
	add := func(f fixture, pe uuid.UUID, amount int64, unit any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, line_no, account_id, amount, quantity, uom_code)
				VALUES ($1,$2,3,$3,$4,$5,$6)`, f.tenant, pe, f.expense, amount, nil, unit)
		})
	}
	t.Run("changed while parked", func(t *testing.T) {
		f := newFixture(t)
		f.park(t, "human")
		expect(t, setLine(f, "amount = 250"), "")
		expect(t, setLine(f, "is_removed = true"), "")
	})
	t.Run("frozen once submitted", func(t *testing.T) {
		f := newFixture(t)
		pe := f.park(t, "human")
		f.mustChange(t, pe, submit)
		failsWith(t, setLine(f, "amount = 250"), "items of a pending_approval ledger.parked_entry cannot change")
		failsWith(t, add(f, pe, 5, nil), "items of a pending_approval ledger.parked_entry cannot change")
	})
	t.Run("moved off a submitted entry", func(t *testing.T) {
		f := newFixture(t)
		submitted, open := f.park(t, "human"), f.park(t, "human")
		f.mustChange(t, submitted, submit)
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.parked_entry_line SET parked_entry_id = $2, line_no = 9 WHERE parked_entry_id = $1 AND line_no = 1`, submitted, open)
		})
		failsWith(t, err, "stays on its parked entry")
	})
	t.Run("zero amount", func(t *testing.T) {
		f := newFixture(t)
		failsWith(t, add(f, f.park(t, "human"), 0, nil), "parked_entry_line_amount_check")
	})
	t.Run("unit without quantity", func(t *testing.T) {
		f := newFixture(t)
		failsWith(t, add(f, f.park(t, "human"), 5, "EA"), "parked_entry_line_check")
	})
}

func TestParkedEntryAccess(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	a.park(t, "human")
	var seen int
	mustApp(t, b.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM ledger.parked_entry").Scan(&seen)
	})
	if seen != 0 {
		t.Errorf("another tenant's parked entries seen: got %d, want 0", seen)
	}
	for code, want := range map[string]bool{"ledger.entry.park": false, "ledger.entry.approve": true} {
		t.Run(code, func(t *testing.T) {
			var approval bool
			if err := testDB.QueryRow(context.Background(), "SELECT is_approval FROM ref.permission WHERE code = $1", code).Scan(&approval); err != nil {
				t.Fatalf("permission %s: %v", code, err)
			}
			if approval != want {
				t.Errorf("%s is_approval: got %v, want %v", code, approval, want)
			}
		})
	}
}

// TestParkedEntryProvenance: who parked an entry, and how, is never
// rewritten, and the document type has the journal's format.
func TestParkedEntryProvenance(t *testing.T) {
	need(t)
	for _, set := range []string{"created_by_kind = 'human', owner_id = created_by", "created_by = gen_random_uuid()", "created_at = now() - interval '1 day'"} {
		t.Run(set, func(t *testing.T) {
			f := newFixture(t)
			failsWith(t, f.change(t, f.park(t, "agent"), set), "who parked entry")
		})
	}
	t.Run("document type", func(t *testing.T) {
		f := newFixture(t)
		failsWith(t, f.change(t, f.park(t, "human"), "document_type = 'x'"), "parked_entry_document_type_check")
	})
}

// parkByLimit parks an entry of f as a principal of kind with the limit it
// was parked by, the columns and values given (T61n, 000175).
func (f fixture) parkByLimit(t *testing.T, kind, columns, values string) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
			currency, created_by_kind, created_by, updated_by, `+columns+`) VALUES ($1,$2,$3,$4,'SA','2026-05-04','2026-05-04','USD',$5,$6,$6, `+values+`)`,
			f.tenant, id(), f.entity, f.ledger, kind, f.actor)
	})
}

// TestParkedEntryLimit: an entry the ERP parked by a limit names the kind,
// its debits and the limit, each with its currency, only for a service
// principal's entry, and never rewrites them (ADR-0049 §7, T61n).
func TestParkedEntryLimit(t *testing.T) {
	need(t)
	const columns = "limit_kind, limit_debits, limit_debits_currency, limit_amount, limit_currency"
	for _, c := range []struct {
		name, kind, values, want string
	}{
		{"a service's entry above its limit", "service", "'service_posting_limit', 105000, 'CAD', 100000, 'CAD'", ""},
		{"in another currency", "service", "'service_posting_limit', 80001, 'USD', 100000, 'EUR'", ""},
		{"another kind of limit", "service", "'manual_entry_approval_limit', 105000, 'CAD', 100000, 'CAD'", "parked_entry_limit_kind_check"},
		{"without the debits", "service", "'service_posting_limit', NULL, 'CAD', 100000, 'CAD'", "parked_entry_limit_figures"},
		{"without the debits' currency", "service", "'service_posting_limit', 105000, NULL, 100000, 'CAD'", "parked_entry_limit_figures"},
		{"without the limit", "service", "'service_posting_limit', 105000, 'CAD', NULL, 'CAD'", "parked_entry_limit_figures"},
		{"without the limit's currency", "service", "'service_posting_limit', 105000, 'CAD', 100000, NULL", "parked_entry_limit_figures"},
		{"figures without a kind", "service", "NULL, 105000, 'CAD', 100000, 'CAD'", "parked_entry_limit_figures"},
		{"negative debits", "service", "'service_posting_limit', -1, 'CAD', 100000, 'CAD'", "parked_entry_limit_figures"},
		{"a negative limit", "service", "'service_posting_limit', 105000, 'CAD', -1, 'CAD'", "parked_entry_limit_figures"},
		{"an unknown currency of the debits", "service", "'service_posting_limit', 105000, 'XXQ', 100000, 'CAD'", "parked_entry_limit_debits_currency_fkey"},
		{"an unknown currency of the limit", "service", "'service_posting_limit', 105000, 'CAD', 100000, 'XXQ'", "parked_entry_limit_currency_fkey"},
		{"an agent's entry", "agent", "'service_posting_limit', 105000, 'CAD', 100000, 'CAD'", "parked_entry_limit_by_service"},
	} {
		t.Run(c.name, func(t *testing.T) {
			expect(t, newFixture(t).parkByLimit(t, c.kind, columns, c.values), c.want)
		})
	}
	for _, set := range []string{"limit_kind = NULL, limit_debits = NULL, limit_debits_currency = NULL, limit_amount = NULL, limit_currency = NULL",
		"limit_debits = 1", "limit_debits_currency = 'EUR'", "limit_amount = 1", "limit_currency = 'EUR'"} {
		t.Run("rewritten: "+set, func(t *testing.T) {
			f := newFixture(t)
			if err := f.parkByLimit(t, "service", columns, "'service_posting_limit', 105000, 'CAD', 100000, 'CAD'"); err != nil {
				t.Fatal(err)
			}
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `UPDATE ledger.parked_entry SET `+set+`, version = version + 1 WHERE created_by_kind = 'service'`)
			})
			failsWith(t, err, "was parked does not change")
		})
	}
	t.Run("set later", func(t *testing.T) {
		f := newFixture(t)
		pe := f.park(t, "service")
		failsWith(t, f.change(t, pe, "limit_kind = 'service_posting_limit', limit_debits = 1, limit_debits_currency = 'USD', limit_amount = 0, limit_currency = 'USD'"),
			"was parked does not change")
	})
}
