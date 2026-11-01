package db

// Schema tests run against a real PostgreSQL. Set DATABASE_URL to an admin
// connection (for example postgres://postgres:dev@localhost:55432/postgres);
// each run creates and drops its own database. Without DATABASE_URL the
// tests are skipped.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	testDB   *pgx.Conn
	adminURL string // admin connection; tests may create further databases
)

func TestMain(m *testing.M) {
	admin := os.Getenv("DATABASE_URL")
	if admin == "" {
		fmt.Println("DATABASE_URL not set; schema tests skipped")
		os.Exit(m.Run())
	}
	adminURL = admin
	ctx := context.Background()
	name := fmt.Sprintf("erp_test_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Println("create database:", err)
		os.Exit(1)
	}
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	testDB, err = pgx.Connect(ctx, u.String())
	if err != nil {
		fmt.Println("connect test database:", err)
		os.Exit(1)
	}
	// As the server migrates: partitioned indexes are built per partition.
	if _, err := migrate(ctx, testDB, Migrations()); err != nil {
		fmt.Println("migrate:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = testDB.Close(ctx) // the drop below forces any session off
	if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		fmt.Println("drop test database:", err)
	}
	_ = conn.Close(ctx) // the process exits next
	os.Exit(code)
}

func applyAll(ctx context.Context, c *pgx.Conn, up bool) error {
	ms := Migrations()
	if !up {
		for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
			ms[i], ms[j] = ms[j], ms[i]
		}
	}
	for _, m := range ms {
		sql := m.Up
		if !up {
			sql = m.Down
		}
		if _, err := c.PgConn().Exec(ctx, sql).ReadAll(); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	return nil
}

func need(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("DATABASE_URL not set")
	}
}

// asApp runs fn in a transaction as the application role for a tenant, the
// way the service does: row-level security and grants apply.
func asApp(t *testing.T, tenant uuid.UUID, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE erp_app"); err != nil {
		t.Fatal(err)
	}
	if tenant != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
			t.Fatal(err)
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func mustApp(t *testing.T, tenant uuid.UUID, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := asApp(t, tenant, fn); err != nil {
		t.Fatal(err)
	}
}

// failsWith asserts err is a PostgreSQL error whose message contains want.
func failsWith(t *testing.T, err error, want string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("want database error containing %q, got %v", want, err)
	}
	if !strings.Contains(pg.Message, want) {
		t.Fatalf("want error containing %q, got %q", want, pg.Message)
	}
}

func id() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func exec(tx pgx.Tx, sql string, args ...any) error {
	_, err := tx.Exec(context.Background(), sql, args...)
	return err
}

// fixture is one tenant with a legal entity, ledger, open 2026 periods,
// accounts and a supplier.
type fixture struct {
	tenant, actor, entity, ledger, calendar, chart  uuid.UUID
	bank, ap, expense, tax, supplier, otherSupplier uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixtureIDs()
	mustApp(t, f.tenant, f.seed)
	return f
}

func fixtureIDs() fixture {
	return fixture{tenant: id(), actor: id(), entity: id(), ledger: id(), calendar: id(), chart: id(),
		bank: id(), ap: id(), expense: id(), tax: id(), supplier: id(), otherSupplier: id()}
}

// seed writes the fixture's rows in tx, the tenant's scope already set.
func (f fixture) seed(tx pgx.Tx) error {
	steps := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Acme', 'eu-west', 'EUR', 'active')", []any{f.tenant}},
		{"INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by) VALUES ($1,$2,'1000','Acme US','US','USD',$3,'active',$4,$4)", []any{f.tenant, f.entity, f.calendar, f.actor}},
		{"INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1,$2,'0L','Leading',true,'IFRS',$3)", []any{f.tenant, f.ledger, f.actor}},
		{"INSERT INTO ledger.fiscal_calendar (tenant_id, id, code, name, normal_periods, special_periods, created_by) VALUES ($1,$2,'K4','Calendar year',12,4,$3)", []any{f.tenant, f.calendar, f.actor}},
		{"INSERT INTO ledger.chart_of_accounts (tenant_id, id, code, name, created_by) VALUES ($1,$2,'INT','International',$3)", []any{f.tenant, f.chart, f.actor}},
	}
	for _, s := range steps {
		if err := exec(tx, s.sql, s.args...); err != nil {
			return fmt.Errorf("%s: %w", s.sql[:40], err)
		}
	}
	for m := 1; m <= 12; m++ {
		start := time.Date(2026, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, -1)
		if err := exec(tx, "INSERT INTO ledger.fiscal_period VALUES ($1,$2,2026,$3,$4,$5,false)", f.tenant, f.calendar, m, start, end); err != nil {
			return err
		}
		status := "open"
		if m == 1 {
			status = "closed"
		}
		if err := exec(tx, "INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by) VALUES ($1,gen_random_uuid(),$2,$3,$4,2026,$5,$6,$7)",
			f.tenant, f.entity, f.ledger, f.calendar, m, status, f.actor); err != nil {
			return err
		}
	}
	for _, a := range []struct {
		id           uuid.UUID
		num, typ, rk string
	}{{f.bank, "100000", "asset", "none"}, {f.ap, "211000", "liability", "supplier"}, {f.expense, "520000", "expense", "none"}, {f.tax, "140000", "asset", "none"}} {
		if err := exec(tx, "INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by) VALUES ($1,$2,$3,$4,$4,$5,$6,'active',$7,$7)",
			f.tenant, a.id, f.chart, a.num, a.typ, a.rk, f.actor); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)", f.tenant, f.entity, a.id, f.actor); err != nil {
			return err
		}
	}
	if err := exec(tx, "INSERT INTO partner.payment_term (tenant_id, code, description, net_days, created_by, updated_by) VALUES ($1,'NT30','Net 30',30,$2,$2)", f.tenant, f.actor); err != nil {
		return err
	}
	for i, p := range []uuid.UUID{f.supplier, f.otherSupplier} {
		if err := exec(tx, "INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by) VALUES ($1,$2,$3,'organization',$4,'US','active',$5,$5)",
			f.tenant, p, fmt.Sprintf("S%04d", i+1), fmt.Sprintf("Supplier %d", i+1), f.actor); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1,$2,'supplier',$3)", f.tenant, p, f.actor); err != nil {
			return err
		}
	}
	return nil
}

// entry posts a journal entry with the given (account, amount) lines in USD.
func (f fixture) entry(tx pgx.Tx, period int, date string, lines ...line) (uuid.UUID, error) {
	return f.entryFor(tx, nil, period, date, lines...)
}

// entryFor posts a manual journal entry as entry does, naming source as its
// source document (none when nil).
func (f fixture) entryFor(tx pgx.Tx, source *uuid.UUID, period int, date string, lines ...line) (uuid.UUID, error) {
	je := id()
	if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context,
		source_document_id, posted_by)
		VALUES ($1,2026,$2,$3,$4,$5,'SA',$6,$7,$7,'USD','USD','EUR',1,0.9,'ledger',$8,$9)`,
		f.tenant, je, f.entity, f.ledger, period, je.String()[24:], date, source, f.actor); err != nil {
		return je, err
	}
	for i, l := range lines {
		if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, partner_id) VALUES ($1,2026,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11)`,
			f.tenant, je, i+1, f.entity, f.ledger, period, date, l.account, l.amount, l.amount*9/10, l.partner); err != nil {
			return je, err
		}
	}
	return je, nil
}

type line struct {
	account uuid.UUID
	amount  int64
	partner *uuid.UUID
}

// --- Structural rules -------------------------------------------------------

// Every design rule in 03-data-model §3.1 that can be checked from the catalog.
func TestDesignRules(t *testing.T) {
	need(t)
	ctx := context.Background()
	query := func(sql string) []string {
		rows, err := testDB.Query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tenantSchemas := `('platform','tenancy','identity','config','partner','ledger','procurement','payables','payments','workflow','audit','integration','agents','analytics','cash','controlling','tax')`

	if bad := query(`SELECT c.oid::regclass::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ` + tenantSchemas + ` AND c.relkind IN ('r','p') AND NOT c.relispartition AND NOT c.relrowsecurity`); len(bad) > 0 {
		t.Errorf("tables without row-level security: %v", bad)
	}
	if bad := query(`SELECT c.oid::regclass::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ` + tenantSchemas + ` AND c.relkind IN ('r','p') AND NOT c.relispartition
		AND c.oid <> 'tenancy.tenant'::regclass AND c.oid <> 'audit.chain_head'::regclass
		AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary
		                AND (SELECT attname FROM pg_attribute WHERE attrelid = c.oid AND attnum = i.indkey[0]) = 'tenant_id')`); len(bad) > 0 {
		t.Errorf("primary keys not led by tenant_id: %v", bad)
	}
	if bad := query(`SELECT conrelid::regclass::text || ' -> ' || confrelid::regclass::text FROM pg_constraint c
		JOIN pg_class s ON s.oid = c.conrelid JOIN pg_namespace sn ON sn.oid = s.relnamespace
		JOIN pg_class d ON d.oid = c.confrelid JOIN pg_namespace dn ON dn.oid = d.relnamespace
		WHERE c.contype = 'f' AND sn.nspname <> dn.nspname AND dn.nspname <> 'ref' AND NOT s.relispartition
		AND NOT (sn.nspname = 'tenancy' AND dn.nspname = 'tenancy')`); len(bad) > 0 {
		t.Errorf("foreign keys crossing bounded contexts: %v", bad)
	}
	if bad := query(`SELECT conrelid::regclass::text FROM pg_constraint c JOIN pg_class s ON s.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = s.relnamespace
		WHERE c.contype = 'f' AND n.nspname IN ` + tenantSchemas + ` AND NOT s.relispartition
		AND c.confrelid::regclass::text NOT LIKE 'ref.%' AND c.confrelid <> 'tenancy.tenant'::regclass
		AND (SELECT attname FROM pg_attribute WHERE attrelid = c.conrelid AND attnum = c.conkey[1]) <> 'tenant_id'`); len(bad) > 0 {
		t.Errorf("foreign keys that do not start with tenant_id: %v", bad)
	}
	if bad := query(`SELECT table_schema || '.' || table_name || '.' || column_name FROM information_schema.columns
		WHERE table_schema IN ` + tenantSchemas + ` AND data_type IN ('real', 'double precision', 'money')`); len(bad) > 0 {
		t.Errorf("floating-point or money columns: %v", bad)
	}
	if bad := query(`SELECT c.oid::regclass::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ` + tenantSchemas + ` AND c.relkind IN ('r','p') AND NOT c.relispartition
		AND has_table_privilege('erp_app', c.oid, 'DELETE')`); len(bad) > 0 {
		t.Errorf("the application may delete from: %v", bad)
	}
	if bad := query(`SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner
		WHERE p.prosecdef AND (r.rolsuper OR r.rolbypassrls)`); len(bad) > 0 {
		t.Errorf("security-definer functions that would bypass row-level security: %v", bad)
	}
	if bad := query(`SELECT p.oid::regprocedure::text FROM pg_proc p
		WHERE p.prosecdef AND NOT coalesce(array_to_string(p.proconfig, ',') LIKE '%search_path=%', false)`); len(bad) > 0 {
		t.Errorf("security-definer functions without a pinned search_path: %v", bad)
	}
	insertOnly := query(`SELECT DISTINCT t.tgrelid::regclass::text FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		JOIN pg_class c ON c.oid = t.tgrelid WHERE p.proname = 'forbid_mutation' AND NOT c.relispartition`)
	if len(insertOnly) < 15 {
		t.Errorf("expected the insert-only tables to be protected, found %v", insertOnly)
	}
	for _, tbl := range insertOnly {
		var can bool
		if err := testDB.QueryRow(ctx, "SELECT has_table_privilege('erp_app', $1, 'UPDATE')", tbl).Scan(&can); err != nil {
			t.Fatalf("privilege on %s: %v", tbl, err)
		}
		if can {
			t.Errorf("erp_app UPDATE privilege on insert-only table %s: got true, want false", tbl)
		}
	}
}

// TestMigrationsRevertCleanly runs every migration up, down and up again in a
// database of its own: the shared one holds rows other tests inserted, which
// the constraints a down migration restores may rightly refuse.
func TestMigrationsRevertCleanly(t *testing.T) {
	ctx := context.Background()
	conn := emptyDatabase(t)
	if err := applyAll(ctx, conn, true); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := applyAll(ctx, conn, false); err != nil {
		t.Fatalf("down: %v", err)
	}
	var schemas int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_namespace WHERE nspname IN ('ledger','partner','payables')").Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas != 0 {
		t.Fatalf("down migrations left %d schemas behind, want 0", schemas)
	}
	if err := applyAll(ctx, conn, true); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// --- Tenant isolation -------------------------------------------------------

func TestTenantIsolation(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	var seen int
	mustApp(t, b.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM partner.business_partner WHERE id = $1", a.supplier).Scan(&seen)
	})
	if seen != 0 {
		t.Fatal("tenant B can see tenant A's partner")
	}
	err := asApp(t, b.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by) VALUES ($1,$2,'X1','organization','X','US','active',$3,$3)", a.tenant, id(), b.actor)
	})
	failsWith(t, err, "row-level security")
	// Without a tenant, nothing is visible.
	mustApp(t, uuid.Nil, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM partner.business_partner").Scan(&seen)
	})
	if seen != 0 {
		t.Fatal("rows visible without a tenant")
	}
	// A row can never reference another tenant's row: foreign keys include tenant_id.
	err = asApp(t, b.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by) VALUES ($1,$2,$3,$4,'NT30',$5,$5)",
			b.tenant, a.supplier, b.entity, b.ap, b.actor)
	})
	failsWith(t, err, "foreign key")
}

// --- Ledger -----------------------------------------------------------------

func TestJournalEntriesMustBalance(t *testing.T) {
	need(t)
	f := newFixture(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		_, err := f.entry(tx, 3, "2026-03-15", line{account: f.expense, amount: 10_000}, line{account: f.bank, amount: -10_000})
		return err
	})
	err := asApp(t, f.tenant, func(tx pgx.Tx) error {
		_, err := f.entry(tx, 3, "2026-03-15", line{account: f.expense, amount: 10_000}, line{account: f.bank, amount: -9_999})
		return err
	})
	failsWith(t, err, "unbalanced")
	err = asApp(t, f.tenant, func(tx pgx.Tx) error {
		_, err := f.entry(tx, 3, "2026-03-15", line{account: f.expense, amount: 0})
		return err
	})
	failsWith(t, err, "at least two")
}

func TestJournalIsInsertOnly(t *testing.T) {
	need(t)
	f := newFixture(t)
	var je uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		je, err = f.entry(tx, 4, "2026-04-02", line{account: f.expense, amount: 500}, line{account: f.bank, amount: -500})
		return err
	})
	err := asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE ledger.journal_line SET amount_tc = 1 WHERE journal_entry_id = $1", je)
	})
	failsWith(t, err, "permission denied")
	err = asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "DELETE FROM ledger.journal_entry WHERE id = $1", je)
	})
	failsWith(t, err, "permission denied")
	// Even the owner cannot change posted lines.
	_, err = testDB.Exec(context.Background(), "UPDATE ledger.journal_line SET amount_tc = 1 WHERE journal_entry_id = $1", je)
	failsWith(t, err, "insert-only")
}

func TestPostingPeriods(t *testing.T) {
	need(t)
	f := newFixture(t)
	err := asApp(t, f.tenant, func(tx pgx.Tx) error {
		_, err := f.entry(tx, 1, "2026-01-15", line{account: f.expense, amount: 1}, line{account: f.bank, amount: -1})
		return err
	})
	failsWith(t, err, "is closed")
	err = asApp(t, f.tenant, func(tx pgx.Tx) error {
		_, err := f.entry(tx, 3, "2026-04-15", line{account: f.expense, amount: 1}, line{account: f.bank, amount: -1})
		return err
	})
	failsWith(t, err, "outside period")
}

// TestPeriodTransitions: a period moves open <-> closed and closed ->
// locked, and no other way (000050, T32): a locked period never reopens
// by an update, and nothing locks a period still open for postings.
func TestPeriodTransitions(t *testing.T) {
	need(t)
	set := func(tenant uuid.UUID, status string) error {
		_, err := testDB.Exec(context.Background(), `UPDATE ledger.period_status SET status = $2, version = version + 1
			WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 5`, tenant, status)
		return err
	}
	// Each case has a tenant of its own and puts period 5 (open) in its
	// starting status by allowed steps, so no case depends on another.
	path := map[string][]string{"open": {"closed", "open"}, "closed": {"open", "closed"}, "locked": {"open", "closed", "locked"}}
	for _, c := range []struct {
		from, to string
		want     string // empty: allowed
	}{
		{"open", "locked", "cannot go from open to locked"},
		{"open", "open", ""},
		{"open", "closed", ""},
		{"closed", "open", ""},
		{"closed", "locked", ""},
		{"locked", "open", "cannot go from locked to open"},
		{"locked", "closed", "cannot go from locked to closed"},
		{"locked", "locked", ""},
	} {
		t.Run(c.from+" to "+c.to, func(t *testing.T) {
			f := newFixture(t)
			for _, step := range path[c.from] {
				if err := set(f.tenant, step); err != nil {
					t.Fatalf("set up %s: %v", c.from, err)
				}
			}
			err := set(f.tenant, c.to)
			if c.want == "" {
				if err != nil {
					t.Errorf("got %v, want no error", err)
				}
				return
			}
			failsWith(t, err, c.want)
		})
	}
}

