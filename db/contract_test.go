package db

// Tests of the contract-step migrations (06-engineering-process §6.4): each
// migrates a database part of the way, plants the rows the previous release
// may leave behind, and checks what the contract step makes of them.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// migrateBefore applies every migration older than version to conn.
func migrateBefore(t *testing.T, conn *pgx.Conn, version int) {
	t.Helper()
	var before []Migration
	for _, m := range Migrations() {
		if v, _ := m.Version(); v < version { // the names were checked by TestMigrationFiles
			before = append(before, m)
		}
	}
	if _, err := migrate(context.Background(), conn, before); err != nil {
		t.Fatal(err)
	}
}

// inTenant runs fn in a committed transaction on conn, scoped to tenant,
// as the connection's (administrative) role: it sets the scene the way a
// release long gone left it, under no application grants.
func inTenant(t *testing.T, conn *pgx.Conn, tenant uuid.UUID, fn func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
			return err
		}
		return fn(tx)
	}); err != nil {
		t.Fatal(err)
	}
}

// payables is a scene of payment runs and the open items they pay, in a
// database migrated part of the way.
type payables struct {
	fixture
	conn *pgx.Conn
	bank uuid.UUID
}

func newPayables(t *testing.T, conn *pgx.Conn) *payables {
	t.Helper()
	p := &payables{fixture: fixtureIDs(), conn: conn, bank: id()}
	inTenant(t, conn, p.tenant, func(tx pgx.Tx) error {
		if err := p.seed(tx); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,'active',$5,$5)`, p.tenant, p.bank, p.entity, p.fixture.bank, p.actor)
	})
	return p
}

// item posts a supplier invoice; its open item is line 2.
func (p *payables) item(t *testing.T) uuid.UUID {
	t.Helper()
	var entry uuid.UUID
	inTenant(t, p.conn, p.tenant, func(tx pgx.Tx) error {
		var err error
		entry, err = p.entry(tx, 7, "2026-07-02", line{account: p.expense, amount: 400}, line{account: p.ap, amount: -400, partner: &p.supplier})
		return err
	})
	return entry
}

// run is a payment run in status, decided by decider (uuid.Nil: nobody).
type run struct {
	id, proposer, decider, updater uuid.UUID
	number, status                 string
}

func (p *payables) run(t *testing.T, number, status string, decided bool) run {
	t.Helper()
	r := run{id: id(), proposer: id(), updater: id(), number: number, status: status}
	if decided {
		r.decider = id()
	}
	inTenant(t, p.conn, p.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status,
			proposed_by, decided_by, decided_at, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,'2026-07-31','2026-07-31','USD',0,$6,$7,$8,CASE WHEN $8::uuid IS NOT NULL THEN now() END,$7,$9)`,
			p.tenant, r.id, number, p.entity, p.bank, status, r.proposer, uuidOrNull(r.decider), r.updater)
	})
	return r
}

func uuidOrNull(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

// pays adds a payment of the run settling the item, posted or not, its
// item active or not.
func (p *payables) pays(t *testing.T, r run, item uuid.UUID, posted, active bool) {
	t.Helper()
	payment := id()
	var entry any
	if posted {
		entry = id()
	}
	inTenant(t, p.conn, p.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id,
			journal_entry_year, journal_entry_id, created_by) VALUES ($1,$2,$3,$4,$5,400,'USD',$6,CASE WHEN $7::uuid IS NOT NULL THEN 2026 END,$7,$8)`,
			p.tenant, payment, r.id, id(), id(), payment.String()[:35], entry, r.proposer); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date, is_active)
			VALUES ($1,$2,2026,$3,2,400,'2026-07-31',$4)`, p.tenant, payment, item, active)
	})
}

