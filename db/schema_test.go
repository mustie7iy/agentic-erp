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
	if err := applyAll(ctx, testDB, true); err != nil {
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
	tenantSchemas := `('platform','tenancy','identity','config','partner','ledger','procurement','payables','payments','workflow','audit','integration','agents','analytics')`

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
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
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
	if err := release(id()); err != nil {
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

// TestBankFileOncePerRun: a run's release writes one bank file; a second
// file for the run, from a release finished twice, is refused.
func TestBankFileOncePerRun(t *testing.T) {
	need(t)
	f := newFixture(t)
	hb, run, other := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
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
	file := func(r uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO payments.bank_file (tenant_id, id, payment_run_id, format, object_ref, sha256, status, created_by)
				VALUES ($1,$2,$3,'pain.001.001.03','sha256:x',decode(repeat('ab', 32), 'hex'),'created',$4)`, f.tenant, id(), r, f.actor)
		}
	}
	mustApp(t, f.tenant, file(run))
	failsWith(t, asApp(t, f.tenant, file(run)), "bank_file_once_per_run")
	mustApp(t, f.tenant, file(other))
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

func TestGoodsReceiptLimits(t *testing.T) {
	need(t)
	f := newFixture(t)
	po := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date, payment_term_code, status, created_by, updated_by)
			VALUES ($1,$2,'4500000001',$3,$4,'USD','2026-08-01','NT30','released',$5,$5)`, f.tenant, po, f.entity, f.supplier, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id, over_delivery_pct)
			VALUES ($1,$2,10,'Paper',10,'EA',250,$3,10)`, f.tenant, po, f.expense)
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
		if err := exec(tx, `INSERT INTO payments.house_bank (tenant_id, id, legal_entity_id, code, name, iban_cipher, iban_last4, bic, currency, gl_account_id, status, created_by, updated_by)
			VALUES ($1,$2,$3,'MAIN','Main','\x00','5555','BUKBGB22','USD',$4,'active',$5,$5)`, f.tenant, hb, f.entity, f.bank, f.actor); err != nil {
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
	mustApp(t, a.tenant, registerProvider(a, p, "https://idp.example.com/a", "erp"))

	t.Run("an active pair belongs to one tenant", func(t *testing.T) {
		failsWith(t, asApp(t, b.tenant, registerProvider(b, id(), "https://idp.example.com/a", "erp")), "identity_provider_issuer_audience")
	})
	t.Run("another audience is another registration", func(t *testing.T) {
		if err := asApp(t, b.tenant, registerProvider(b, id(), "https://idp.example.com/a", "erp-b")); err != nil {
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
	t.Run("retired_at goes with retired", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "status = 'retired'")), "identity_provider_retired_at")
	})
	t.Run("a retired registration frees its pair and stays retired", func(t *testing.T) {
		mustApp(t, a.tenant, changeProvider(a, p, "status = 'retired', retired_at = now()"))
		if err := asApp(t, b.tenant, registerProvider(b, id(), "https://idp.example.com/a", "erp")); err != nil {
			t.Errorf("the retired pair registered by another tenant: got %v, want registered", err)
		}
		failsWith(t, asApp(t, a.tenant, changeProvider(a, p, "acr_values = '{x}'")), "is retired")
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
// what decides the tenant and the policy, not who changed them.
func TestControlRoleReadsTheRegistry(t *testing.T) {
	need(t)
	f := newFixture(t)
	mustApp(t, f.tenant, registerProvider(f, id(), "https://idp.example.com/control", "erp"))
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
		{"providers", "SELECT tenant_id, id, issuer, audience, tenant_claim, acr_values, amr_values, status FROM identity.identity_provider", ""},
		{"policies", "SELECT tenant_id, mfa_required, session_max_age, idle_timeout FROM identity.auth_policy", ""},
		{"principals", "SELECT tenant_id, id, kind, external_issuer, external_subject, display_name, status, version FROM identity.principal", ""},
		{"who registered", "SELECT created_by FROM identity.identity_provider", "permission denied"},
		{"who changed the policy", "SELECT updated_by FROM identity.auth_policy", "permission denied"},
		{"principals' email", "SELECT email FROM identity.principal", "permission denied"},
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