// TestPeriodUnlock: a locked period returns to closed only by consuming the
// unlock request it names, which it clears in the same update (000051,
// T32); and only a locked period names one. That the request was approved
// is checked by the ledger's approvals callback (T32c): the database rule
// stays within the ledger's schema (02-architecture, boundary rule 1).
func TestPeriodUnlock(t *testing.T) {
	need(t)
	ctx := context.Background()
	// locked puts period 5 of a new tenant in locked, naming an unlock
	// request when named is set, and returns the tenant.
	locked := func(t *testing.T, named bool) uuid.UUID {
		t.Helper()
		f := newFixture(t)
		steps := []string{"status = 'closed'", "status = 'locked'"}
		if named {
			steps = append(steps, "unlock_request_id = gen_random_uuid()")
		}
		for _, set := range steps {
			if _, err := testDB.Exec(ctx, `UPDATE ledger.period_status SET `+set+`, version = version + 1
				WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 5`, f.tenant); err != nil {
				t.Fatal(err)
			}
		}
		return f.tenant
	}
	for _, c := range []struct {
		name  string
		named bool
		set   string
		want  string // empty: allowed
	}{
		{"consuming the request", true, "unlock_request_id = NULL", ""},
		{"naming none", false, "unlock_request_id = NULL", "cannot go from locked to closed"},
		{"keeping the request", true, "unlock_request_id = unlock_request_id", "cannot go from locked to closed"},
		{"naming another", true, "unlock_request_id = gen_random_uuid()", "cannot go from locked to closed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant := locked(t, c.named)
			var closed int64
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, `UPDATE ledger.period_status SET status = 'closed', `+c.set+`, version = version + 1
					WHERE fiscal_year = 2026 AND period = 5`)
				closed = tag.RowsAffected()
				return err
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if closed != 1 {
				t.Errorf("periods closed: got %d, want 1", closed)
			}
		})
	}
	t.Run("naming one while open", func(t *testing.T) {
		f := newFixture(t)
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.period_status SET unlock_request_id = $1, version = version + 1 WHERE fiscal_year = 2026 AND period = 5`, id())
		})
		failsWith(t, err, "period_status_unlock_only_locked")
	})
}

// TestPostingHoldsItsPeriod: inserting an entry holds its period's status
// row FOR SHARE until the transaction ends (F8b, 000043), as the ledger's
// postingPeriod does, so a close, which locks the row FOR UPDATE, waits for
// any posting under way rather than commit beneath it.
func TestPostingHoldsItsPeriod(t *testing.T) {
	need(t)
	f := newFixture(t)
	ctx := context.Background()
	closer, err := pgx.ConnectConfig(ctx, testDB.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close(ctx) }() // a close error leaves nothing to undo: the session ends either way
	err = asApp(t, f.tenant, func(tx pgx.Tx) error {
		if _, err := f.entry(tx, 7, "2026-07-02", line{account: f.expense, amount: 1}, line{account: f.bank, amount: -1}); err != nil {
			return fmt.Errorf("post: %w", err)
		}
		_, err := closer.Exec(ctx, `SELECT FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 7
			FOR UPDATE NOWAIT`, f.tenant, f.entity)
		return err
	})
	failsWith(t, err, "could not obtain lock on row in relation \"period_status\"")
}

// TestClearingsRespectPeriods: a clearing is dated in a fiscal period of
// its entry's legal entity and ledger, and that period is open (000049,
// 03-data-model §3.10), as an entry's posting date is. A clearing posts no
// entry when a migration brings it over, so the rule is on the clearing.
func TestClearingsRespectPeriods(t *testing.T) {
	need(t)
	f := newFixture(t)
	var pay uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		pay, err = f.entry(tx, 6, "2026-06-20", line{account: f.ap, amount: 700, partner: &f.supplier}, line{account: f.bank, amount: -700})
		return err
	})
	// Period 1 starts closed: lock it. Close period 2.
	for period, status := range map[int]string{1: "locked", 2: "closed"} {
		if _, err := testDB.Exec(context.Background(), `UPDATE ledger.period_status SET status = $3, version = version + 1
			WHERE tenant_id = $1 AND period = $2`, f.tenant, period, status); err != nil {
			t.Fatal(err)
		}
	}
	clear := func(day string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by) VALUES ($1,$2,$3,$4,2026,$5,$6)",
				f.tenant, id(), f.entity, day, pay, f.actor)
		})
	}
	for name, c := range map[string]struct{ day, want string }{
		"locked period":    {"2026-01-20", "is locked"},
		"closed period":    {"2026-02-20", "is closed"},
		"no fiscal period": {"2027-01-05", "no fiscal period"},
	} {
		t.Run(name, func(t *testing.T) { failsWith(t, clear(c.day), c.want) })
	}
	if err := clear("2026-07-01"); err != nil {
		t.Errorf("clearing in an open period: got %v, want none", err)
	}
	// The clearing entry exists and is of the clearing's legal entity.
	other := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by) VALUES ($1,$2,'2000','Acme DE','DE','EUR',$3,'active',$4,$4)",
			f.tenant, other, f.calendar, f.actor)
	})
	for name, c := range map[string]struct {
		entity, entry  uuid.UUID
		want, sqlState string
	}{
		"entry of another legal entity": {other, pay, "belongs to another legal entity", "23514"},
		"entry that does not exist":     {f.entity, id(), "does not exist", "23503"},
	} {
		t.Run(name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by) VALUES ($1,$2,$3,'2026-07-01',2026,$4,$5)",
					f.tenant, id(), c.entity, c.entry, f.actor)
			})
			failsWith(t, err, c.want)
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code != c.sqlState {
				t.Errorf("SQLSTATE: got %s, want %s", pg.Code, c.sqlState)
			}
		})
	}
}

// TestClearingHoldsItsPeriod: inserting a clearing holds its period's
// status row FOR SHARE until the transaction ends, as posting does, so a
// close waits for it.
func TestClearingHoldsItsPeriod(t *testing.T) {
	need(t)
	f := newFixture(t)
	ctx := context.Background()
	var pay uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		pay, err = f.entry(tx, 6, "2026-06-20", line{account: f.ap, amount: 700, partner: &f.supplier}, line{account: f.bank, amount: -700})
		return err
	})
	closer, err := pgx.ConnectConfig(ctx, testDB.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close(ctx) }() // a close error leaves nothing to undo: the session ends either way
	err = asApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, "INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by) VALUES ($1,$2,$3,'2026-08-03',2026,$4,$5)",
			f.tenant, id(), f.entity, pay, f.actor); err != nil {
			return fmt.Errorf("clear: %w", err)
		}
		_, err := closer.Exec(ctx, `SELECT FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 8
			FOR UPDATE NOWAIT`, f.tenant, f.entity)
		return err
	})
	failsWith(t, err, "could not obtain lock on row in relation \"period_status\"")
}

func TestReconciliationAccountsNeedPartner(t *testing.T) {
	need(t)
	f := newFixture(t)
	err := asApp(t, f.tenant, func(tx pgx.Tx) error {
		_, err := f.entry(tx, 5, "2026-05-10", line{account: f.expense, amount: 100}, line{account: f.ap, amount: -100})
		return err
	})
	failsWith(t, err, "without a partner")
}

func TestReversalAndClearingHappenOnce(t *testing.T) {
	need(t)
	f := newFixture(t)
	var inv, pay uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		if inv, err = f.entry(tx, 6, "2026-06-01", line{account: f.expense, amount: 700}, line{account: f.ap, amount: -700, partner: &f.supplier}); err != nil {
			return err
		}
		pay, err = f.entry(tx, 6, "2026-06-20", line{account: f.ap, amount: 700, partner: &f.supplier}, line{account: f.bank, amount: -700})
		return err
	})
	var open int
	count := func() {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), "SELECT count(*) FROM ledger.open_item WHERE partner_id = $1", f.supplier).Scan(&open)
		})
	}
	count()
	if open != 2 {
		t.Fatalf("want 2 open items, got %d", open)
	}
	clr := id()
	clear := func(tx pgx.Tx, c uuid.UUID) error {
		if err := exec(tx, "INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by) VALUES ($1,$2,$3,'2026-06-20',2026,$4,$5)",
			f.tenant, c, f.entity, pay, f.actor); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO ledger.clearing_item VALUES ($1,2026,$2,2,$3)", f.tenant, inv, c); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO ledger.clearing_item VALUES ($1,2026,$2,1,$3)", f.tenant, pay, c)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return clear(tx, clr) })
	count()
	if open != 0 {
		t.Fatalf("cleared items are not open: %d", open)
	}
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return clear(tx, id()) }), "duplicate key")

	rev := func(tx pgx.Tx) error {
		r, err := f.entry(tx, 6, "2026-06-25", line{account: f.expense, amount: -700}, line{account: f.ap, amount: 700, partner: &f.supplier})
		if err != nil {
			return err
		}
		return exec(tx, "INSERT INTO ledger.reversal VALUES ($1,2026,$2,2026,$3)", f.tenant, inv, r)
	}
	mustApp(t, f.tenant, rev)
	failsWith(t, asApp(t, f.tenant, rev), "duplicate key")
}

// --- Documents ----------------------------------------------------------------

func TestDuplicateInvoicesAreImpossible(t *testing.T) {
	need(t)
	f := newFixture(t)
	ins := func(ref string, sha []byte, source string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
				invoice_date, currency, gross_amount, tax_amount, status, source, capture_sha256, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,'2026-06-01','USD',1080,80,'parked',$7,$8,$9,$9)`,
				f.tenant, id(), ref+"-"+source, f.entity, f.supplier, ref, source, sha, f.actor)
		}
	}
	mustApp(t, f.tenant, ins("INV-2026/001", nil, "manual"))
	failsWith(t, asApp(t, f.tenant, ins("inv 2026 001", nil, "edi")), "supplier_invoice_no_duplicates")
	sha := make([]byte, 32)
	mustApp(t, f.tenant, ins("INV-2", sha, "capture"))
	failsWith(t, asApp(t, f.tenant, ins("INV-3", sha, "capture")), "supplier_invoice_capture_once")
}

func TestPaymentItemsAreReservedOnce(t *testing.T) {
	need(t)
	f := newFixture(t)
	var inv uuid.UUID
	hb, run1, run2 := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		if inv, err = f.entry(tx, 7, "2026-07-01", line{account: f.expense, amount: 300}, line{account: f.ap, amount: -300, partner: &f.supplier}); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		for i, r := range []uuid.UUID{run1, run2} {
			if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,'2026-07-31','2026-07-31','USD',300,'proposed',$6,$6,$6)`, f.tenant, r, fmt.Sprintf("PR%d", i), f.entity, hb, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	addItem := func(run uuid.UUID, e2e string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			p := id()
			if err := exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by)
				VALUES ($1,$2,$3,$4,$5,300,'USD',$6,$7)`, f.tenant, p, run, f.supplier, id(), e2e, f.actor); err != nil {
				return err
			}
			return exec(tx, "INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date) VALUES ($1,$2,2026,$3,2,300,'2026-07-31')", f.tenant, p, inv)
		}
	}
	mustApp(t, f.tenant, addItem(run1, "E2E-1"))
	failsWith(t, asApp(t, f.tenant, addItem(run2, "E2E-2")), "payment_item_reserved_once")
	// Rejecting run 1 releases the item; reactivation is refused.
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE payments.payment_item SET is_active = false WHERE tenant_id = $1 AND item_entry_id = $2", f.tenant, inv)
	})
	mustApp(t, f.tenant, addItem(run2, "E2E-3"))
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE payments.payment_item SET is_active = true WHERE tenant_id = $1 AND item_entry_id = $2 AND NOT is_active", f.tenant, inv)
	}), "only be deactivated")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE payments.payment_item SET amount = 1 WHERE tenant_id = $1", f.tenant)
	}), "permission denied")
	// Whoever proposed a run can never release it, and a release records who decided.
	release := func(by any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE payments.payment_run SET status = 'released', decided_by = $3, decided_at = now(), version = version + 1
				WHERE tenant_id = $1 AND id = $2`, f.tenant, run2, by)
		})
	}
	failsWith(t, release(f.actor), "payment_run_sod")
	failsWith(t, release(nil), "payment_run_check")
	releaser := id()
	if err := release(releaser); err != nil {
		t.Fatal(err)
	}
	// A large run's release is approved by a second person, neither its
	// proposer nor its releaser, and the run records the request whole
	// (ADR-0038, 000153).
	approval := func(state any, total any, by any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE payments.payment_run SET release_request_id = $3, release_approval = $4, release_request_total = $5,
				release_approved_by = $6, release_approved_at = CASE WHEN $6::uuid IS NULL THEN NULL ELSE now() END, version = version + 1
				WHERE tenant_id = $1 AND id = $2`, f.tenant, run2, id(), state, total, by)
		})
	}
	failsWith(t, approval("approved", 300, f.actor), "payment_run_release_sod")
	failsWith(t, approval("approved", 300, releaser), "payment_run_release_sod")
	failsWith(t, approval("approved", 300, nil), "payment_run_release_approved")
	failsWith(t, approval("pending", 300, id()), "payment_run_release_approved")
	failsWith(t, approval("pending", nil, nil), "payment_run_release_request")
	failsWith(t, approval("maybe", 300, nil), "payment_run_release_approval_check")
	if err := approval("approved", 300, id()); err != nil {
		t.Fatal(err)
	}
	// A credit memo is settled at a negative amount, never at zero.
	item := func(amount int) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
				SELECT tenant_id, id, 2026, $2, 3, $3, '2026-07-31' FROM payments.payment WHERE tenant_id = $1 AND end_to_end_id = 'E2E-3'`, f.tenant, id(), amount)
		}
	}
	mustApp(t, f.tenant, item(-50))
	failsWith(t, asApp(t, f.tenant, item(0)), "payment_item_amount_check")
	// A payment records its journal entry once.
	post := func(tx pgx.Tx) error {
		return exec(tx, `UPDATE payments.payment SET journal_entry_year = 2026, journal_entry_id = $2 WHERE tenant_id = $1 AND end_to_end_id = 'E2E-3'`, f.tenant, id())
	}
	mustApp(t, f.tenant, post)
	failsWith(t, asApp(t, f.tenant, post), "already posted")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE payments.payment SET amount = 1 WHERE tenant_id = $1`, f.tenant)
	}), "permission denied")
}

// TestBankFileOncePerBankAndMethod: a release writes one bank file per
// house bank and payment method (T45c); a second file for the same, from a
// release finished twice, is refused, and a run proposed for one bank,
// whose file names neither, has one file.
func TestBankFileOncePerBankAndMethod(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run, other := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		for i, r := range []uuid.UUID{run, other} {
			if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,'2026-07-31','2026-07-31','USD',300,'releasing',$6,$6,$6)`, f.tenant, r, fmt.Sprintf("PR%d", i), f.entity, hb, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	file := func(r uuid.UUID, bank *uuid.UUID, method *string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_by, house_bank_id, payment_method)
				VALUES ($1,$2,$3,'pain.001.001.03','sha256:x',decode(repeat('ab', 32), 'hex'),'created',$4,$5,$6)`, f.tenant, id(), r, f.actor, bank, method)
		}
	}
	transfer, cheque := "T", "C"
	mustApp(t, f.tenant, file(run, nil, nil))
	failsWith(t, asApp(t, f.tenant, file(run, nil, nil)), "bank_file_once_per_bank")
	mustApp(t, f.tenant, file(other, &hb, &transfer))
	failsWith(t, asApp(t, f.tenant, file(other, &hb, &transfer)), "bank_file_once_per_bank")
	mustApp(t, f.tenant, file(other, &hb, &cheque))
	failsWith(t, asApp(t, f.tenant, file(other, nil, &cheque)), "bank_file_method_of_bank")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_by, house_bank_id, payment_method, message_id)
			VALUES ($1,$2,$3,'pain.001.001.03','sha256:x',decode(repeat('cd', 32), 'hex'),'created',$4,$5,'X','')`, f.tenant, id(), other, f.actor, hb)
	}), "bank_file_message_id_check")
}

// TestPaymentsNameTheirBank: a run may have no house bank (T45c); a
// payment's method needs the bank that pays it, which must be a house
// bank; a supplier no ranked bank can pay is excluded with no_house_bank.
func TestPaymentsNameTheirBank(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,'2026-07-31','2026-07-31','USD',300,'proposed',$4,$4,$4)`, f.tenant, run, f.entity, f.actor)
	})
	pay := func(e2e string, bank *uuid.UUID, method string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by, house_bank_id, payment_method)
				VALUES ($1,$2,$3,$4,$5,300,'USD',$6,$7,$8,$9)`, f.tenant, id(), run, id(), id(), e2e, f.actor, bank, method)
		}
	}
	unknown := id()
	mustApp(t, f.tenant, pay("E2E-1", &hb, "T"))
	failsWith(t, asApp(t, f.tenant, pay("E2E-2", nil, "T")), "payment_method_of_bank")
	failsWith(t, asApp(t, f.tenant, pay("E2E-3", &hb, "TT")), "payment_method_of_bank")
	failsWith(t, asApp(t, f.tenant, pay("E2E-4", &unknown, "T")), "payment_house_bank")
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.payment_exclusion (tenant_id, payment_run_id, supplier_id, reason_code, amount) VALUES ($1,$2,$3,'no_house_bank',300)`,
			f.tenant, run, id())
	})
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.request_exclusion (tenant_id, payment_run_id, down_payment_request_id, supplier_id, reason_code, amount)
			VALUES ($1,$2,$3,$4,'no_house_bank',300)`, f.tenant, run, id(), id())
	})
}

// TestHouseBankAccountOnce: at most one active house bank of a legal entity
// per ledger account (ADR-0038), checked at commit, so two banks may swap
// accounts in one transaction; active_gl_account_id is the account while
// the bank is active and null otherwise.
func TestHouseBankAccountOnce(t *testing.T) {
	need(t)
	f := newFixture(t)
	a, b := id(), id()
	bank := func(hb uuid.UUID, code string, account uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$4,'\x00','5555','BUKBGB22','USD',$5,$5,'active',$6,$6)`, f.tenant, hb, f.entity, code, account, f.actor)
		}
	}
	mustApp(t, f.tenant, bank(a, "A", f.bank))
	failsWith(t, asApp(t, f.tenant, bank(b, "B", f.bank)), "house_bank_account_once")
	mustApp(t, f.tenant, bank(b, "B", f.expense))
	move := func(tx pgx.Tx, hb, account uuid.UUID) error {
		return exec(tx, `UPDATE payments.house_bank SET gl_account_id = $2, active_gl_account_id = $2, version = version + 1 WHERE id = $1`, hb, account)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := move(tx, a, f.expense); err != nil {
			return err
		}
		return move(tx, b, f.bank)
	})
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return move(tx, a, f.bank) }), "house_bank_account_once")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE payments.house_bank SET status = 'inactive', version = version + 1 WHERE id = $1`, b)
	}), "house_bank_active_account")
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE payments.house_bank SET status = 'inactive', active_gl_account_id = NULL, version = version + 1 WHERE id = $1`, b)
	})
	mustApp(t, f.tenant, bank(id(), "C", f.bank))
}