// reserve reserves the item for the run, released or not.
func (p *payables) reserve(t *testing.T, r run, item uuid.UUID, released bool) {
	t.Helper()
	inTenant(t, p.conn, p.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context, owner_reference, created_by,
			released_at, released_by) VALUES ($1,$2,2026,$3,2,'payments',$4,$5,CASE WHEN $6 THEN now() END,CASE WHEN $6 THEN $5::uuid END)`,
			p.tenant, r.id, item, r.number, r.proposer, released)
	})
}

// now is the database's clock, read before a migration runs.
func (p *payables) now(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := p.conn.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// reservation is an item's reservation by an owner after the migration.
type reservation struct {
	reference     string
	createdBy     uuid.UUID
	released      bool
	releasedBy    uuid.UUID
	releasedSince bool // released at or after the time reservation was asked about
}

// reservation reads the owner's reservation of the item; releasedSince
// says whether it was released at since or later.
func (p *payables) reservation(t *testing.T, owner, item uuid.UUID, since time.Time) (reservation, bool) {
	t.Helper()
	var r reservation
	var by *uuid.UUID
	err := p.conn.QueryRow(context.Background(), `SELECT owner_reference, created_by, released_at IS NOT NULL, released_by, coalesce(released_at >= $4, false)
		FROM ledger.item_reservation WHERE tenant_id = $1 AND owner_id = $2 AND journal_entry_id = $3 AND line_no = 2`,
		p.tenant, owner, item, since).Scan(&r.reference, &r.createdBy, &r.released, &by, &r.releasedSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	if err != nil {
		t.Fatal(err)
	}
	if by != nil {
		r.releasedBy = *by
	}
	return r, true
}

// TestReservationsReconciled: 000042 makes the ledger's reservations the
// items of live payment runs not paid yet (F5b, 03-data-model §3.10). A
// reservation no live run holds, left by a release from before 000034, is
// released in the name of whoever last decided or changed its run (its
// creator's, when there is no run); an item a live run pays without a
// reservation is reserved for it.
func TestReservationsReconciled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 42)
	p := newPayables(t, conn)
	held, missing, rejected, posted, unposted, moved := p.item(t), p.item(t), p.item(t), p.item(t), p.item(t), p.item(t)
	proposed := p.run(t, "PR1", "proposed", false)
	p.pays(t, proposed, held, false, true)
	p.reserve(t, proposed, held, false)
	p.pays(t, proposed, missing, false, true)
	dropped := p.run(t, "PR2", "rejected", true)
	p.pays(t, dropped, rejected, false, false)
	p.reserve(t, dropped, rejected, false)
	// moved: the rejected run still holds it, though the live run pays it now.
	p.pays(t, dropped, moved, false, false)
	p.reserve(t, dropped, moved, false)
	p.pays(t, proposed, moved, false, true)
	releasing := p.run(t, "PR3", "releasing", false)
	p.pays(t, releasing, posted, true, true)
	p.reserve(t, releasing, posted, false)
	p.pays(t, releasing, unposted, false, true)
	p.reserve(t, releasing, unposted, false)
	gone, orphan := run{id: id(), proposer: id(), number: "PR4"}, p.item(t) // a run without a row
	p.reserve(t, gone, orphan, false)
	before := p.now(t)
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		owner       run
		item        uuid.UUID
		released    bool
		releasedBy  uuid.UUID
		createdBy   uuid.UUID
		ownerNumber string
	}{
		{"held by a live run", proposed, held, false, uuid.Nil, proposed.proposer, "PR1"},
		{"missing for a live run", proposed, missing, false, uuid.Nil, proposed.proposer, "PR1"},
		{"of a rejected run", dropped, rejected, true, dropped.decider, dropped.proposer, "PR2"},
		{"of a posted payment", releasing, posted, true, releasing.updater, releasing.proposer, "PR3"},
		{"of an unposted payment", releasing, unposted, false, uuid.Nil, releasing.proposer, "PR3"},
		{"of a run that does not exist", gone, orphan, true, gone.proposer, gone.proposer, "PR4"},
		{"moved off a rejected run", dropped, moved, true, dropped.decider, dropped.proposer, "PR2"},
		{"moved onto a live run", proposed, moved, false, uuid.Nil, proposed.proposer, "PR1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := p.reservation(t, c.owner.id, c.item, before)
			if !ok {
				t.Fatalf("reservation: got none, want one")
			}
			if got.released != c.released {
				t.Errorf("released: got %v, want %v", got.released, c.released)
			}
			if got.releasedBy != c.releasedBy {
				t.Errorf("released by: got %s, want %s", got.releasedBy, c.releasedBy)
			}
			if c.released && !got.releasedSince {
				t.Errorf("released by the migration: got false, want true")
			}
			if got.createdBy != c.createdBy {
				t.Errorf("created by: got %s, want %s", got.createdBy, c.createdBy)
			}
			if got.reference != c.ownerNumber {
				t.Errorf("owner reference: got %s, want %s", got.reference, c.ownerNumber)
			}
		})
	}
}

// TestReservationsReconcileFailsLoudly: an item a live run pays whose
// reservation by that run was already released cannot be reserved for it
// again (a reservation is released once, and its key is the owner's and
// the item's), so 000042 stops and says so rather than leave the item
// unreserved. The migration's other changes roll back with it.
func TestReservationsReconcileFailsLoudly(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 42)
	p := newPayables(t, conn)
	item := p.item(t)
	proposed := p.run(t, "PR1", "proposed", false)
	p.pays(t, proposed, item, false, true)
	p.reserve(t, proposed, item, true)
	dropped, stray := p.run(t, "PR2", "rejected", true), p.item(t)
	p.pays(t, dropped, stray, false, false)
	p.reserve(t, dropped, stray, false)
	_, err := Migrate(context.Background(), conn)
	failsWith(t, err, "1 item(s) of live payment runs were released by their run")
	if got, err := CurrentVersion(context.Background(), conn); err != nil || got != 41 {
		t.Errorf("version after the failure: got %d (%v), want 41", got, err)
	}
	got, ok := p.reservation(t, dropped.id, stray, time.Time{})
	if !ok {
		t.Fatalf("stray reservation after the failed migration: got none, want one")
	}
	if got.released {
		t.Errorf("stray reservation released by the failed migration: got true, want false")
	}
}

// TestReservationsReconcileRefusesRowSecurity: run by a role row-level
// security applies to (erp_owner, which owns the tables), 000042 fails
// rather than see no tenant's rows and reconcile nothing.
func TestReservationsReconcileRefusesRowSecurity(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 42)
	var up string
	for _, m := range Migrations() {
		if v, _ := m.Version(); v == 42 { // the names were checked by TestMigrationFiles
			up = m.Up
		}
	}
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE erp_owner"); err != nil {
			return err
		}
		_, err := tx.Conn().PgConn().Exec(ctx, up).ReadAll()
		return err
	})
	failsWith(t, err, "row-level security policy")
}

// TestReservationsReconcileHoldsWriters: 000042 locks the runs, payments
// and reservations against writers before it reads them, so a proposal or
// rejection a running replica commits cannot slip between its statements;
// a writer it cannot wait out within the lock timeout fails it, and the
// next start retries.
func TestReservationsReconcileHoldsWriters(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 42)
	ctx := context.Background()
	replica, err := pgx.ConnectConfig(ctx, conn.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replica.Close(ctx) }() // a close error leaves nothing to undo: the session ends either way
	writing, err := replica.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writing.Exec(ctx, "LOCK TABLE ledger.item_reservation IN ROW EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	_, err = Migrate(ctx, conn)
	failsWith(t, err, "lock timeout")
	if err := writing.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, conn); err != nil {
		t.Errorf("retry once the writer is done: got %v, want nil", err)
	}
}

// TestDeferredChecksValidated: the checks migrations add NOT VALID, so as
// not to scan a table under its lock, are validated by a later one (06
// §6.4), and then hold for every row, not only new ones (T31). A check an
// expand step leaves for a later release to validate is the exception this
// test would name, with the release that validates it.
func TestDeferredChecksValidated(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
		WHERE NOT convalidated AND connamespace::regnamespace::text NOT IN ('pg_catalog', 'information_schema') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) > 0 {
		t.Errorf("constraints not validated: got %v, want none", pending)
	}
}