func TestBankAccountValidityCannotOverlap(t *testing.T) {
	need(t)
	f := newFixture(t)
	add := func(from string, to *string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme,
				bank_country_code, holder_name, valid_from, valid_to, partner_change_id, created_by)
				VALUES ($1,$2,$3,'\x01',$4,'4321','iban','DE','Supplier 1',$5,$6,$7,$8)`,
				f.tenant, id(), f.supplier, strings.Repeat("a", 64), from, to, id(), f.actor)
		}
	}
	end := "2026-07-01"
	mustApp(t, f.tenant, add("2026-01-01", &end))
	mustApp(t, f.tenant, add("2026-07-01", nil))
	failsWith(t, asApp(t, f.tenant, add("2026-06-15", nil)), "conflicting key value violates exclusion constraint")
}

// TestCanadianBankAccounts: a partner account of scheme ca has an
// institution number of 3 digits and a transit number of 5, and accounts
// of other schemes have neither; a house bank has both or neither (T45d).
func TestCanadianBankAccounts(t *testing.T) {
	need(t)
	f := newFixture(t)
	from := 2000
	add := func(scheme string, institution, transit *string) func(tx pgx.Tx) error {
		from++ // each account its own validity, so none overlaps another
		valid := fmt.Sprintf("%d-01-01", from)
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme,
				bank_country_code, holder_name, valid_from, valid_to, partner_change_id, created_by, institution_number, transit_number)
				VALUES ($1,$2,$3,'\x01',$4,'4567',$5,'CA','Supplier 1',$6,($6::date + 1),$7,$8,$9,$10)`,
				f.tenant, id(), f.supplier, strings.Repeat("a", 64), scheme, valid, id(), f.actor, institution, transit)
		}
	}
	s := func(v string) *string { return &v }
	mustApp(t, f.tenant, add("ca", s("003"), s("12345")))
	mustApp(t, f.tenant, add("iban", nil, nil))
	failsWith(t, asApp(t, f.tenant, add("swift", nil, nil)), "bank_account_account_scheme_check")
	for name, c := range map[string]struct {
		scheme               string
		institution, transit *string
	}{
		"ca without an institution":   {"ca", nil, s("12345")},
		"ca without a transit":        {"ca", s("003"), nil},
		"ca with a short institution": {"ca", s("03"), s("12345")},
		"ca with a letter in transit": {"ca", s("003"), s("1234A")},
		"iban with an institution":    {"iban", s("003"), nil},
		"domestic with a transit":     {"domestic", nil, s("12345")},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, add(c.scheme, c.institution, c.transit)), "bank_account_ca_routing")
		})
	}
	bank := func(code string, institution, transit *string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id,
				status, created_by, updated_by, institution_number, transit_number)
				VALUES ($1,$2,$3,$4,$4,'\x00','0001','ROYCCAT2','CAD',$5,'inactive',$6,$6,$7,$8)`, f.tenant, id(), f.entity, code, f.bank, f.actor, institution, transit)
		}
	}
	mustApp(t, f.tenant, bank("CA", s("003"), s("00002")))
	mustApp(t, f.tenant, bank("GB", nil, nil))
	failsWith(t, asApp(t, f.tenant, bank("X1", s("003"), nil)), "house_bank_ca_routing")
	failsWith(t, asApp(t, f.tenant, bank("X2", s("3"), s("00002"))), "house_bank_ca_routing")
}

func TestGoodsReceiptLimits(t *testing.T) {
	need(t)
	f := newFixture(t)
	po := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date, payment_term_code, status, created_by, updated_by)
			VALUES ($1,$2,'4500000001',$3,$4,'USD','2026-08-01','NT30','draft',$5,$5)`, f.tenant, po, f.entity, f.supplier, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id, over_delivery_pct)
			VALUES ($1,$2,10,'Paper',10,'EA',250,$3,10)`, f.tenant, po, f.expense); err != nil {
			return err
		}
		return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
	})
	receive := func(qty string, mt string, rev *uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			gm := id()
			if err := exec(tx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date, reversal_of_id, posted_by)
				VALUES ($1,$2,$3,$4,$5,'2026-08-05','2026-08-05',$6,$7)`, f.tenant, gm, gm.String()[24:], f.entity, mt, rev, f.actor); err != nil {
				return err
			}
			if rev != nil {
				*rev = gm
			}
			return exec(tx, "INSERT INTO procurement.goods_movement_item VALUES ($1,$2,1,$3,10,$4,'EA')", f.tenant, gm, po, qty)
		}
	}
	mustApp(t, f.tenant, receive("10", "101", nil))
	mustApp(t, f.tenant, receive("1", "101", nil)) // within 10% tolerance
	failsWith(t, asApp(t, f.tenant, receive("1", "101", nil)), "exceeds ordered")
	var first uuid.UUID
	if err := testDB.QueryRow(context.Background(), "SELECT id FROM procurement.goods_movement WHERE tenant_id = $1 ORDER BY posted_at LIMIT 1", f.tenant).Scan(&first); err != nil {
		t.Fatalf("first goods movement: %v", err)
	}
	failsWith(t, asApp(t, f.tenant, receive("-12", "102", &first)), "reversal exceeds")
}

// --- Identity and workflow ----------------------------------------------------

func TestAgentsCannotHoldApprovalPermissions(t *testing.T) {
	need(t)
	f := newFixture(t)
	agent, approver, clerk := id(), id(), id()
	grant := func(tx pgx.Tx, role uuid.UUID, perm string) error {
		return exec(tx, "INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by) VALUES ($1,$2,$3,$4,current_date,$5)",
			f.tenant, id(), role, perm, f.actor)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, "INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1,$2,'agent','Config agent','active',$3,$3)", f.tenant, agent, f.actor); err != nil {
			return err
		}
		for _, r := range []struct {
			id   uuid.UUID
			code string
			perm string
		}{{approver, "approver", "payments.run.release"}, {clerk, "clerk", "payables.invoice.capture"}} {
			if err := exec(tx, "INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1,$2,$3,$3,'active',$4,$4)", f.tenant, r.id, r.code, f.actor); err != nil {
				return err
			}
			if err := grant(tx, r.id, r.perm); err != nil {
				return err
			}
		}
		return exec(tx, "INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1,gen_random_uuid(),$2,$3,'2026-01-01',$4)", f.tenant, agent, clerk, f.actor)
	})
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1,gen_random_uuid(),$2,$3,'2026-01-01',$4)", f.tenant, agent, approver, f.actor)
	}), "agents may not hold approval permission")
	// Adding an approval permission to a role an agent already holds is refused too.
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return grant(tx, clerk, "config.change.approve")
	}), "agents may not hold approval permission")
}

func TestWorkflowDecisionRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	req, role, requester, alice, bob := id(), id(), id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, status, created_by)
			VALUES ($1,$2,'requisition',$3,'PR 1',$4,'pending',$4)`, f.tenant, req, id(), requester); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO workflow.approval_step VALUES ($1,$2,1,$3,1)", f.tenant, req, role); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO workflow.approval_step VALUES ($1,$2,2,$3,1)", f.tenant, req, role)
	})
	decide := func(actor uuid.UUID, kind, decision string, step int, comment *string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO workflow.decision (tenant_id, id, request_id, step_no, actor_id, actor_kind, decision, comment) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
				f.tenant, id(), req, step, actor, kind, decision, comment)
		})
	}
	failsWith(t, decide(requester, "human", "approve", 1, nil), "cannot decide their own")
	failsWith(t, decide(alice, "agent", "approve", 1, nil), "decision_people_decide")
	failsWith(t, decide(alice, "human", "reject", 1, nil), "decision_check")
	failsWith(t, decide(alice, "human", "approve", 2, nil), "request is at step 1")
	if err := decide(alice, "human", "approve", 1, nil); err != nil {
		t.Fatal(err)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE workflow.approval_request SET current_step = 2, version = version + 1 WHERE tenant_id = $1 AND id = $2", f.tenant, req)
	})
	failsWith(t, decide(alice, "human", "approve", 2, nil), "decision_one_approval_per_actor")
	reason := "raised in error"
	failsWith(t, decide(bob, "human", "cancel", 2, &reason), "only the requester can cancel")
	failsWith(t, decide(requester, "agent", "cancel", 2, nil), "decision_check")
	// An agent that raised a request may withdraw it; it can never approve.
	if err := decide(requester, "agent", "cancel", 2, &reason); err != nil {
		t.Fatal(err)
	}
}

// --- Platform mechanics -------------------------------------------------------

func TestOptimisticLocking(t *testing.T) {
	need(t)
	f := newFixture(t)
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE partner.business_partner SET name = 'Renamed' WHERE tenant_id = $1 AND id = $2", f.tenant, f.supplier)
	}), "optimistic lock")
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE partner.business_partner SET name = 'Renamed', version = version + 1 WHERE tenant_id = $1 AND id = $2", f.tenant, f.supplier)
	})
}

func TestGaplessDocumentNumbers(t *testing.T) {
	need(t)
	f := newFixture(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO config.number_range (tenant_id, id, legal_entity_id, document_type, fiscal_year, prefix, first_value, last_value, next_value, is_gapless, created_by)
			VALUES ($1,$2,$3,'supplier_invoice',2026,'INV-',1,3,1,true,$4)`, f.tenant, id(), f.entity, f.actor)
	})
	next := func() (string, error) {
		var n string
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), "SELECT config.next_document_number($1,$2,'supplier_invoice',2026::smallint)", f.tenant, f.entity).Scan(&n)
		})
		return n, err
	}
	if n, _ := next(); n != "INV-1" {
		t.Fatalf("first number %q", n)
	}
	// A rolled-back transaction gives its number back.
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(tx, "SET LOCAL ROLE erp_app"); err != nil {
		t.Fatal(err)
	}
	if err := exec(tx, "SELECT set_config('app.tenant_id', $1, true)", f.tenant.String()); err != nil {
		t.Fatal(err)
	}
	var lost string
	if err := tx.QueryRow(ctx, "SELECT config.next_document_number($1,$2,'supplier_invoice',2026::smallint)", f.tenant, f.entity).Scan(&lost); err != nil {
		t.Fatalf("number in the rolled-back transaction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := next(); n != "INV-2" {
		t.Fatalf("gapless range skipped a number: %q", n)
	}
	if n, err := next(); err != nil {
		t.Fatalf("last number of the range: %q, %v", n, err)
	}
	_, err = next()
	failsWith(t, err, "exhausted")
	var missing string
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT config.next_document_number($1,$2,'payment_run',2026::smallint)", f.tenant, f.entity).Scan(&missing)
	}), "no number range")
}

func TestAssignmentsAreEndedNeverRewritten(t *testing.T) {
	need(t)
	f := newFixture(t)
	principal, role, assignment := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, "INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by) VALUES ($1,$2,'human','https://idp.example.com','idp|ann','Ann','active',$3,$3)", f.tenant, principal, f.actor); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1,$2,'ap','AP clerk','active',$3,$3)", f.tenant, role, f.actor); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1,$2,$3,$4,current_date - 30,$5)", f.tenant, assignment, principal, role, f.actor)
	})
	end := func(set string, args ...any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE identity.principal_role SET "+set+" WHERE tenant_id = $1 AND id = $2", append([]any{f.tenant, assignment}, args...)...)
		})
	}
	failsWith(t, end("role_id = $3", id()), "permission denied")
	failsWith(t, end("valid_to = current_date - 1, ended_by = $3", f.actor), "in the past")
	failsWith(t, end("valid_to = current_date + 10"), "ended_by is required")
	if err := end("valid_to = current_date + 10, ended_by = $3", f.actor); err != nil {
		t.Fatal(err)
	}
	failsWith(t, end("valid_to = current_date + 20, ended_by = $3", f.actor), "only be ended or shortened")
	failsWith(t, end("valid_to = NULL, ended_by = $3", f.actor), "only be ended or shortened")
	if err := end("valid_to = current_date + 5, ended_by = $3", f.actor); err != nil {
		t.Fatal(err)
	}
	var endedAt *time.Time
	if err := testDB.QueryRow(context.Background(), "SELECT ended_at FROM identity.principal_role WHERE tenant_id = $1 AND id = $2", f.tenant, assignment).Scan(&endedAt); err != nil {
		t.Fatalf("ended_at: %v", err)
	}
	if endedAt == nil {
		t.Fatal("ended_at not recorded")
	}
	// A new assignment may start where the old one ends, not before.
	reassign := func(from string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1,$2,$3,$4,"+from+",$5)", f.tenant, id(), principal, role, f.actor)
		})
	}
	failsWith(t, reassign("current_date + 4"), "exclusion constraint")
	if err := reassign("current_date + 5"); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxFollowsTheEventContract(t *testing.T) {
	need(t)
	f := newFixture(t)
	publish := func(schema, eventType string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO "+schema+".outbox (tenant_id, id, aggregate_type, aggregate_id, event_type, payload, actor_id) VALUES ($1,$2,'x',$3,$4,'{}',$5)",
				f.tenant, id(), id(), eventType, f.actor)
		})
	}
	if err := publish("payables", "com.example.erp.payables.supplier_invoice_posted.v1"); err != nil {
		t.Fatal(err)
	}
	if err := publish("config", "com.example.erp.configuration.configuration_activated.v1"); err != nil {
		t.Fatal(err)
	}
	if err := publish("platform", "com.example.erp.platform.operation_completed.v1"); err != nil {
		t.Fatal(err)
	}
	// A context can only publish its own events, in the CloudEvents type format.
	failsWith(t, publish("payables", "com.example.erp.ledger.journal_entry_posted.v1"), "outbox_event_type_check")
	failsWith(t, publish("payables", "erp.payables.supplier_invoice.posted.v1"), "outbox_event_type_check")
	// The relay marks rows published; nothing else about an event can change.
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE payables.outbox SET payload = '{\"x\":1}' WHERE tenant_id = $1", f.tenant)
	}), "permission denied")
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "UPDATE payables.outbox SET published_at = now() WHERE tenant_id = $1", f.tenant)
	})
}

func TestInvoiceLifecycleConsistency(t *testing.T) {
	need(t)
	f := newFixture(t)
	invoice := func(status string, entry, reversal bool) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			var je, rev any
			if entry {
				je = id()
			}
			if reversal {
				rev = id()
			}
			return exec(tx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
				invoice_date, currency, gross_amount, tax_amount, status, source, journal_entry_year, journal_entry_id, reversal_entry_year, reversal_entry_id, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,'R-1','2026-06-01','USD',100,0,$6,'manual',
				        CASE WHEN $7::uuid IS NULL THEN NULL ELSE 2026 END, $7, CASE WHEN $8::uuid IS NULL THEN NULL ELSE 2026 END, $8, $9,$9)`,
				f.tenant, id(), id().String()[24:], f.entity, f.supplier, status, je, rev, f.actor)
		}
	}
	failsWith(t, asApp(t, f.tenant, invoice("posted", false, false)), "supplier_invoice_check")
	failsWith(t, asApp(t, f.tenant, invoice("reversed", true, false)), "supplier_invoice_check")
	failsWith(t, asApp(t, f.tenant, invoice("parked", false, true)), "supplier_invoice_check")
	// A reversed invoice frees its reference for a corrected entry; a posted one does not.
	mustApp(t, f.tenant, invoice("reversed", true, true))
	mustApp(t, f.tenant, invoice("posted", true, false))
	failsWith(t, asApp(t, f.tenant, invoice("parked", false, false)), "supplier_invoice_no_duplicates")
}

// TestItemReservationsHoldOnce: an open item is held by one owner at a time
// (item_reserved_once); a reservation is released once and otherwise never
// changes; it names a journal line that exists.
func TestItemReservationsHoldOnce(t *testing.T) {
	need(t)
	f := newFixture(t)
	var inv uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		inv, err = f.entry(tx, 7, "2026-07-02", line{account: f.expense, amount: 400}, line{account: f.ap, amount: -400, partner: &f.supplier})
		return err
	})
	run1, run2 := id(), id()
	reserve := func(owner, entry uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context, owner_reference, created_by)
				VALUES ($1, $2, 2026, $3, 2, 'payments', 'PR1', $4)`, f.tenant, owner, entry, f.actor)
		}
	}
	release := func(owner uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.item_reservation SET released_at = now(), released_by = $3 WHERE tenant_id = $1 AND owner_id = $2`, f.tenant, owner, f.actor)
		}
	}
	mustApp(t, f.tenant, reserve(run1, inv))
	failsWith(t, asApp(t, f.tenant, reserve(run2, inv)), "item_reserved_once")
	failsWith(t, asApp(t, f.tenant, reserve(run2, id())), "violates foreign key constraint")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.item_reservation SET released_at = now() WHERE tenant_id = $1`, f.tenant)
	}), "item_reservation_check")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.item_reservation SET owner_id = $2 WHERE tenant_id = $1`, f.tenant, run2)
	}), "permission denied")
	mustApp(t, f.tenant, release(run1))
	failsWith(t, asApp(t, f.tenant, release(run1)), "already released")
	mustApp(t, f.tenant, reserve(run2, inv))
	// Only the payments context owns reservations so far.
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context, owner_reference, created_by)
			VALUES ($1, $2, 2026, $3, 2, 'payables', 'X', $4)`, f.tenant, id(), inv, f.actor)
	}), "item_reservation_owner_context_check")
}

// TestExclusionReasons: a run excludes a supplier for one of the known
// reasons, among them the two a release drops a payment for (000035).
func TestExclusionReasons(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,$4,'2026-07-31','2026-07-31','USD',0,'releasing',$5,$5,$5)`, f.tenant, run, f.entity, hb, f.actor)
	})
	exclude := func(reason string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.payment_exclusion (tenant_id, payment_run_id, supplier_id, reason_code, amount) VALUES ($1,$2,$3,$4,100)`,
				f.tenant, run, id(), reason)
		}
	}
	for _, reason := range []string{"item_reversed", "item_not_open", "debit_balance"} {
		mustApp(t, f.tenant, exclude(reason))
	}
	failsWith(t, asApp(t, f.tenant, exclude("item_lost")), "payment_exclusion_reason_code_check")
	// T46c: a request left out has an exclusion of its own, once per run,
	// for a reason its supplier cannot be paid or its tax code.
	request := id()
	excludeRequest := func(request uuid.UUID, reason string, amount int) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.request_exclusion (tenant_id, payment_run_id, down_payment_request_id, supplier_id, reason_code, amount)
				VALUES ($1,$2,$3,$4,$5,$6)`, f.tenant, run, request, id(), reason, amount)
		}
	}
	mustApp(t, f.tenant, excludeRequest(request, "tax_code_invalid", 100))
	mustApp(t, f.tenant, excludeRequest(id(), "supplier_blocked", 100))
	for _, c := range []struct {
		name   string
		insert func(tx pgx.Tx) error
		fails  string
	}{
		{"a request twice", excludeRequest(request, "supplier_blocked", 100), "request_exclusion_pkey"},
		{"a debit balance", excludeRequest(id(), "debit_balance", 100), "request_exclusion_reason_code_check"},
		{"nothing left out", excludeRequest(id(), "tax_code_invalid", 0), "request_exclusion_amount_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.insert), c.fails) })
	}
}

// TestPrincipalsAreTheirIssuersSubjects: a principal's issuer and subject
// are unique per tenant (000038, ADR-0010), a subject alone is not
// (000048), and a principal with a subject has an issuer (000046); the
// unique index the unit of work resolves tokens with is built and valid.
func TestPrincipalsAreTheirIssuersSubjects(t *testing.T) {
	need(t)
	f := newFixture(t)
	insert := func(issuer, subject string) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by) VALUES ($1,$2,'human',$3,$4,'P','active',$5,$5)",
				f.tenant, id(), issuer, subject, f.actor)
		}
	}
	mustApp(t, f.tenant, insert("https://a.example.com", "idp|bo"))
	failsWith(t, asApp(t, f.tenant, insert("https://a.example.com", "idp|bo")), "duplicate key")
	// The subject alone is no longer unique (000048): another issuer may issue it.
	if err := asApp(t, f.tenant, insert("https://b.example.com", "idp|bo")); err != nil {
		t.Errorf("the same subject under another issuer: got %v, want success", err)
	}
	// A subject needs its issuer (000046, 000047).
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, status, created_by, updated_by) VALUES ($1,$2,'human','idp|cy','P','active',$3,$3)",
			f.tenant, id(), f.actor)
	}), "principal_issuer_with_subject")
	var valid, unique bool
	if err := testDB.QueryRow(context.Background(), "SELECT indisvalid, indisunique FROM pg_index WHERE indexrelid = 'identity.principal_issuer_subject'::regclass").Scan(&valid, &unique); err != nil {
		t.Fatalf("index: %v", err)
	}
	if !valid {
		t.Errorf("principal_issuer_subject valid: got %v, want true", valid)
	}
	if !unique {
		t.Errorf("principal_issuer_subject unique: got %v, want true", unique)
	}
}

// TestPrincipalIssuerBackfill: 000037 binds the principals that exist
// before it to the development issuer, the single issuer of the
// installations that hold any, and leaves agents without one; a fresh
// install has nothing to backfill.
func TestPrincipalIssuerBackfill(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	migrateBefore(t, conn, 37)
	tenant, human, agent := id(), id(), id()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Acme', 'eu-west', 'EUR', 'active')", []any{tenant}},
		{"INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, status, created_by, updated_by) VALUES ($1,$2,'human','idp|ann','Ann','active',$2,$2)", []any{tenant, human}},
		{"INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1,$2,'agent','Agent','active',$3,$3)", []any{tenant, agent, human}},
	} {
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
	}
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   uuid.UUID
		want string
	}{{"human", human, "erp-dev"}, {"agent", agent, "(none)"}} {
		var got string
		if err := conn.QueryRow(ctx, "SELECT coalesce(external_issuer, '(none)') FROM identity.principal WHERE id = $1", c.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s issuer: got %s, want %s", c.name, got, c.want)
		}
	}
}

// TestRequireTenant: platform.require_tenant() is the transaction's tenant,
// and fails a transaction without one rather than answer NULL (000039).
func TestRequireTenant(t *testing.T) {
	need(t)
	tenant := id()
	mustApp(t, tenant, func(tx pgx.Tx) error {
		var got uuid.UUID
		if err := tx.QueryRow(context.Background(), `SELECT platform.require_tenant()`).Scan(&got); err != nil {
			return err
		}
		if got != tenant {
			t.Errorf("require_tenant: got %s, want %s", got, tenant)
		}
		return nil
	})
	failsWith(t, asApp(t, uuid.Nil, func(tx pgx.Tx) error {
		return exec(tx, `SELECT platform.require_tenant()`)
	}), "no tenant scope")
}

// TestMatchResultInvoiceVersion: a match result records the positive
// invoice version it is valid for, or none (matches before 000040, which
// count for no version), and keeps it: results are insert-only (F13).
func TestMatchResultInvoiceVersion(t *testing.T) {
	need(t)
	f := newFixture(t)
	invoice := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
			invoice_date, currency, gross_amount, tax_amount, status, source, created_by, updated_by)
			VALUES ($1,$2,'M-1',$3,$4,'R-M1','2026-06-01','USD',100,0,'parked','manual',$5,$5)`, f.tenant, invoice, f.entity, f.supplier, f.actor)
	})
	match := func(version any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.match_result (tenant_id, id, supplier_invoice_id, evaluated_at, is_clean, config_version, evaluated_by,
				invoice_version) VALUES ($1,$2,$3,clock_timestamp(),true,1,$4,$5)`, f.tenant, id(), invoice, f.actor, version)
		}
	}
	for _, c := range []struct {
		name    string
		version any
		fails   string
	}{
		{"positive", 1, ""},
		{"none", nil, ""},
		{"zero", 0, "match_result_invoice_version_positive"},
		{"negative", -1, "match_result_invoice_version_positive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, match(c.version))
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsWith(t, err, c.fails)
		})
	}
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE payables.match_result SET invoice_version = 2 WHERE supplier_invoice_id = $1`, invoice)
	}), "permission denied")
}

// TestInvoiceExceptionVersion: an invoice records the version its approved
// exception left it at, a positive one no later than its own, or none
// (approvals before 000041, which count for no version) (F16).
func TestInvoiceExceptionVersion(t *testing.T) {
	need(t)
	f := newFixture(t)
	for i, c := range []struct {
		name    string
		version any
		fails   string
	}{
		{"current", 2, ""},
		{"earlier", 1, ""},
		{"none", nil, ""},
		{"zero", 0, "supplier_invoice_exception_version_valid"},
		{"later", 3, "supplier_invoice_exception_version_valid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// An invoice of version 1 moved on to version 2, as approved.
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				invoice := id()
				if err := exec(tx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
					invoice_date, currency, gross_amount, tax_amount, status, source, created_by, updated_by)
					VALUES ($1,$2,$3,$4,$5,$3,'2026-06-01','USD',100,0,'parked','manual',$6,$6)`,
					f.tenant, invoice, fmt.Sprintf("E-%d", i), f.entity, f.supplier, f.actor); err != nil {
					return err
				}
				return exec(tx, `UPDATE payables.supplier_invoice SET exception_version = $2, version = 2 WHERE id = $1`, invoice, c.version)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("approve: got %v, want success", err)
				}
				return
			}
			failsWith(t, err, c.fails)
		})
	}
}

// TestLocalCurrencyIsFixed: a legal entity's local currency is set when it
// is created and never changes (000045, F14): configurations are checked
// against it and its postings are kept in it. Its other details still change.
func TestLocalCurrencyIsFixed(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct {
		name, set, fails string
	}{
		{"renamed", "name = 'Acme Inc.'", ""},
		{"same currency", "local_currency = 'USD'", ""},
		{"another currency", "local_currency = 'EUR'", "local currency of legal entity"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "UPDATE tenancy.legal_entity SET "+c.set+", version = version + 1 WHERE id = $1", f.entity)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("update: got %v, want success", err)
				}
				return
			}
			failsWith(t, err, c.fails)
		})
	}
}

// --- GR/IR clearing (T33a, 000053) -------------------------------------------

// failsOn asserts err is a PostgreSQL error raised for the named constraint,
// the name the application maps to a problem (problem.RegisterConstraint).
func failsOn(t *testing.T, err error, constraint string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("want a violation of %s, got %v", constraint, err)
	}
	if pg.ConstraintName != constraint {
		t.Fatalf("constraint: got %q (%s), want %q", pg.ConstraintName, pg.Message, constraint)
	}
}

// grIRAccount sets up a liability account in the fixture's legal entity and
// makes it the entity's GR/IR account.
func (f fixture) grIRAccount(t *testing.T) uuid.UUID {
	t.Helper()
	account := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
			VALUES ($1,$2,$3,'219000','GR/IR clearing','liability','none','active',$4,$4)`, f.tenant, account, f.chart, f.actor); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)",
			f.tenant, f.entity, account, f.actor); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1,$2,'gr_ir',$3,$4)",
			f.tenant, f.entity, account, f.actor)
	})
	return account
}

// TestAccountDeterminationKeys: price_difference is a key of the account
// determination; a key outside the list is refused.
func TestAccountDeterminationKeys(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct{ key, fails string }{{"price_difference", ""}, {"price_variance", "account_determination_key_check"}} {
		t.Run(c.key, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1,$2,$3,$4,$5)",
					f.tenant, f.entity, c.key, f.expense, f.actor)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
}

// grIRLine is a journal line with the order item it belongs to.
type grIRLine struct {
	account        uuid.UUID
	amount         int64
	order          *uuid.UUID
	item, quantity *string
}

// grIREntry posts an entry of the source context whose lines may name an
// order item.
func (f fixture) grIREntry(source string, lines ...grIRLine) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		je := id()
		if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
			document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
			VALUES ($1,2026,$2,$3,$4,8,'WE',$5,'2026-08-05','2026-08-05','USD','USD','EUR',1,0.9,$6,$7)`,
			f.tenant, je, f.entity, f.ledger, je.String()[24:], source, f.actor); err != nil {
			return err
		}
		for i, l := range lines {
			var unit *string
			if l.quantity != nil {
				ea := "EA"
				unit = &ea
			}
			if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
				posting_date, account_id, amount_tc, amount_lc, amount_gc, quantity, uom_code, purchase_order_id, po_item_no)
				VALUES ($1,2026,$2,$3,$4,$5,8,'2026-08-05',$6,$7,$7,$8,$9::numeric,$10,$11,$12::int)`,
				f.tenant, je, i+1, f.entity, f.ledger, l.account, l.amount, l.amount*9/10, l.quantity, unit, l.order, l.item); err != nil {
				return err
			}
		}
		return nil
	}
}

// TestGRIRLines: a line names its order item with both the order and the
// item and a quantity, and a line on the legal entity's GR/IR account names
// its order item unless a migration run loaded it.
func TestGRIRLines(t *testing.T) {
	need(t)
	f := newFixture(t)
	grir := f.grIRAccount(t)
	order, item, qty := id(), "10", "-4"
	for _, c := range []struct {
		name, source string
		line         grIRLine
		fails        string
	}{
		{"GR/IR line with its order item", "procurement", grIRLine{grir, -100, &order, &item, &qty}, ""},
		{"order without its item", "procurement", grIRLine{grir, -100, &order, nil, &qty}, "journal_line_order_item_pair"},
		{"item without its order", "procurement", grIRLine{grir, -100, nil, &item, &qty}, "journal_line_order_item_pair"},
		{"order item without a quantity", "procurement", grIRLine{grir, -100, &order, &item, nil}, "journal_line_order_item_quantity"},
		{"GR/IR line without an order item", "ledger", grIRLine{grir, -100, nil, nil, nil}, "journal_line_gr_ir_order_item"},
		{"GR/IR line of another context without an order item", "payables", grIRLine{grir, -100, nil, nil, nil}, "journal_line_gr_ir_order_item"},
		{"GR/IR line a migration run loaded", "integration", grIRLine{grir, -100, nil, nil, nil}, ""},
		{"another account without an order item", "ledger", grIRLine{f.bank, -100, nil, nil, nil}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, f.grIREntry(c.source, c.line, grIRLine{account: f.expense, amount: 100}))
			if c.fails == "" {
				if err != nil {
					t.Fatalf("post: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
}

// TestOrderValuation: an order is valued directly or through GR/IR, decided
// when it is released and fixed from then on; a draft is valued directly.
func TestOrderValuation(t *testing.T) {
	need(t)
	f := newFixture(t)
	order := func(status, valuation string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			o := id()
			return exec(tx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
				payment_term_code, status, valuation, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,'USD','2026-08-01','NT30',$6,$7,$8,$8)`,
				f.tenant, o, o.String()[24:], f.entity, f.supplier, status, valuation, f.actor)
		}
	}
	t.Run("unknown valuation", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, order("released", "standard")), "purchase_order_valuation_check")
	})
	t.Run("draft through GR/IR", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, order("draft", "gr_ir")), "purchase_order_draft_direct")
	})
	var draft uuid.UUID
	mustApp(t, f.tenant, order("draft", "direct"))
	if err := testDB.QueryRow(context.Background(), "SELECT id FROM procurement.purchase_order WHERE tenant_id = $1 AND status = 'draft'", f.tenant).Scan(&draft); err != nil {
		t.Fatalf("read the draft: %v", err)
	}
	update := func(set string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE procurement.purchase_order SET "+set+", version = version + 1 WHERE id = $1", draft)
		})
	}
	// The cases run in order on one order: draft, released, closed.
	for _, c := range []struct{ name, set, fails string }{
		// Only the release decides it: a draft is not valued in advance.
		{"valued on its draft", "valuation = 'gr_ir'", "purchase_order_valuation_fixed"},
		{"released through GR/IR", "status = 'released', valuation = 'gr_ir'", ""},
		{"changed once released", "valuation = 'direct'", "purchase_order_valuation_fixed"},
		{"closed", "status = 'closed'", ""},
		{"changed once closed", "valuation = 'direct'", "purchase_order_valuation_fixed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := update(c.set)
			if c.fails == "" {
				if err != nil {
					t.Fatalf("update: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
}

// TestGoodsMovementEntry: a goods movement names its journal entry with
// both the entry and its fiscal year, or neither.
func TestGoodsMovementEntry(t *testing.T) {
	need(t)
	f := newFixture(t)
	entry := id()
	year := int16(2026)
	for _, c := range []struct {
		name  string
		entry *uuid.UUID
		year  *int16
		fails string
	}{
		{"no entry", nil, nil, ""},
		{"entry and year", &entry, &year, ""},
		{"entry without its year", &entry, nil, "goods_movement_entry_pair"},
		{"year without its entry", nil, &year, "goods_movement_entry_pair"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				gm := id()
				return exec(tx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date, posting_date,
					posted_by, journal_entry_year, journal_entry_id) VALUES ($1,$2,$3,$4,'101','2026-08-05','2026-08-05',$5,$6,$7)`,
					f.tenant, gm, gm.String()[24:], f.entity, f.actor, c.year, c.entry)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
}

// TestGRIRPositionBackfill: migration 000054 projects each order item's
// GR/IR position from its lines on the legal entity's GR/IR account in the
// leading ledger: received (the procurement context's lines, negated),
// invoiced (the payables context's), the balance in the three currencies
// and the last posting date. Each position equals the sum of its lines.
func TestGRIRPositionBackfill(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 54)
	f := fixtureIDs()
	order, other := id(), id()
	item := "10"
	qty := func(q string) *string { return &q }
	inTenant(t, conn, f.tenant, f.seed)
	grir := id()
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
			VALUES ($1,$2,$3,'219000','GR/IR clearing','liability','none','active',$4,$4)`, f.tenant, grir, f.chart, f.actor); err != nil {
			return err
		}
		if err := exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)",
			f.tenant, f.entity, grir, f.actor); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1,$2,'gr_ir',$3,$4)",
			f.tenant, f.entity, grir, f.actor)
	})
	inTenant(t, conn, f.tenant, f.grIREntry("procurement", grIRLine{grir, -300, &order, &item, qty("-3")}, grIRLine{account: f.expense, amount: 300}))
	inTenant(t, conn, f.tenant, f.grIREntry("payables", grIRLine{grir, 110, &order, &item, qty("1")}, grIRLine{account: f.expense, amount: -110}))
	inTenant(t, conn, f.tenant, f.grIREntry("procurement", grIRLine{grir, -50, &other, &item, qty("-1")}, grIRLine{account: f.expense, amount: 50}))
	inTenant(t, conn, f.tenant, f.grIREntry("ledger", grIRLine{f.expense, -70, &order, &item, qty("-7")}, grIRLine{account: f.bank, amount: 70})) // not on GR/IR
	var up Migration
	for _, m := range Migrations() {
		if v, _ := m.Version(); v == 54 {
			up = m
		}
	}
	if _, err := migrate(context.Background(), conn, []Migration{up}); err != nil {
		t.Fatalf("migrate 000054: %v", err)
	}
	rows, _ := conn.Query(context.Background(), `SELECT purchase_order_id = $2, trim_scale(received)::text, trim_scale(invoiced)::text, amount_tc, amount_lc,
		amount_gc, currency_tc, last_posting_date::text, is_open FROM ledger.gr_ir_position WHERE tenant_id = $1 ORDER BY 1 DESC`, f.tenant, order)
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var first, open bool
		var received, invoiced, currency, last string
		var tc, lc, gc int64
		err := r.Scan(&first, &received, &invoiced, &tc, &lc, &gc, &currency, &last, &open)
		return fmt.Sprint(first, received, invoiced, tc, lc, gc, currency, last, open), err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprint(true, "3", "1", -190, -190, -171, "USD", "2026-08-05", true), fmt.Sprint(false, "1", "0", -50, -50, -45, "USD", "2026-08-05", true)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("positions: got %v, want %v", got, want)
	}
}

// TestGRIRAccountIsFixed: once a legal entity has a GR/IR account its
// determination does not change (000054): the positions are its lines, and
// a reversal of one posted on another account would no longer be projected.
// Other keys still change.
func TestGRIRAccountIsFixed(t *testing.T) {
	need(t)
	f := newFixture(t)
	f.grIRAccount(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1,$2,'price_difference',$3,$4)",
			f.tenant, f.entity, f.expense, f.actor)
	})
	for _, c := range []struct{ name, key, fails string }{
		{"another price difference account", "price_difference", ""},
		{"another GR/IR account", "gr_ir", "account_determination_gr_ir_fixed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "UPDATE ledger.account_determination SET account_id = $3, version = version + 1 WHERE legal_entity_id = $1 AND key = $2",
					f.entity, c.key, f.bank)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("update: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
}

// --- Identity providers (ADR-0031) -------------------------------------------

// registerProvider inserts an identity provider of f's tenant.
func registerProvider(f fixture, provider uuid.UUID, issuer, audience string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO identity.identity_provider (tenant_id, id, issuer, audience, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $5)`, f.tenant, provider, issuer, audience, f.actor)
	}
}

// changeProvider updates provider p of f's tenant.
func changeProvider(f fixture, p uuid.UUID, set string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		return exec(tx, "UPDATE identity.identity_provider SET "+set+", version = version + 1 WHERE tenant_id = $1 AND id = $2", f.tenant, p)
	}
}

// TestIdentityProviders: an active issuer and audience decide one tenant,
// so the pair is unique across tenants; a registration's issuer and
// audience never change, and a retired one stays retired, freeing the
// pair (ADR-0031). The subtests share the registration p and run in order:
// the last one retires it.
func TestIdentityProviders(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	p := id()
	// Unique per run: an active pair stays registered, and a rerun (-count)
	// shares the database.
	issuer := "https://idp.example.com/a-" + id().String()
	mustApp(t, a.tenant, registerProvider(a, p, issuer, "erp"))

	t.Run("an active pair belongs to one tenant", func(t *testing.T) {
		failsWith(t, asApp(t, b.tenant, registerProvider(b, id(), issuer, "erp")), "identity_provider_issuer_audience")
	})
	t.Run("another audience is another registration", func(t *testing.T) {
		if err := asApp(t, b.tenant, registerProvider(b, id(), issuer, "erp-b")); err != nil {
			t.Errorf("same issuer, another audience: got %v, want registered", err)
		}
	})
	t.Run("an issuer is an http or https URL", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, registerProvider(a, id(), "ftp://idp.example.com", "erp")), "identity_provider_issuer_check")
	})
	t.Run("an audience is not empty", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, registerProvider(a, id(), "https://idp.example.com/e", "")), "identity_provider_audience_check")
	})
	t.Run("issuer and audience never change", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "issuer = 'https://other.example.com'")), "issuer and audience cannot change")
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "audience = 'other'")), "issuer and audience cannot change")
	})
	t.Run("tokens are not certificate-bound unless said", func(t *testing.T) {
		var bound bool
		if err := testDB.QueryRow(context.Background(), "SELECT certificate_bound FROM identity.identity_provider WHERE tenant_id = $1 AND id = $2", a.tenant, p).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if bound {
			t.Error("certificate_bound: got true, want false by default")
		}
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "certificate_bound = NULL")), "certificate_bound")
	})
	t.Run("retired_at goes with retired", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "status = 'retired'")), "identity_provider_retired_at")
	})
	t.Run("a retired registration frees its pair and stays retired", func(t *testing.T) {
		mustApp(t, a.tenant, changeProvider(a, p, "status = 'retired', retired_at = now()"))
		if err := asApp(t, b.tenant, registerProvider(b, id(), issuer, "erp")); err != nil {
			t.Errorf("the retired pair registered by another tenant: got %v, want registered", err)
		}
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "acr_values = '{x}'")), "is retired")
	})
}

// TestWebClients: a provider's web workspace client is all five fields or
// none, each in its form, and its login code names one active provider
// across tenants, freed when the provider retires (ADR-0052 G8).
func TestWebClients(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	p := id()
	code := "c-" + strings.ReplaceAll(id().String(), "-", "")[:20]
	mustApp(t, a.tenant, registerProvider(a, p, "https://web.example.com/"+id().String(), "erp"))
	web := func(code string) string {
		return "web_client_id = 'erp-web', web_auth_method = 'private_key_jwt', web_credential_ref = 'file:/k', web_login_code = '" + code + "', web_scope = 'openid erp'"
	}
	mustApp(t, a.tenant, changeProvider(a, p, web(code)))
	t.Run("a login code names one active provider", func(t *testing.T) {
		q := id()
		mustApp(t, b.tenant, registerProvider(b, q, "https://web.example.com/"+id().String(), "erp"))
		failsWith(t, asApp(t, b.tenant, changeProvider(b, q, web(code))), "identity_provider_login_code")
	})
	for name, c := range map[string]struct{ set, fails string }{
		"all or none":         {"web_scope = NULL", "identity_provider_web_client"},
		"a client id":         {"web_client_id = 'a b'", "identity_provider_web_client_id_check"},
		"an auth method":      {"web_auth_method = 'none'", "identity_provider_web_auth_method_check"},
		"a reference":         {"web_credential_ref = 'the-secret'", "identity_provider_web_credential_ref_check"},
		"a login code":        {"web_login_code = 'Acme'", "identity_provider_web_login_code_check"},
		"openid in the scope": {"web_scope = 'profile email'", "identity_provider_web_scope_check"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, a.tenant, changeProvider(a, p, c.set)), c.fails)
		})
	}
	t.Run("the control role reads it", func(t *testing.T) {
		tx, err := testDB.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }() // read only
		if _, err := tx.Exec(context.Background(), "SET LOCAL ROLE erp_control"); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := tx.QueryRow(context.Background(), `SELECT web_login_code FROM identity.identity_provider WHERE id = $1`, p).Scan(&got); err != nil || got != code {
			t.Errorf("got %q, %v, want %s", got, err, code)
		}
	})
	t.Run("retired, the code is free", func(t *testing.T) {
		mustApp(t, a.tenant, changeProvider(a, p, "status = 'retired', retired_at = now()"))
		q := id()
		mustApp(t, b.tenant, registerProvider(b, q, "https://web.example.com/"+id().String(), "erp"))
		if err := asApp(t, b.tenant, changeProvider(b, q, web(code))); err != nil {
			t.Errorf("got %v, want the freed code taken", err)
		}
	})
}

// TestAuthPolicy: a tenant's session lifetime and idle timeout stay within
// their bounds, and the idle timeout never exceeds the lifetime.
func TestAuthPolicy(t *testing.T) {
	need(t)
	f := newFixture(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO identity.auth_policy (tenant_id) VALUES ($1)", f.tenant)
	})
	var mfa bool
	var age, idle time.Duration
	if err := testDB.QueryRow(context.Background(), `SELECT mfa_required, extract(epoch FROM session_max_age)::bigint * 1000000000,
		extract(epoch FROM idle_timeout)::bigint * 1000000000 FROM identity.auth_policy WHERE tenant_id = $1`, f.tenant).Scan(&mfa, &age, &idle); err != nil {
		t.Fatal(err)
	}
	if mfa {
		t.Errorf("default mfa_required: got %v, want false", mfa)
	}
	if age != 12*time.Hour {
		t.Errorf("default session_max_age: got %v, want 12h", age)
	}
	if idle != 30*time.Minute {
		t.Errorf("default idle_timeout: got %v, want 30m", idle)
	}
	for _, c := range []struct{ name, set, fails string }{
		{"lifetime too short", "session_max_age = '14 minutes', idle_timeout = '5 minutes'", "auth_policy_session_max_age_check"},
		{"lifetime too long", "session_max_age = '25 hours'", "auth_policy_session_max_age_check"},
		{"idle too short", "idle_timeout = '4 minutes'", "auth_policy_idle_timeout_check"},
		{"idle too long", "session_max_age = '24 hours', idle_timeout = '9 hours'", "auth_policy_idle_timeout_check"},
		{"idle beyond the lifetime", "session_max_age = '1 hour', idle_timeout = '2 hours'", "auth_policy_idle_within_age"},
	} {
		t.Run(c.name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "UPDATE identity.auth_policy SET "+c.set+", version = version + 1 WHERE tenant_id = $1", f.tenant)
			}), c.fails)
		})
	}
}

// TestControlRoleReadsTheRegistry: the cell resolves a token's tenant
// before it knows the tenant, as the control role across tenants; it reads
// what decides the tenant and the policy, not who changed them. It reads
// connectors' kind and status, to refuse EBICS in production (000101,
// T44d), never their settings or secret references.
func TestControlRoleReadsTheRegistry(t *testing.T) {
	need(t)
	f := newFixture(t)
	mustApp(t, f.tenant, registerProvider(f, id(), "https://idp.example.com/control-"+id().String(), "erp")) // unique per run, as in TestIdentityProviders
	asControl := func(sql string) error {
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE erp_control"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql)
		return err
	}
	for _, c := range []struct{ name, sql, fails string }{
		{"providers", "SELECT tenant_id, id, issuer, audience, tenant_claim, acr_values, amr_values, status, certificate_bound FROM identity.identity_provider", ""},
		{"policies", "SELECT tenant_id, mfa_required, session_max_age, idle_timeout FROM identity.auth_policy", ""},
		{"principals", "SELECT tenant_id, id, kind, external_issuer, external_subject, display_name, status, version FROM identity.principal", ""},
		{"who registered", "SELECT created_by FROM identity.identity_provider", "permission denied"},
		{"who changed the policy", "SELECT updated_by FROM identity.auth_policy", "permission denied"},
		{"principals' email", "SELECT email FROM identity.principal", "permission denied"},
		{"connectors' kind and status", "SELECT tenant_id, id, kind, status FROM integration.connector", ""},
		{"connectors' settings", "SELECT settings FROM integration.connector", "permission denied"},
		{"connectors' secret references", "SELECT secret_ref FROM integration.connector", "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asControl(c.sql)
			if c.fails == "" {
				if err != nil {
					t.Fatalf("read: got %v, want success", err)
				}
				return
			}
			failsWith(t, err, c.fails)
		})
	}
}

// TestSessions: a session's key is unique per principal, only its last
// sighting and its end change, and an ended session never resumes
// (ADR-0031); the sweeper's role may delete them, the application may not.
func TestSessions(t *testing.T) {
	need(t)
	f := newFixture(t)
	principal := id()
	key := make([]byte, 32)
	start := func(tx pgx.Tx, s uuid.UUID) error {
		return exec(tx, `INSERT INTO identity.session (tenant_id, id, principal_id, session_key, started_at, last_seen_at) VALUES ($1, $2, $3, $4, now(), now())`,
			f.tenant, s, principal, key)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by) VALUES ($1,$2,'human','https://s.example.com','s|1','S','active',$2,$2)",
			f.tenant, principal)
	})
	s := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return start(tx, s) })
	update := func(set string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE identity.session SET "+set+", version = version + 1 WHERE id = $1", s)
		})
	}
	t.Run("one session per key", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return start(tx, id()) }), "session_principal_key")
	})
	t.Run("a key is a sha256", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO identity.session (tenant_id, id, principal_id, session_key, started_at, last_seen_at) VALUES ($1, $2, $3, '\x00', now(), now())`, f.tenant, id(), principal)
		}), "session_session_key_check")
	})
	t.Run("only the last sighting and the end change", func(t *testing.T) {
		if err := update("last_seen_at = now()"); err != nil {
			t.Errorf("last sighting: got %v, want updated", err)
		}
		failsWith(t, update("started_at = now() - interval '1 day'"), "only last_seen_at and its end change")
		failsWith(t, update("id = gen_random_uuid()"), "only last_seen_at and its end change")
		failsWith(t, update("authenticated_at = now()"), "only last_seen_at and its end change")
		failsWith(t, update("last_seen_at = now() - interval '1 day'"), "only moves on")
		failsWith(t, update("ended_at = now()"), "session_end_reason")
	})
	t.Run("an ended session never resumes", func(t *testing.T) {
		if err := update("ended_at = now(), end_reason = 'idle'"); err != nil {
			t.Fatal(err)
		}
		failsWith(t, update("last_seen_at = now()"), "has ended")
	})
	t.Run("the sweeper's role deletes, the application's does not", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, "DELETE FROM identity.session WHERE id = $1", s) }), "permission denied")
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE erp_worker"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM identity.session WHERE id = $1", s); err != nil {
			t.Errorf("sweeper delete: got %v, want deleted", err)
		}
	})
}

// --- Credit memos and subsequent debits/credits (T34a, 000059) ---------------

// invoiceOf inserts a parked supplier invoice of the kind, with its
// purchase order, reference invoice and gross amount, and returns its id.
func (f fixture) invoiceOf(kind string, order, reference *uuid.UUID, gross int64) (uuid.UUID, func(tx pgx.Tx) error) {
	v := id()
	return v, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
			invoice_date, currency, gross_amount, tax_amount, status, source, kind, purchase_order_id, reference_invoice_id, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$3,'2026-06-01','USD',$6,0,'parked','manual',$7,$8,$9,$10,$10)`,
			f.tenant, v, v.String()[24:], f.entity, f.supplier, gross, kind, order, reference, f.actor)
	}
}

// TestSupplierInvoiceKinds: an invoice is an invoice, a credit memo or a
// subsequent debit or credit, fixed once entered; it may name the invoice
// it corrects, never itself; subsequent debits and credits are on an order;
// amounts are positive whatever the kind, which gives the direction.
func TestSupplierInvoiceKinds(t *testing.T) {
	need(t)
	f := newFixture(t)
	order := id()
	invoice, ins := f.invoiceOf("invoice", nil, nil, 1000)
	mustApp(t, f.tenant, ins)
	for _, c := range []struct {
		name, kind string
		order      *uuid.UUID
		reference  *uuid.UUID
		gross      int64
		fails      string
	}{
		{"credit memo on its invoice", "credit_memo", nil, &invoice, 300, ""},
		{"subsequent debit on an order", "subsequent_debit", &order, nil, 50, ""},
		{"subsequent credit without an order", "subsequent_credit", nil, &invoice, 50, "supplier_invoice_subsequent_on_order"},
		{"unknown kind", "debit_memo", nil, nil, 50, "supplier_invoice_kind_check"},
		{"unknown reference", "credit_memo", nil, &order, 50, "supplier_invoice_reference_fkey"},
		{"negative amount", "credit_memo", nil, nil, -300, "supplier_invoice_amounts_not_negative"},
		{"zero amount", "credit_memo", nil, nil, 0, ""},
		{"invoice naming an invoice", "invoice", nil, &invoice, 300, "supplier_invoice_reference_corrects"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ins := f.invoiceOf(c.kind, c.order, c.reference, c.gross)
			err := asApp(t, f.tenant, ins)
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	t.Run("its own reference", func(t *testing.T) {
		memo, ins := f.invoiceOf("credit_memo", nil, nil, 100)
		mustApp(t, f.tenant, ins)
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE payables.supplier_invoice SET reference_invoice_id = id, version = version + 1 WHERE id = $1", memo)
		}), "supplier_invoice_not_its_own_reference")
	})
	t.Run("negative tax", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE payables.supplier_invoice SET tax_amount = -1, version = version + 1 WHERE id = $1", invoice)
		}), "supplier_invoice_amounts_not_negative")
	})
	t.Run("kind set to itself", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE payables.supplier_invoice SET kind = kind, version = version + 1 WHERE id = $1", invoice)
		})
	})
	t.Run("kind changed", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE payables.supplier_invoice SET kind = 'credit_memo', version = version + 1 WHERE id = $1", invoice)
		}), "supplier_invoice_kind_fixed")
	})
}

// TestSupplierInvoiceItemQuantities: a subsequent debit or credit moves no
// quantity, so its items are order items with quantity 0; every other
// kind's items have a positive quantity; a removed item no longer counts,
// and writing it back counts again.
func TestSupplierInvoiceItemQuantities(t *testing.T) {
	need(t)
	f := newFixture(t)
	order := id()
	documents := map[string]uuid.UUID{}
	for _, kind := range []string{"invoice", "credit_memo", "subsequent_debit", "subsequent_credit"} {
		v, ins := f.invoiceOf(kind, &order, nil, 50)
		mustApp(t, f.tenant, ins)
		documents[kind] = v
	}
	item := func(v uuid.UUID, qty string, onOrder, removed bool) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			var o *uuid.UUID
			var po *int
			if onOrder {
				ten := 10
				o, po = &order, &ten
			}
			return exec(tx, `INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no, quantity, uom_code,
				amount, account_id, is_removed) SELECT $1, $2, coalesce(max(item_no), 0) + 1, $3, $4, $5::numeric, 'EA', 50, $6, $7
				FROM payables.supplier_invoice_item WHERE supplier_invoice_id = $2`, f.tenant, v, o, po, qty, f.expense, removed)
		}
	}
	for _, c := range []struct {
		name, kind, qty string
		onOrder         bool
		removed         bool
		fails           string
	}{
		{"invoice item with a quantity", "invoice", "2", true, false, ""},
		{"invoice item without one", "invoice", "0", true, false, "supplier_invoice_item_quantity_kind"},
		{"removed invoice item without one", "invoice", "0", true, true, ""},
		{"credit memo item without a quantity", "credit_memo", "0", true, false, "supplier_invoice_item_quantity_kind"},
		{"subsequent debit item without a quantity", "subsequent_debit", "0", true, false, ""},
		{"subsequent debit item with one", "subsequent_debit", "2", true, false, "supplier_invoice_item_quantity_kind"},
		{"subsequent debit item on an account", "subsequent_debit", "0", false, false, "supplier_invoice_item_quantity_kind"},
		{"subsequent credit item without a quantity", "subsequent_credit", "0", true, false, ""},
		{"subsequent credit item with one", "subsequent_credit", "2", true, false, "supplier_invoice_item_quantity_kind"},
		{"negative quantity", "invoice", "-1", true, false, "supplier_invoice_item_quantity_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, item(documents[c.kind], c.qty, c.onOrder, c.removed))
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	update := func(set string) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE payables.supplier_invoice_item SET "+set+" WHERE supplier_invoice_id = $1 AND item_no = 1", documents["invoice"])
		})
	}
	t.Run("invoice item updated to no quantity", func(t *testing.T) {
		failsOn(t, update("quantity = 0"), "supplier_invoice_item_quantity_kind")
	})
	t.Run("removed item written back", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE payables.supplier_invoice_item SET is_removed = false WHERE supplier_invoice_id = $1 AND item_no = 2", documents["invoice"])
		}), "supplier_invoice_item_quantity_kind")
	})
}

// TestPaymentTermDiscountTiers: a payment term has up to two cash discount
// tiers (SAP T052 ZTAG1/ZPRZ1, ZTAG2/ZPRZ2), each of days and percent; the
// second only after the first, both within the net days, and the later
// tier gives less.
func TestPaymentTermDiscountTiers(t *testing.T) {
	need(t)
	f := newFixture(t)
	for i, c := range []struct {
		name, tiers, fails string
	}{
		{"net only", "NULL, NULL, NULL, NULL", ""},
		{"one tier", "14, 3, NULL, NULL", ""},
		{"two tiers", "14, 3, 30, 2", ""},
		{"second tier without days", "14, 3, NULL, 2", "payment_term_discount2_pair"},
		{"second tier without the first", "NULL, NULL, 30, 2", "payment_term_discount2_tiers"},
		{"second tier not later", "14, 3, 14, 2", "payment_term_discount2_tiers"},
		{"second tier at the net days", "14, 3, 45, 2", "payment_term_discount2_tiers"},
		{"second tier not less", "14, 3, 30, 3", "payment_term_discount2_tiers"},
		{"second tier percent", "14, 3, 30, 0", "payment_term_discount2_pct_check"},
		// No valid term reaches 100 on the second tier (it is below the
		// first, itself below 100), so the first breaks its check too; the
		// column check is evaluated before the table checks.
		{"second tier percent 100", "14, 100, 30, 100", "payment_term_discount2_pct_check"},
		{"second tier days negative", "14, 3, -1, 2", "payment_term_discount2_days_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, discount2_days, discount2_pct,
					created_by, updated_by) VALUES ($1, $2, 'Terms', 45, `+c.tiers+`, $3, $3)`, f.tenant, fmt.Sprintf("T%d", i), f.actor)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
}

// TestTermsFrozenAtPosting: an open item and a posted supplier invoice keep
// the payment terms they were posted with (T36b), by the rules of
// partner.payment_term, and the terms never change.
func TestTermsFrozenAtPosting(t *testing.T) {
	need(t)
	f := newFixture(t)
	var je uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		je, err = f.entry(tx, 4, "2026-04-02", line{account: f.expense, amount: 500}, line{account: f.ap, amount: -500, partner: &f.supplier})
		return err
	})
	invoice, ins := f.invoiceOf("invoice", nil, nil, 500)
	mustApp(t, f.tenant, ins)
	tables := map[string]func(tx pgx.Tx, line int, tiers string) error{
		"open_item_terms": func(tx pgx.Tx, line int, tiers string) error {
			return exec(tx, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days,
				discount_days, discount_pct, discount2_days, discount2_pct) VALUES ($1, 2026, $2, $3, 'ZB01', '2026-04-02', 45, `+tiers+`)`, f.tenant, je, line)
		},
		"supplier_invoice_terms": func(tx pgx.Tx, _ int, tiers string) error {
			return exec(tx, `INSERT INTO payables.supplier_invoice_terms (tenant_id, supplier_invoice_id, payment_term_code, baseline_date, net_days,
				discount_days, discount_pct, discount2_days, discount2_pct) VALUES ($1, $2, 'ZB01', '2026-04-02', 45, `+tiers+`)`, f.tenant, invoice)
		},
	}
	for table, insert := range tables {
		for _, c := range []struct {
			name, tiers, fails string
		}{
			{"first tier without its percent", "14, NULL, NULL, NULL", "pairs"},
			{"first tier at the net days", "45, 3, NULL, NULL", "tiers"},
			{"second tier without the first", "NULL, NULL, 30, 2", "tiers"},
			{"second tier not less", "14, 3, 30, 3", "tiers"},
			{"second tier not later", "14, 3, 14, 2", "tiers"},
			{"percent 100", "14, 100, NULL, NULL", "discount_pct_check"},
			{"percent 0", "14, 0, NULL, NULL", "discount_pct_check"},
			{"first tier days negative", "-1, 3, NULL, NULL", "discount_days_check"},
			{"second tier days negative", "14, 3, -1, 2", "discount2_days_check"},
			{"second tier percent 0", "14, 3, 30, 0", "discount2_pct_check"},
		} {
			t.Run(table+": "+c.name, func(t *testing.T) {
				failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return insert(tx, 2, c.tiers) }), table+"_"+c.fails)
			})
		}
		t.Run(table+": two tiers, then frozen", func(t *testing.T) {
			mustApp(t, f.tenant, func(tx pgx.Tx) error { return insert(tx, 2, "14, 3, 30, 2") })
			name := map[string]string{"open_item_terms": "ledger", "supplier_invoice_terms": "payables"}[table] + "." + table
			_, err := testDB.Exec(context.Background(), "UPDATE "+name+" SET net_days = 60 WHERE tenant_id = $1", f.tenant)
			failsWith(t, err, "insert-only")
			_, err = testDB.Exec(context.Background(), "DELETE FROM "+name+" WHERE tenant_id = $1", f.tenant)
			failsWith(t, err, "insert-only")
		})
	}
	for _, net := range []int{-1, 366} {
		t.Run(fmt.Sprintf("net days %d", net), func(t *testing.T) {
			failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days)
					VALUES ($1, 2026, $2, 1, 'ZB01', '2026-04-02', $3)`, f.tenant, je, net)
			}), "open_item_terms_net_days_check")
		})
	}
	t.Run("terms of no invoice", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.supplier_invoice_terms (tenant_id, supplier_invoice_id, payment_term_code, baseline_date, net_days)
				VALUES ($1, $2, 'ZB01', '2026-04-02', 30)`, f.tenant, id())
		}), "supplier_invoice_terms_tenant_id_supplier_invoice_id_fkey")
	})
	t.Run("terms of no line", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return tables["open_item_terms"](tx, 9, "NULL, NULL, NULL, NULL") }), "open_item_terms_line_fkey")
	})
}

// TestPaymentItemDiscounts: a payment item takes a cash discount of its
// own sign and less than its amount (T36c), fixed like the rest of the
// item, and a payment records the discounts its items take.
func TestPaymentItemDiscounts(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run, pay := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,$4,'2026-07-31','2026-07-31','USD',294,'proposed',$5,$5,$5)`, f.tenant, run, f.entity, hb, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, discount_amount, currency, end_to_end_id, created_by)
			VALUES ($1,$2,$3,$4,$5,294,6,'USD','E2E-1',$6)`, f.tenant, pay, run, f.supplier, id(), f.actor)
	})
	item := func(amount, discount int) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, discount_amount, due_date)
				VALUES ($1, $2, 2026, $3, 2, $4, $5, '2026-07-31')`, f.tenant, pay, id(), amount, discount)
		}
	}
	for _, c := range []struct {
		name             string
		amount, discount int
		fails            string
	}{
		{"an invoice's discount", 300, 6, ""},
		{"a credit memo's discount", -50, -1, ""},
		{"no discount", 300, 0, ""},
		{"a discount of the other sign", 300, -6, "payment_item_discount_within"},
		{"a discount of the whole amount", 300, 300, "payment_item_discount_within"},
		{"a credit memo's discount of the other sign", -50, 1, "payment_item_discount_within"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, item(c.amount, c.discount))
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	t.Run("a discount changed", func(t *testing.T) {
		_, err := testDB.Exec(context.Background(), "UPDATE payments.payment_item SET discount_amount = 1 WHERE tenant_id = $1 AND discount_amount = 6", f.tenant)
		failsWith(t, err, "only be deactivated")
	})
}

// TestModelCalls: model prices are reference data the application reads;
// a model call's row is the tenant's, never changed once written, with
// non-negative tokens and cost and a known outcome; a session sums its
// calls' cost in millionths of the minor unit (ADR-0036).
func TestModelCalls(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	session, call := id(), id()
	// Prices are global; each run prices a model of its own.
	priced := "test-" + id().String()
	mustApp(t, a.tenant, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM ref.model_price").Scan(&n); err != nil {
			return err // the application reads prices
		}
		if err := exec(tx, "INSERT INTO agents.session (tenant_id, id, agent_id, purpose, model, status, cost_currency) VALUES ($1,$2,$3,'test','erp-default','running','USD')", a.tenant, session, a.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO agents.model_call (tenant_id, id, principal_id, session_id, alias, model, status, input_tokens, output_tokens, cost_micro, currency)
			VALUES ($1,$2,$3,$4,'erp-default','claude-opus-5-5','succeeded',10,20,4400,'USD')`, a.tenant, call, a.actor, session)
	})
	t.Run("a call is never changed", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE agents.model_call SET cost_micro = 0 WHERE tenant_id = $1 AND id = $2", a.tenant, call)
		}), "model_call")
	})
	t.Run("tokens and cost are not negative", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO agents.model_call (tenant_id, id, principal_id, alias, model, status, input_tokens, output_tokens, cost_micro, currency)
				VALUES ($1, gen_random_uuid(), $2, 'erp-default', 'claude-opus-5-5', 'succeeded', -1, 0, 0, 'USD')`, a.tenant, a.actor)
		}), "model_call_input_tokens_check")
	})
	t.Run("redactions are counted, not negative", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO agents.model_call (tenant_id, id, principal_id, alias, model, status, input_tokens, output_tokens, cost_micro, currency, redactions)
				VALUES ($1, gen_random_uuid(), $2, 'erp-default', 'claude-opus-5-5', 'succeeded', 0, 0, 0, 'USD', -1)`, a.tenant, a.actor)
		}), "model_call_redactions_check")
	})
	t.Run("an outcome is known", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO agents.model_call (tenant_id, id, principal_id, alias, model, status, input_tokens, output_tokens, cost_micro, currency)
				VALUES ($1, gen_random_uuid(), $2, 'erp-default', 'claude-opus-5-5', 'maybe', 0, 0, 0, 'USD')`, a.tenant, a.actor)
		}), "model_call_status_check")
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		mustApp(t, b.tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM agents.model_call").Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("calls visible to another tenant: %d", n)
			}
			return nil
		})
	})
	t.Run("a session's cost in millionths", func(t *testing.T) {
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE agents.session SET cost_micro = cost_micro + 4400, version = version + 1 WHERE tenant_id = $1 AND id = $2", a.tenant, session)
		})
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE agents.session SET cost_micro = -1, version = version + 1 WHERE tenant_id = $1 AND id = $2", a.tenant, session)
		}), "session_cost_micro_check")
	})
	t.Run("prices are not negative", func(t *testing.T) {
		_, err := testDB.Exec(context.Background(), "INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read, recorded_by) VALUES ($1, '2026-01-01', 'USD', -1, 0, 0, 0, 'ops')", priced)
		failsWith(t, err, "model_price_input_check")
	})
	t.Run("prices are in one currency", func(t *testing.T) {
		_, err := testDB.Exec(context.Background(), "INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read, recorded_by) VALUES ($1, '2026-03-01', 'EUR', 1, 1, 1, 1, 'ops')", priced)
		failsWith(t, err, "model_price_currency_check")
	})
	t.Run("a recorded price stands", func(t *testing.T) {
		if _, err := testDB.Exec(context.Background(), "INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read, recorded_by) VALUES ($1, '2026-02-01', 'USD', 1, 1, 1, 1, 'ops')", priced); err != nil {
			t.Fatal(err)
		}
		_, err := testDB.Exec(context.Background(), "UPDATE ref.model_price SET input = 0 WHERE model = $1", priced)
		failsWith(t, err, "insert-only")
	})
}

// TestModelBudgets: a budget line is the tenant's or an agent's, one per
// month and in the prices' currency, never spent below nothing, and
// announces only 80% and 100%; a reservation holds a non-negative amount
// until it expires, after it was made; the application settles
// reservations but deletes neither them nor lines; another tenant sees
// none (ADR-0036).
func TestModelBudgets(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	tenantLine, agentLine := id(), id()
	insertLine := func(line uuid.UUID, agent *uuid.UUID, month, currency string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO agents.spend (tenant_id, id, principal_id, month, currency) VALUES ($1, $2, $3, $4, $5)", a.tenant, line, agent, month, currency)
		}
	}
	insertReservation := func(amount int64, reserved, expires string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO agents.model_reservation (tenant_id, id, spend_id, amount_micro, reserved_at, expires_at) VALUES ($1, gen_random_uuid(), $2, $3, $4, $5)`,
				a.tenant, tenantLine, amount, reserved, expires)
		}
	}
	agent := id()
	mustApp(t, a.tenant, insertLine(tenantLine, nil, "2026-10-01", "USD"))
	mustApp(t, a.tenant, insertLine(agentLine, &agent, "2026-10-01", "USD"))
	mustApp(t, a.tenant, insertReservation(2000000, "2026-10-17T12:00:00Z", "2026-10-17T12:12:00Z"))
	for _, c := range []struct {
		name       string
		insert     func(tx pgx.Tx) error
		constraint string
	}{
		{"one tenant line a month", insertLine(id(), nil, "2026-10-01", "USD"), "spend_line_key"},
		{"one line a month per agent", insertLine(id(), &agent, "2026-10-01", "USD"), "spend_line_key"},
		{"a month is its first day", insertLine(id(), nil, "2026-11-02", "USD"), "spend_month_check"},
		{"in the prices' currency", insertLine(id(), nil, "2026-12-01", "EUR"), "spend_currency_check"},
		{"a reservation holds no negative amount", insertReservation(-1, "2026-10-17T12:00:00Z", "2026-10-17T12:12:00Z"), "model_reservation_amount_check"},
		{"a reservation expires after it is made", insertReservation(1, "2026-10-17T12:00:00Z", "2026-10-17T12:00:00Z"), "model_reservation_expiry_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			failsOn(t, asApp(t, a.tenant, c.insert), c.constraint)
		})
	}
	for _, c := range []struct {
		name, set, constraint string
	}{{"spend is not negative", "spent_micro = -1", "spend_spent_micro_check"}, {"only 80% and 100% are announced", "threshold = 50", "spend_threshold_check"}} {
		t.Run(c.name, func(t *testing.T) {
			failsOn(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
				return exec(tx, "UPDATE agents.spend SET "+c.set+", version = version + 1 WHERE tenant_id = $1 AND id = $2", a.tenant, agentLine)
			}), c.constraint)
		})
	}
	t.Run("a reservation is settled", func(t *testing.T) {
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE agents.model_reservation SET settled_at = now(), version = version + 1 WHERE tenant_id = $1 AND spend_id = $2", a.tenant, tenantLine)
		})
	})
	for _, table := range []string{"agents.model_reservation", "agents.spend"} {
		t.Run("the application deletes no row of "+table, func(t *testing.T) {
			failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
				return exec(tx, "DELETE FROM "+table+" WHERE tenant_id = $1", a.tenant)
			}), "permission denied")
		})
	}
	t.Run("another tenant sees none", func(t *testing.T) {
		mustApp(t, b.tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM agents.spend) + (SELECT count(*) FROM agents.model_reservation)").Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("budget rows visible to another tenant: %d", n)
			}
			return nil
		})
	})
}

// TestAIPolicy: a tenant's AI policy is one row, the tenant's alone; a
// suspension says why and nothing else does; a cap is in the prices'
// currency with its amount, never negative; routes are an object; the
// permissions to read and manage it exist and are no approvals (ADR-0036).
func TestAIPolicy(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	mustApp(t, a.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO agents.ai_policy (tenant_id, updated_by) VALUES ($1, $2)", a.tenant, a.actor)
	})
	for _, c := range []struct {
		name, set, constraint string
	}{
		{"a suspension says why", "suspended = true", "ai_policy_suspension_reason"},
		{"only a suspension has a reason", "suspended_reason = 'audit'", "ai_policy_suspension_reason"},
		{"a cap has its currency", "monthly_cap = 100", "ai_policy_cap_currency"},
		{"a cap is not negative", "monthly_cap = -1, cap_currency = 'USD'", "ai_policy_monthly_cap_check"},
		{"a cap is in the prices' currency", "monthly_cap = 100, cap_currency = 'EUR'", "ai_policy_cap_currency_check"},
		{"routes are an object", "routes = '[]'", "ai_policy_routes_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			failsOn(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
				return exec(tx, "UPDATE agents.ai_policy SET "+c.set+", version = version + 1 WHERE tenant_id = $1", a.tenant)
			}), c.constraint)
		})
	}
	t.Run("one policy a tenant", func(t *testing.T) {
		failsOn(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, "INSERT INTO agents.ai_policy (tenant_id) VALUES ($1)", a.tenant)
		}), "ai_policy_pkey")
	})
	t.Run("a suspension with its reason", func(t *testing.T) {
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE agents.ai_policy SET suspended = true, suspended_reason = 'billing dispute', version = version + 1 WHERE tenant_id = $1", a.tenant)
		})
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		mustApp(t, b.tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), "SELECT count(*) FROM agents.ai_policy").Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("policies visible to another tenant: %d", n)
			}
			return nil
		})
	})
	t.Run("the permissions exist", func(t *testing.T) {
		var n int
		if err := testDB.QueryRow(context.Background(), `SELECT count(*) FROM ref.permission WHERE code IN ('agents.ai_policy.read', 'agents.ai_policy.manage') AND NOT is_approval`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("AI policy permissions: got %d, want 2", n)
		}
	})
}

// TestAgentModelAliases: 000076 maps the agent profiles that name a model
// outright to the alias the gateway routes (ADR-0036): every Claude model
// to erp-default. An alias stays, and a value it does not know stays too,
// for the next write of the profile to refuse.
func TestAgentModelAliases(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	migrateBefore(t, conn, 76)
	tenant, human := id(), id()
	agents := map[string]string{"claude-opus-5-5": "erp-default", "claude-haiku-5": "erp-default", "erp-analytics": "erp-analytics", "gpt-9": "gpt-9"}
	ids := map[string]uuid.UUID{}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Acme', 'eu-west', 'EUR', 'active')", tenant)
	for model := range agents {
		ids[model] = id()
		exec("INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1,$2,'agent','Agent','active',$3,$3)",
			tenant, ids[model], human)
		exec("INSERT INTO identity.agent_profile (tenant_id, principal_id, model, description, created_by, updated_by) VALUES ($1,$2,$3,'d',$4,$4)",
			tenant, ids[model], model, human)
	}
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for model, want := range agents {
		t.Run(model, func(t *testing.T) {
			var got string
			if err := conn.QueryRow(ctx, "SELECT model FROM identity.agent_profile WHERE principal_id = $1", ids[model]).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("model: got %s, want %s", got, want)
			}
		})
	}
}

// TestExistingAgentsLevelTwo: 000077 raises the agents that exist to
// autonomy level 2, the writes their permissions were given for; a new
// profile still starts at 0.
func TestExistingAgentsLevelTwo(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	migrateBefore(t, conn, 77)
	tenant, human, agent, other := id(), id(), id(), id()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Acme', 'eu-west', 'EUR', 'active')", tenant)
	for _, a := range []uuid.UUID{agent, other} {
		exec("INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1,$2,'agent','Agent','active',$3,$3)", tenant, a, human)
	}
	exec("INSERT INTO identity.agent_profile (tenant_id, principal_id, model, description, created_by, updated_by) VALUES ($1,$2,'erp-default','d',$3,$3)", tenant, agent, human)
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO identity.agent_profile (tenant_id, principal_id, model, description, created_by, updated_by) VALUES ($1,$2,'erp-default','d',$3,$3)", tenant, other, human)
	for a, want := range map[uuid.UUID]int{agent: 2, other: 0} {
		var got int
		if err := conn.QueryRow(ctx, "SELECT autonomy_level FROM identity.agent_profile WHERE principal_id = $1", a).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("level of %s: got %d, want %d", a, got, want)
		}
	}
}

// TestSpecialGLItems: a down payment is a supplier line with special G/L
// indicator A on the legal entity's down payment account (ADR-0037, T46a):
// a line is on that account exactly when it carries the indicator, the
// indicator needs a partner and is A, and the account is fixed once
// determined.
func TestSpecialGLItems(t *testing.T) {
	need(t)
	f := newFixture(t)
	dp := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
			VALUES ($1, $2, $3, '159000', 'Down payments made', 'asset', 'supplier', 'active', $4, $4)`, f.tenant, dp, f.chart, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`,
			f.tenant, f.entity, dp, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'down_payment', $3, $4)`,
			f.tenant, f.entity, dp, f.actor)
	})
	post := func(account uuid.UUID, partner *uuid.UUID, special any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			je := id()
			if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
				document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
				VALUES ($1,2026,$2,$3,$4,4,'ZP',$5,'2026-04-02','2026-04-02','USD','USD','EUR',1,0.9,'payments',$6)`,
				f.tenant, je, f.entity, f.ledger, je.String()[24:], f.actor); err != nil {
				return err
			}
			if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
				posting_date, account_id, amount_tc, amount_lc, amount_gc, partner_id, special_gl) VALUES ($1,2026,$2,1,$3,$4,4,'2026-04-02',$5,100,100,90,$6,$7)`,
				f.tenant, je, f.entity, f.ledger, account, partner, special); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
				posting_date, account_id, amount_tc, amount_lc, amount_gc) VALUES ($1,2026,$2,2,$3,$4,4,'2026-04-02',$5,-100,-100,-90)`,
				f.tenant, je, f.entity, f.ledger, f.bank)
		}
	}
	for _, c := range []struct {
		name    string
		account uuid.UUID
		partner *uuid.UUID
		special any
		fails   string
	}{
		{"a down payment", dp, &f.supplier, "A", ""},
		{"the down payment account without the indicator", dp, &f.supplier, nil, "journal_line_special_gl_account"},
		{"the indicator on the payables account", f.ap, &f.supplier, "A", "journal_line_special_gl_account"},
		{"the indicator without a partner", f.expense, nil, "A", "journal_line_special_gl"},
		{"another indicator", dp, &f.supplier, "B", "journal_line_special_gl"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, post(c.account, c.partner, c.special))
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	t.Run("the down payment account changed", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.account_determination SET account_id = $2, version = version + 1 WHERE tenant_id = $1 AND key = 'down_payment'`, f.tenant, f.ap)
		}), "account_determination_down_payment_fixed")
	})
}

// TestDownPaymentRequests: a down payment request (ADR-0037, T46b) is open
// first, moves only from open to cancelled with a reason (an open request
// is not edited), is for more than
// zero, carries tax only with a tax code and below its amount, and what was
// requested never changes.
func TestDownPaymentRequests(t *testing.T) {
	need(t)
	f := newFixture(t)
	insert := func(r uuid.UUID, amount int, taxCode any, tax int, status string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, amount,
				tax_code, tax_amount, due_date, status, created_by, updated_by) VALUES ($1, $2, gen_random_uuid()::text, $3, $4, 'USD', $5, $6, $7, '2026-04-02', $8, $9, $9)`,
				f.tenant, r, f.entity, f.supplier, amount, taxCode, tax, status, f.actor)
		}
	}
	open := id()
	mustApp(t, f.tenant, insert(open, 11900, "V1", 1900, "open"))
	for _, c := range []struct {
		name   string
		insert func(tx pgx.Tx) error
		fails  string
	}{
		{"zero", insert(id(), 0, nil, 0, "open"), "down_payment_request_amount_check"},
		{"tax without a code", insert(id(), 100, nil, 10, "open"), "down_payment_request_tax"},
		{"tax of the whole amount", insert(id(), 100, "V1", 100, "open"), "down_payment_request_tax"},
		{"cancelled first", insert(id(), 100, nil, 0, "cancelled"), "down_payment_request_lifecycle"},
		{"a number twice", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, amount,
				due_date, created_by, updated_by) SELECT $1, $2, document_number, legal_entity_id, supplier_id, currency, 100, due_date, $3, $3
				FROM payables.down_payment_request WHERE tenant_id = $1 AND id = $4`, f.tenant, id(), f.actor, open)
		}, "down_payment_request_number"},
		{"blank reference", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, amount,
				due_date, reference, created_by, updated_by) VALUES ($1, $2, gen_random_uuid()::text, $3, $4, 'USD', 100, '2026-04-02', '  ', $5, $5)`,
				f.tenant, id(), f.entity, f.supplier, f.actor)
		}, "down_payment_request_reference_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.insert), c.fails) })
	}
	update := func(set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE payables.down_payment_request SET `+set+`, version = version + 1 WHERE tenant_id = $1 AND id = $2`, f.tenant, open)
		}
	}
	for _, c := range []struct{ name, set, fails string }{
		{"changed while open", "amount = 100", "down_payment_request_lifecycle"},
		{"amount changed on cancelling", "status = 'cancelled', cancel_reason = 'x', amount = 100", "down_payment_request_fixed"},
		{"number changed on cancelling", "status = 'cancelled', cancel_reason = 'x', document_number = 'X'", "down_payment_request_fixed"},
		{"cancelled without a reason", "status = 'cancelled'", "down_payment_request_cancel_reason"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, update(c.set)), c.fails) })
	}
	mustApp(t, f.tenant, update("status = 'cancelled', cancel_reason = 'x'"))
	t.Run("reopened", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'open', cancel_reason = NULL")), "down_payment_request_lifecycle")
	})
}

// TestDownPaymentPayments: a payment pays a supplier's items, at most one
// such payment per supplier and run, or one down payment request, named
// exactly when it is of kind down_payment and paid once per run; a paid
// request names its entry, and only an open one is paid (T46c, ADR-0037).
func TestDownPaymentPayments(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run, request := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,$4,'2026-07-31','2026-07-31','USD',300,'proposed',$5,$5,$5)`, f.tenant, run, f.entity, hb, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, amount, due_date,
			created_by, updated_by) VALUES ($1, $2, 'DP1', $3, $4, 'USD', 100, '2026-07-31', $5, $5)`, f.tenant, request, f.entity, f.supplier, f.actor)
	})
	pay := func(kind string, req any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by,
				kind, down_payment_request_id) VALUES ($1, $2, $3, $4, $5, 100, 'USD', left(md5(random()::text), 20), $6, $7, $8)`, f.tenant, id(), run, f.supplier, id(), f.actor, kind, req)
		}
	}
	mustApp(t, f.tenant, pay("items", nil))
	mustApp(t, f.tenant, pay("down_payment", request))
	for _, c := range []struct {
		name   string
		insert func(tx pgx.Tx) error
		fails  string
	}{
		{"a second items payment", pay("items", nil), "payment_items_once"},
		{"the request twice", pay("down_payment", request), "payment_request_once"},
		{"a down payment without its request", pay("down_payment", nil), "payment_down_payment_request"},
		{"items naming a request", pay("items", id()), "payment_down_payment_request"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.insert), c.fails) })
	}
	update := func(set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE payables.down_payment_request SET `+set+`, version = version + 1 WHERE tenant_id = $1 AND id = $2`, f.tenant, request)
		}
	}
	t.Run("paid without its entry", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'paid'")), "down_payment_request_paid")
	})
	t.Run("an entry without its year", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'paid', journal_entry_id = gen_random_uuid()")), "down_payment_request_paid")
	})
	mustApp(t, f.tenant, update("status = 'paid', journal_entry_year = 2026, journal_entry_id = gen_random_uuid()"))
	t.Run("cancelled once paid", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'cancelled', cancel_reason = 'x'")), "down_payment_request_lifecycle")
	})
	// T46d: a cleared request names the invoice and the transfer, and its
	// payment stays its payment.
	t.Run("cleared without the transfer", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'cleared'")), "down_payment_request_cleared")
	})
	t.Run("cleared against an unknown invoice", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'cleared', cleared_invoice_id = gen_random_uuid(), clearing_entry_year = 2026, clearing_entry_id = gen_random_uuid()")),
			"down_payment_request_invoice")
	})
	t.Run("the payment changed", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("journal_entry_id = gen_random_uuid()")), "down_payment_request_lifecycle")
	})
	t.Run("the payment changed on clearing", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, update("status = 'cleared', cleared_invoice_id = gen_random_uuid(), clearing_entry_year = 2026, clearing_entry_id = gen_random_uuid(), "+
			"journal_entry_id = gen_random_uuid()")), "down_payment_request_fixed")
	})
}

// TestWithholdingTaxCodes: a legal entity's withholding tax code has a
// code of 1-4 upper-case letters or digits, a rate above 0 and below 100,
// a base of net or gross, a posting at invoice or payment and an account
// of the legal entity; a supplier's code belongs to its company data
// (T46e).
func TestWithholdingTaxCodes(t *testing.T) {
	need(t)
	f := newFixture(t)
	code := func(c string, rate int, base, posting string, account uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.withholding_tax_code (tenant_id, legal_entity_id, code, description, rate_pct, base, account_id, posting,
				created_by, updated_by) VALUES ($1, $2, $3, 'WHT', $4, $5, $6, $7, $8, $8)`, f.tenant, f.entity, c, rate, base, account, posting, f.actor)
		}
	}
	mustApp(t, f.tenant, code("W1", 10, "net", "invoice", f.ap))
	for _, c := range []struct {
		name   string
		insert func(tx pgx.Tx) error
		fails  string
	}{
		{"a lower-case code", code("w2", 10, "net", "invoice", f.ap), "withholding_tax_code_code_check"},
		{"a rate of 0", code("W2", 0, "net", "invoice", f.ap), "withholding_tax_code_rate_pct_check"},
		{"a rate of 100", code("W2", 100, "net", "invoice", f.ap), "withholding_tax_code_rate_pct_check"},
		{"another base", code("W2", 10, "tax", "invoice", f.ap), "withholding_tax_code_base_check"},
		{"another posting", code("W2", 10, "net", "order", f.ap), "withholding_tax_code_posting_check"},
		{"an account of no company", code("W2", 10, "net", "invoice", id()), "withholding_tax_code_tenant_id_legal_entity_id_account_id_fkey"},
		{"a supplier without company data", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO partner.supplier_withholding (tenant_id, partner_id, legal_entity_id, code, created_by, updated_by)
				VALUES ($1, $2, $3, 'W1', $4, $4)`, f.tenant, id(), f.entity, f.actor)
		}, "supplier_withholding_tenant_id_partner_id_legal_entity_id_fkey"},
		{"a supplier's lower-case code", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO partner.supplier_withholding (tenant_id, partner_id, legal_entity_id, code, created_by, updated_by)
				VALUES ($1, $2, $3, 'w1', $4, $4)`, f.tenant, id(), f.entity, f.actor)
		}, "supplier_withholding_code_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.insert), c.fails) })
	}
}

// TestPaymentMethodSchema: a payment method's code is one upper-case
// letter or digit; a ranking belongs to a method of its legal entity, in a
// known currency, and ranks at least one house bank; a supplier has up to
// ten methods, each one upper-case letter or digit (ADR-0038).
func TestPaymentMethodSchema(t *testing.T) {
	need(t)
	f := newFixture(t)
	method := func(code string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, created_by, updated_by)
				VALUES ($1, $2, $3, 'M', $4, $4)`, f.tenant, f.entity, code, f.actor)
		}
	}
	ranking := func(m, currency string, banks []string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.bank_ranking (tenant_id, legal_entity_id, method, currency, house_banks, created_by, updated_by)
				VALUES ($1, $2, $3, $4, $5, $6, $6)`, f.tenant, f.entity, m, currency, banks, f.actor)
		}
	}
	methods := func(supplier uuid.UUID, list string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code,
				payment_methods, created_by, updated_by) VALUES ($1, $2, $3, $4, 'NT30', `+list+`, $5, $5)`, f.tenant, supplier, f.entity, f.ap, f.actor)
		}
	}
	mustApp(t, f.tenant, method("T"))
	mustApp(t, f.tenant, ranking("T", "USD", []string{"MAIN", "HSBC"}))
	mustApp(t, f.tenant, methods(f.supplier, `ARRAY['T', 'U']`))
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"a two-character method", method("TT"), "payment_method_code_check"},
		{"a lower-case method", method("t"), "payment_method_code_check"},
		{"a ranking of no method", ranking("U", "USD", []string{"MAIN"}), "bank_ranking_tenant_id_legal_entity_id_method_fkey"},
		{"a ranking in an unknown currency", ranking("T", "XXY", []string{"MAIN"}), "bank_ranking_currency_fkey"},
		{"a ranking of no bank", ranking("T", "EUR", []string{}), "bank_ranking_house_banks_check"},
		{"a supplier's lower-case method", methods(f.otherSupplier, `ARRAY['t']`), "supplier_company_payment_methods"},
		{"a supplier's empty method", methods(f.otherSupplier, `ARRAY['']`), "supplier_company_payment_methods"},
		{"a supplier's null method", methods(f.otherSupplier, `ARRAY['T', NULL]`), "supplier_company_payment_methods"},
		{"eleven methods", methods(f.otherSupplier, `string_to_array('A,B,C,D,E,F,G,H,I,J,K', ',')`), "supplier_company_payment_methods"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
}

// TestOpenItemWithholding: an open item keeps its withholding taxes
// (T46f), one row per code: the amount withheld at invoice, of the base's
// sign and less than it, and none for a code deducted at payment; the rows
// are fixed. A payment item's withholding is of its sign and leaves it
// paying something after its discount, fixed like the rest of the item.
func TestOpenItemWithholding(t *testing.T) {
	need(t)
	f := newFixture(t)
	var je uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		je, err = f.entry(tx, 4, "2026-04-02", line{account: f.expense, amount: 500}, line{account: f.ap, amount: -500, partner: &f.supplier})
		return err
	})
	row := func(code, posting string, base int, amount *int, lineNo int) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.open_item_withholding (tenant_id, fiscal_year, journal_entry_id, line_no, code, posting, rate_pct, base_amount,
				amount, account_id) VALUES ($1, 2026, $2, $3, $4, $5, 10, $6, $7, $8)`, f.tenant, je, lineNo, code, posting, base, amount, f.ap)
		}
	}
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name, code, posting string
		base                int
		amount              *int
		line                int
		fails               string
	}{
		{"at invoice", "W1", "invoice", 500, n(50), 2, ""},
		{"at payment", "W2", "payment", 500, nil, 2, ""},
		{"a credit memo's at invoice", "W3", "invoice", -500, n(-50), 2, ""},
		{"at invoice without an amount", "W4", "invoice", 500, nil, 2, "open_item_withholding_amount"},
		{"at payment with an amount", "W4", "payment", 500, n(50), 2, "open_item_withholding_amount"},
		{"of the other sign", "W4", "invoice", 500, n(-50), 2, "open_item_withholding_amount"},
		{"the whole base", "W4", "invoice", 500, n(500), 2, "open_item_withholding_amount"},
		{"another moment", "W4", "clearing", 500, nil, 2, "open_item_withholding_posting_check"},
		{"of no line", "W4", "payment", 500, nil, 9, "open_item_withholding_line_fkey"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, row(c.code, c.posting, c.base, c.amount, c.line))
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	t.Run("fixed", func(t *testing.T) {
		_, err := testDB.Exec(context.Background(), "UPDATE ledger.open_item_withholding SET base_amount = 1 WHERE tenant_id = $1", f.tenant)
		failsWith(t, err, "insert-only")
	})
	hb, run, pay := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,$4,'2026-07-31','2026-07-31','USD',250,'proposed',$5,$5,$5)`, f.tenant, run, f.entity, hb, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, withholding_amount, currency, end_to_end_id, created_by)
			VALUES ($1,$2,$3,$4,$5,250,50,'USD','E2E-1',$6)`, f.tenant, pay, run, f.supplier, id(), f.actor)
	})
	for _, c := range []struct {
		name                          string
		amount, discount, withholding int
		fails                         string
	}{
		{"an invoice's withholding", 300, 6, 45, ""},
		{"a credit memo's withholding", -50, 0, -7, ""},
		{"of the other sign", 300, 0, -45, "payment_item_withholding_within"},
		{"with the discount, the whole amount", 300, 100, 200, "payment_item_withholding_within"},
	} {
		t.Run("payment item: "+c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, discount_amount,
					withholding_amount, due_date) VALUES ($1, $2, 2026, $3, 2, $4, $5, $6, '2026-07-31')`, f.tenant, pay, id(), c.amount, c.discount, c.withholding)
			})
			if c.fails == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want success", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	t.Run("payment item: a withholding changed", func(t *testing.T) {
		_, err := testDB.Exec(context.Background(), "UPDATE payments.payment_item SET withholding_amount = 1 WHERE tenant_id = $1 AND withholding_amount = 45", f.tenant)
		failsWith(t, err, "only be deactivated")
	})
}

// TestAgentCertification: certifications are platform reference data the
// application reads and never writes, of level 1 or 2 with the report's
// SHA-256, standing once loaded; an agent's type is a lower-case name
// (ADR-0039).
func TestAgentCertification(t *testing.T) {
	need(t)
	ctx := context.Background()
	model := "test-" + id().String() // certifications are global; each run certifies a model of its own
	hash := strings.Repeat("0f", 32)
	insert := func(level int, report string) error {
		_, err := testDB.Exec(ctx, `INSERT INTO ref.agent_certification (agent_type, model, level, suite_version, report_sha256, certified_by)
			VALUES ('test', $1, $2, 1, $3, 'ops')`, model, level, report)
		return err
	}
	if err := insert(2, hash); err != nil {
		t.Fatal(err)
	}
	t.Run("level 1 or 2", func(t *testing.T) {
		failsWith(t, insert(3, strings.Repeat("1f", 32)), "agent_certification_level_check")
	})
	t.Run("a SHA-256", func(t *testing.T) { failsWith(t, insert(1, "abc"), "agent_certification_report_check") })
	t.Run("a certification stands", func(t *testing.T) {
		_, err := testDB.Exec(ctx, "UPDATE ref.agent_certification SET level = 1 WHERE model = $1", model)
		failsWith(t, err, "insert-only")
	})
	a := newFixture(t)
	t.Run("the application reads, never writes", func(t *testing.T) {
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, "SELECT count(*) FROM ref.agent_certification WHERE model = $1", model).Scan(&n)
		})
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ref.agent_certification (agent_type, model, level, suite_version, report_sha256, certified_by)
				VALUES ('test', $1, 1, 1, $2, 'app')`, model, strings.Repeat("2f", 32))
		}), "permission denied")
	})
	t.Run("an agent's type", func(t *testing.T) {
		agent := id()
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, "INSERT INTO identity.principal (tenant_id, id, kind, display_name, status, created_by, updated_by) VALUES ($1,$2,'agent','A','active',$3,$3)", a.tenant, agent, a.actor); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO identity.agent_profile (tenant_id, principal_id, agent_type, model, description, created_by, updated_by)
				VALUES ($1,$2,'AP Exceptions','erp-default','d',$3,$3)`, a.tenant, agent, a.actor)
		}), "agent_profile_agent_type_check")
	})
}

// TestBankStatementSchema: a statement is imported once per house bank and
// key and once per file position, names its kept file, its dates in order,
// a gap only after a previous statement; a line belongs to a statement and keeps its details
// a list; a match is live once per line, names its rule exactly when a
// rule made it, is excluded only by a person, and each target is a payment
// (with its bank file) or a posting rule (T49c, T49e, ADR-0047).
func TestBankStatementSchema(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, first := id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor)
	})
	type stmt struct {
		key, format, status string
		position            int
		sum                 string
		closing             string
		previous            *uuid.UUID
		ref                 string
	}
	statement := func(s stmt) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position,
					statement_key, statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date, line_count,
					previous_statement_id, status, created_by, updated_by, file_ref)
				VALUES ($1,$2,$3,$4,$5,decode($6, 'hex'),$7,$8,'S','1','GB33','USD',0,'2026-10-20',0,$9::date,0,$10,$11,$12,$12,$13)`,
				f.tenant, id(), hb, f.entity, s.format, s.sum, s.position, s.key, s.closing, s.previous, s.status, f.actor, s.ref)
		}
	}
	ok := stmt{key: "K1", format: "mt940", status: "imported", position: 1, sum: strings.Repeat("ab", 32), closing: "2026-10-20", ref: "tenants/t/bank-statements/ab"}
	with := func(change func(*stmt)) func(tx pgx.Tx) error {
		s := ok
		change(&s)
		return statement(s)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position,
				statement_key, statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date, line_count, status, created_by, updated_by, file_ref)
			VALUES ($1,$2,$3,$4,'mt940',decode(repeat('ab', 32), 'hex'),1,'K1','S','1','GB33','USD',0,'2026-10-20',0,'2026-10-20',1,'imported',$5,$5,'tenants/t/bank-statements/ab')`,
			f.tenant, first, hb, f.entity, f.actor)
	})
	line := func(statement uuid.UUID, no int, details string, status ...string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code,
					bank_reference, owner_reference, text, details, status, created_by, updated_by)
				VALUES ($1,$2,$3,-100,'2026-10-20','2026-10-20','NTRF','B','O','T',$4::jsonb,$5,$6,$6)`, f.tenant, statement, no, details,
				append(status, "unmatched")[0], f.actor)
		}
	}
	mustApp(t, f.tenant, line(first, 1, `[]`))
	type match struct {
		kind, status string
		rule         any
	}
	matchOf := func(m match) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by)
				VALUES ($1,$2,$3,1,$4,$5,'r',$6,$7,$7)`, f.tenant, id(), first, m.kind, m.rule, m.status, f.actor)
		}
	}
	live := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by)
			VALUES ($1,$2,$3,1,'rule','amount','two payments','open',$4,$4)`, f.tenant, live, first, f.actor)
	})
	target := func(candidate int, payment, file, rule any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, payment_id, bank_file_id, posting_rule, created_by)
				VALUES ($1,$2,$3,1,$4,$5,$6,$7)`, f.tenant, live, candidate, payment, file, rule, f.actor)
		}
	}
	mustApp(t, f.tenant, target(1, nil, nil, "NCHG"))
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"the same key", with(func(s *stmt) { s.position = 2; s.sum = strings.Repeat("cd", 32) }), "bank_statement_key_once"},
		{"the same file position", with(func(s *stmt) { s.key = "K2" }), "bank_statement_file_once"},
		{"another format", with(func(s *stmt) { s.key, s.format = "K2", "bai2" }), "bank_statement_format_check"},
		{"a short hash", with(func(s *stmt) { s.key, s.sum = "K2", "ab" }), "bank_statement_file_sha256_check"},
		{"position 0", with(func(s *stmt) { s.key, s.position = "K2", 0 }), "bank_statement_file_position_check"},
		{"closing before opening", with(func(s *stmt) { s.key, s.position, s.closing = "K2", 2, "2026-10-19" }), "bank_statement_dates"},
		{"a gap without a previous statement", with(func(s *stmt) { s.key, s.position, s.status = "K2", 2, "gap" }), "bank_statement_gap"},
		{"another status", with(func(s *stmt) { s.key, s.position, s.status = "K2", 2, "posted" }), "bank_statement_status_check"},
		{"no file", with(func(s *stmt) { s.key, s.position, s.ref = "K2", 2, "" }), "bank_statement_file_ref_check"},
		{"an unknown previous statement", with(func(s *stmt) { s.key, s.position, s.previous = "K2", 2, &hb }), "bank_statement_tenant_id_previous_statement_id_fkey"},
		{"a line of no statement", line(hb, 1, `[]`), "bank_statement_line_tenant_id_statement_id_fkey"},
		{"line 0", line(first, 0, `[]`), "bank_statement_line_line_no_check"},
		{"details not a list", line(first, 2, `{}`), "bank_statement_line_details_check"},
		{"a second live match", matchOf(match{kind: "manual", status: "matched"}), "statement_match_live"},
		{"a rule's match without its rule", matchOf(match{kind: "rule", status: "rejected"}), "statement_match_rule"},
		{"a person's match naming a rule", matchOf(match{kind: "manual", status: "rejected", rule: "amount"}), "statement_match_rule"},
		{"another kind", matchOf(match{kind: "robot", status: "rejected"}), "statement_match_kind_check"},
		{"another rule", matchOf(match{kind: "rule", status: "rejected", rule: "iban"}), "statement_match_rule_check"},
		{"another status", matchOf(match{kind: "manual", status: "posted"}), "statement_match_status_check"},
		{"excluded by a rule", matchOf(match{kind: "rule", status: "excluded", rule: "amount"}), "statement_match_excluded"},
		{"a target twice", target(1, nil, nil, "NCHG"), "statement_match_target_pkey"},
		{"a target of nothing", target(2, nil, nil, nil), "statement_match_target_one"},
		{"a payment and a posting rule", target(2, id(), nil, "NCHG"), "statement_match_target_one"},
		{"a bank file without its payment", target(2, nil, id(), "NCHG"), "statement_match_target_one"},
		{"candidate 0", target(0, nil, nil, "NCHG"), "statement_match_target_candidate_check"},
		{"a payment that does not exist", target(2, id(), nil, nil), "statement_match_target_tenant_id_payment_id_fkey"},
		{"a line status of no match", line(first, 3, `[]`, "approved"), "bank_statement_line_status_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
	mustApp(t, f.tenant, with(func(s *stmt) { s.key, s.position, s.status, s.previous = "K2", 2, "gap", &first }))
	mustApp(t, f.tenant, line(first, 2, `[]`, "matched"))
	mustApp(t, f.tenant, line(first, 3, `[]`, "excluded"))
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE payments.statement_match SET status = 'rejected', version = version + 1 WHERE id = $1`, live)
	})
	mustApp(t, f.tenant, matchOf(match{kind: "manual", status: "excluded"}))
	statementPostingSchema(t, f, first)
	statementProposalSchema(t, f, first)
	statementReconciliationSchema(t, f, first, live)
}

// statementReconciliationSchema: a reconciled statement, and only one,
// says when and by whom; a target is one of a payment, a posting rule or
// the incoming payments clearing account (T49h, 000129).
func statementReconciliationSchema(t *testing.T, f fixture, statement, match uuid.UUID) {
	t.Helper()
	set := func(columns string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE payments.bank_statement SET `+columns+`, version = version + 1 WHERE id = $1`, append([]any{statement}, args...)...)
		}
	}
	target := func(candidate int, rule any, incoming bool) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, posting_rule, incoming, created_by)
				VALUES ($1,$2,$3,1,$4,$5,$6)`, f.tenant, match, candidate, rule, incoming, f.actor)
		}
	}
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"reconciled without when", set(`status = 'reconciled'`), "bank_statement_reconciled"},
		{"reconciled without whom", set(`status = 'reconciled', reconciled_at = now()`), "bank_statement_reconciled"},
		{"imported with when", set(`reconciled_at = now(), reconciled_by = $2`, f.actor), "bank_statement_reconciled"},
		{"incoming and a posting rule", target(5, "NCHG", true), "statement_match_target_one"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
	mustApp(t, f.tenant, set(`status = 'reconciled', reconciled_at = now(), reconciled_by = $2`, f.actor))
	mustApp(t, f.tenant, target(6, nil, true))
}

// statementProposalSchema: an agent's match, and only one, names its
// session and what it proposes, stays open until a person decides it,
// never posts, and records the person's outcome once decided; a session
// proposes once per line (T49g, 000127).
func statementProposalSchema(t *testing.T, f fixture, statement uuid.UUID) {
	t.Helper()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code,
				bank_reference, owner_reference, text, status, created_by, updated_by)
			VALUES ($1,$2,20,-100,'2026-10-20','2026-10-20','NTRF','B','O','T','proposed',$3,$3)`, f.tenant, statement, f.actor)
	})
	session := id()
	type proposal struct {
		kind, status string
		session      any
		proposal     any
		outcome      any
		postingDate  any
	}
	write := func(p proposal) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, reason, status, session_id, proposal, outcome,
					posting_date, created_by, updated_by)
				VALUES ($1,$2,$3,20,$4,'r',$5,$6,$7,$8,$9,$10,$10)`, f.tenant, id(), statement, p.kind, p.status, p.session, p.proposal, p.outcome, p.postingDate, f.actor)
		}
	}
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"an agent's match without its session", write(proposal{kind: "agent", status: "open", proposal: "match"}), "statement_match_agent"},
		{"an agent's match proposing nothing", write(proposal{kind: "agent", status: "open", session: session}), "statement_match_agent"},
		{"a person's match naming a session", write(proposal{kind: "manual", status: "rejected", session: session, proposal: "match"}), "statement_match_agent"},
		{"another proposal", write(proposal{kind: "agent", status: "open", session: session, proposal: "post"}), "statement_match_proposal_check"},
		{"an agent's match matched", write(proposal{kind: "agent", status: "matched", session: session, proposal: "match"}), "statement_match_agent_open"},
		{"an agent's match posting", write(proposal{kind: "agent", status: "open", session: session, proposal: "match", postingDate: "2026-10-20"}), "statement_match_agent_open"},
		{"an open proposal decided", write(proposal{kind: "agent", status: "open", session: session, proposal: "match", outcome: "accepted"}), "statement_match_outcome"},
		{"a proposal rejected undecided", write(proposal{kind: "agent", status: "rejected", session: session, proposal: "match"}), "statement_match_outcome"},
		{"a person's match decided", write(proposal{kind: "manual", status: "rejected", outcome: "accepted"}), "statement_match_outcome"},
		{"another outcome", write(proposal{kind: "agent", status: "rejected", session: session, proposal: "match", outcome: "approved"}), "statement_match_outcome_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
	mustApp(t, f.tenant, write(proposal{kind: "agent", status: "rejected", session: session, proposal: "exclude", outcome: "rejected"}))
	t.Run("a session proposing twice for a line", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, write(proposal{kind: "agent", status: "open", session: session, proposal: "match"})), "statement_match_session")
	})
	mustApp(t, f.tenant, write(proposal{kind: "agent", status: "open", session: id(), proposal: "match"}))
}

// statementPostingSchema: a match's posting has its year and date, a
// reason goes with a date, a posted match stays matched and a journal
// entry posts one match (T49f, 000126).
func statementPostingSchema(t *testing.T, f fixture, statement uuid.UUID) {
	t.Helper()
	matched := func(lineNo int, m uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code,
					bank_reference, owner_reference, text, status, created_by, updated_by)
				VALUES ($1,$2,$3,-100,'2026-10-20','2026-10-20','NTRF','B','O','T','matched',$4,$4)`, f.tenant, statement, lineNo, f.actor); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, reason, status, created_by, updated_by)
				VALUES ($1,$2,$3,$4,'manual','r','matched',$5,$5)`, f.tenant, m, statement, lineNo, f.actor)
		}
	}
	posted, other, entry := id(), id(), id()
	mustApp(t, f.tenant, matched(10, posted))
	mustApp(t, f.tenant, matched(11, other))
	set := func(m uuid.UUID, columns string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE payments.statement_match SET `+columns+`, version = version + 1 WHERE id = $1`, append([]any{m}, args...)...)
		}
	}
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"an entry without its year", set(posted, `posting_date = '2026-10-20', journal_entry_id = $2`, entry), "statement_match_journal"},
		{"an entry without its date", set(posted, `journal_entry_year = 2026, journal_entry_id = $2`, entry), "statement_match_posting_date"},
		{"a request without its date", set(posted, `approval_request_id = $2`, id()), "statement_match_posting_date"},
		{"a reason without a date", set(posted, `posting_reason = 'x'`), "statement_match_posting_reason"},
		{"an empty reason", set(posted, `posting_date = '2026-10-20', posting_reason = ''`), "statement_match_posting_reason_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
	mustApp(t, f.tenant, set(posted, `posting_date = '2026-10-21', posting_reason = 'October is locked', journal_entry_year = 2026, journal_entry_id = $2`, entry))
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"a posted match rejected", set(posted, `status = 'rejected'`), "statement_match_posted"},
		{"an entry posting two matches", set(other, `posting_date = '2026-10-20', journal_entry_year = 2026, journal_entry_id = $2`, entry), "statement_match_journal_entry"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
}

// TestStatementFetchSchema: a fetched file is recorded once per house bank
// and content, with its name, in a known status, and a reason exactly when
// refused or changed after its import (T49d, ADR-0047).
func TestStatementFetchSchema(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, active_gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor)
	})
	fetch := func(bank uuid.UUID, sum, name, status string, reason any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.statement_fetch (tenant_id, id, house_bank_id, file_sha256, file_name, status, reason, created_by, updated_by)
				VALUES ($1,$2,$3,decode($4, 'hex'),$5,$6,$7,$8,$8)`, f.tenant, id(), bank, sum, name, status, reason, f.actor)
		}
	}
	sum := strings.Repeat("ab", 32)
	mustApp(t, f.tenant, fetch(hb, sum, "a.sta", "imported", nil))
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"the same file again", fetch(hb, sum, "b.sta", "imported", nil), "statement_fetch_file_once"},
		{"a short hash", fetch(hb, "ab", "b.sta", "imported", nil), "statement_fetch_file_sha256_check"},
		{"no name", fetch(hb, strings.Repeat("cd", 32), "", "imported", nil), "statement_fetch_file_name_check"},
		{"another status", fetch(hb, strings.Repeat("cd", 32), "b.sta", "posted", nil), "statement_fetch_status_check"},
		{"refused without a reason", fetch(hb, strings.Repeat("cd", 32), "b.sta", "refused", nil), "statement_fetch_reason"},
		{"a reason without a refusal", fetch(hb, strings.Repeat("cd", 32), "b.sta", "archived", "x"), "statement_fetch_reason"},
		{"changed without a reason", fetch(hb, strings.Repeat("cd", 32), "b.sta", "changed_after_import", nil), "statement_fetch_reason"},
		{"no such house bank", fetch(id(), strings.Repeat("cd", 32), "b.sta", "imported", nil), "statement_fetch_tenant_id_house_bank_id_fkey"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.write), c.fails) })
	}
	mustApp(t, f.tenant, fetch(hb, strings.Repeat("cd", 32), "b.sta", "refused", "unreadable"))
	mustApp(t, f.tenant, fetch(hb, strings.Repeat("ef", 32), "c.sta", "changed_after_import", "now "+strings.Repeat("12", 32)))
}
