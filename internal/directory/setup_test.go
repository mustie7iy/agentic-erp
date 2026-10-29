package directory_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// logins is a directory set up in a database of its own with a router's,
// a cell's and erp-web's login, each opened as itself, and its owner's
// store beside them.
type logins struct {
	url                      string
	router, cell, web, owner *directory.Postgres
	creds                    map[string]pgtest.Login
}

func setUpLogins(t *testing.T) logins {
	t.Helper()
	ctx := context.Background()
	l := logins{url: pgtest.EmptyDatabase(t), creds: map[string]pgtest.Login{}}
	if err := directory.Setup(ctx, l.url, l.all(t)...); err != nil {
		t.Fatalf("setup: %v", err)
	}
	open := func(u string) *directory.Postgres {
		dir, err := directory.OpenWith(ctx, u, nil)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(dir.Close)
		return dir
	}
	l.owner = open(l.url)
	l.router, l.cell = open(l.creds[directory.RouterRole].As(l.url)), open(l.creds[directory.CellRole].As(l.url))
	// erp-web's login opens no directory store (OpenWith reads
	// directory.tenant, which it may not): a pool of its own.
	pool, err := database.Open(ctx, l.creds[directory.WebRole].As(l.url), database.DefaultSettings)
	if err != nil {
		t.Fatalf("open as erp-web: %v", err)
	}
	t.Cleanup(pool.Close)
	l.web = &directory.Postgres{Pool: pool}
	return l
}

// all names a login of the test's own for each role, the first time.
func (l logins) all(t *testing.T) []directory.Login {
	t.Helper()
	var all []directory.Login
	for _, role := range []string{directory.RouterRole, directory.CellRole, directory.WebRole} {
		c, ok := l.creds[role]
		if !ok {
			c = pgtest.NewLogin(t)
			l.creds[role] = c
		}
		all = append(all, directory.Login{Name: c.Name, Password: c.Password, Role: role})
	}
	return all
}

func exec(ctx context.Context, dir *directory.Postgres, scope database.Scope, sql string) error {
	return dir.Pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql)
		return err
	})
}

// TestTheCellsLoginDoesWhatACellDoes: a cell, as its login, records
// itself, its tenants and its identity providers' claims, reinstates an
// imported tenant and reads its tenants back (directory.Sync, erp-server).
func TestTheCellsLoginDoesWhatACellDoes(t *testing.T) {
	ctx := context.Background()
	l := setUpLogins(t)
	tenant := uuid.New()
	is := directory.Issuer{Issuer: "https://idp.example.com", Audience: "erp", TenantID: tenant, Cell: "eu-1", ProviderID: uuid.New(),
		Web: &directory.WebClient{LoginCode: "acme", ClientID: "erp-web", AuthMethod: "private_key_jwt", CredentialRef: "file:/k/acme.pem", Scope: "openid"}}
	cell := directory.Cell{Name: "eu-1", Region: "eu-west", APIURL: "https://eu-1", ControlURL: "https://eu-1:9090", Accepting: true}
	// In this order: each step builds on the one before.
	steps := []struct {
		name string
		op   func() error
	}{
		{"record the cell", func() error { return l.cell.UpsertCell(ctx, cell) }},
		{"record it again", func() error { cell.Accepting = false; return l.cell.UpsertCell(ctx, cell) }},
		{"record a tenant", func() error {
			return l.cell.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1", Status: directory.Active})
		}},
		{"change it", func() error {
			return l.cell.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1b", Status: directory.Offboarded})
		}},
		{"reinstate it", func() error { return l.cell.Reinstate(ctx, tenant, "eu-1") }},
		{"claim an issuer", func() error { return l.cell.ClaimIssuer(ctx, is) }},
		{"claim it again", func() error { return l.cell.ClaimIssuer(ctx, is) }},
		{"claim its web client", func() error { return l.cell.ClaimWebClient(ctx, is) }},
		{"sync the claims", func() error { _, err := l.cell.SyncIssuers(ctx, "eu-1", []directory.Issuer{is}); return err }},
		{"read the cell's tenants", func() error { _, err := l.cell.CellTenants(ctx, "eu-1"); return err }},
		{"look the tenant up", func() error { _, err := l.cell.Lookup(ctx, tenant); return err }},
		{"release the issuer", func() error { return l.cell.ReleaseIssuer(ctx, is) }},
	}
	for _, s := range steps {
		if err := s.op(); err != nil {
			t.Fatalf("%s: got %v, want it done", s.name, err)
		}
	}
	got, err := l.owner.Lookup(ctx, tenant)
	if err != nil || got.Status != directory.Active || got.APIURL != "https://eu-1b" {
		t.Errorf("the tenant: got %+v, %v, want it active at https://eu-1b", got, err)
	}
}

// TestTheRoutersLoginDoesWhatTheRouterDoes: the router, as its login,
// looks tenants and issuers up, and its global control plane lists
// tenants and cells, freezes a tenant, moves it and places a
// provisioning (internal/globalplane).
func TestTheRoutersLoginDoesWhatTheRouterDoes(t *testing.T) {
	ctx := context.Background()
	l := setUpLogins(t)
	tenant := uuid.New()
	for _, c := range []string{"eu-1", "eu-2"} {
		if err := l.cell.UpsertCell(ctx, directory.Cell{Name: c, Region: "eu-west", APIURL: "https://" + c, ControlURL: "https://" + c + ":9090", Accepting: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.cell.Upsert(ctx, directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1", Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	if err := l.cell.ClaimIssuer(ctx, directory.Issuer{Issuer: "https://idp.example.com", Audience: "erp", TenantID: tenant, Cell: "eu-1", ProviderID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		op   func() error
	}{
		{"look the tenant up", func() error { _, err := l.router.Lookup(ctx, tenant); return err }},
		{"resolve an issuer", func() error {
			_, err := directory.IssuerRegistry{Directory: l.router}.Resolve(ctx, "https://idp.example.com", []string{"erp"})
			return err
		}},
		{"list tenants", func() error { _, _, err := l.router.ListTenants(ctx, directory.Filter{}, mustKeyset(t)); return err }},
		{"list cells", func() error { _, err := l.router.ListCells(ctx, "eu-west"); return err }},
		{"read a cell's tenants", func() error { _, err := l.router.CellTenants(ctx, "eu-1"); return err }},
		{"freeze the tenant", func() error { _, err := l.router.SetStatus(ctx, tenant, 0, directory.ReadOnly); return err }},
		{"move it", func() error { _, err := l.router.Move(ctx, tenant, 0, "eu-2"); return err }},
		{"place a provisioning", func() error { _, err := l.router.Place(ctx, "key-1", "eu-west", ""); return err }},
		{"enter the provisioned tenant at once", func() error {
			return l.router.Upsert(ctx, directory.Entry{TenantID: uuid.New(), Region: "eu-west", Cell: "eu-1", APIURL: "https://eu-1", Status: directory.Active})
		}},
		{"place it again", func() error { _, err := l.router.Place(ctx, "key-1", "eu-west", ""); return err }},
	}
	for _, s := range steps {
		if err := s.op(); err != nil {
			t.Fatalf("%s: got %v, want it done", s.name, err)
		}
	}
	got, err := l.owner.Lookup(ctx, tenant)
	if err != nil || got.Cell != "eu-2" || got.Status != directory.ReadOnly {
		t.Errorf("the tenant: got %+v, %v, want it read-only in eu-2", got, err)
	}
}

// TestErpWebsLoginDoesWhatErpWebDoes: erp-web, as its login working as
// erp_web, keeps its sessions and reads the web clients
// (internal/web/session runs the store itself the same way).
func TestErpWebsLoginDoesWhatErpWebDoes(t *testing.T) {
	ctx := context.Background()
	l := setUpLogins(t)
	// As erp-web runs, working as erp_web, and as the login itself.
	for _, scope := range []database.Scope{{Role: directory.WebRole}, {}} {
		for _, sql := range []string{
			`INSERT INTO web.session (id_hash, tenant_id, issuer, subject, login_code, created_at, last_seen_at, expires_at, sealed)
				VALUES (sha256('id'), gen_random_uuid(), 'https://i', 's', 'c', now(), now(), now() + interval '1 hour', '\x00')`,
			`UPDATE web.session SET last_seen_at = now(), version = version + 1`,
			`SELECT * FROM web.session`,
			`SELECT * FROM web.login_client`,
			`DELETE FROM web.session`,
		} {
			if err := exec(ctx, l.web, scope, sql); err != nil {
				t.Errorf("as %q: %s: got %v, want it done", scope.Role, sql, err)
			}
		}
	}
}

// ddl is what no client's login may do to the directory's database:
// change its schema, make objects or roles, or work as another role.
var ddl = []string{
	`CREATE TABLE directory.mine (x int)`, `CREATE TABLE web.mine (x int)`, `CREATE TABLE public.mine (x int)`,
	`CREATE SCHEMA mine`, `ALTER TABLE directory.tenant ADD COLUMN mine int`, `DROP TABLE directory.placement`,
	`ALTER TABLE web.session ADD COLUMN mine int`, `DROP VIEW web.login_client`, `TRUNCATE directory.tenant`,
	`TRUNCATE web.session`, `CREATE TEMP TABLE mine (x int)`, `CREATE ROLE mine`, `GRANT erp_router TO CURRENT_USER`, `GRANT erp_cell TO CURRENT_USER`,
}

// TestLoginsMayDoNothingMore: each login is refused what its service
// never does: another's tables or columns, deletes, key changes and any
// DDL (T72h).
func TestLoginsMayDoNothingMore(t *testing.T) {
	ctx := context.Background()
	l := setUpLogins(t)
	refused := map[string][]string{
		directory.RouterRole: append([]string{
			`INSERT INTO directory.tenant (tenant_id, region, cell, api_url, status, version) VALUES (gen_random_uuid(), 'r', 'c', 'u', 'active', 9)`,
			`DELETE FROM directory.tenant`, `UPDATE directory.tenant SET tenant_id = gen_random_uuid()`,
			`INSERT INTO directory.cell (name, region, api_url, control_url) VALUES ('c', 'r', 'u', 'u')`, `UPDATE directory.cell SET accepting = false`,
			`DELETE FROM directory.cell`,
			`INSERT INTO directory.issuer (issuer, audience, tenant_id, cell) VALUES ('i', 'a', gen_random_uuid(), 'c')`,
			`UPDATE directory.issuer SET tenant_id = gen_random_uuid()`, `DELETE FROM directory.issuer`,
			`INSERT INTO directory.placement (idempotency_key, cell, created_at) VALUES ('k', 'c', now())`,
			`UPDATE directory.placement SET cell = 'c'`, `DELETE FROM directory.placement`,
			`SELECT FROM web.session`, `SELECT FROM web.login_client`, `SET ROLE erp_cell`, `SET ROLE erp_web`,
		}, ddl...),
		directory.CellRole: append([]string{
			`SELECT FROM directory.placement`, `INSERT INTO directory.placement (idempotency_key, cell) VALUES ('k', 'c')`,
			`DELETE FROM directory.tenant`, `UPDATE directory.tenant SET tenant_id = gen_random_uuid()`,
			`DELETE FROM directory.cell`, `UPDATE directory.cell SET name = 'c'`,
			`UPDATE directory.issuer SET tenant_id = gen_random_uuid()`, `UPDATE directory.issuer SET issuer = 'i'`,
			`SELECT FROM web.session`, `SELECT FROM web.login_client`, `SET ROLE erp_router`, `SET ROLE erp_web`,
		}, ddl...),
		directory.WebRole: append([]string{
			`SELECT FROM directory.tenant`, `SELECT FROM directory.cell`, `SELECT FROM directory.issuer`, `SELECT FROM directory.placement`,
			`INSERT INTO web.login_client (login_code) VALUES ('x')`, `DELETE FROM web.login_client`,
			`SET ROLE erp_router`, `SET ROLE erp_cell`,
		}, ddl...),
	}
	stores := map[string]*directory.Postgres{directory.RouterRole: l.router, directory.CellRole: l.cell, directory.WebRole: l.web}
	for role, sqls := range refused {
		for _, sql := range sqls {
			t.Run(role+": "+sql, func(t *testing.T) {
				err := exec(ctx, stores[role], database.Scope{}, sql)
				if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "42501" {
					t.Errorf("got %v, want it refused for want of the privilege (42501)", err)
				}
			})
		}
	}
}

// TestSetupAgain: a second run sets the logins' passwords again, so a
// changed one takes effect, as SCRAM-SHA-256 verifiers alone, and takes
// back the rights given by hand to a role or a login.
func TestSetupAgain(t *testing.T) {
	ctx := context.Background()
	l := setUpLogins(t)
	router, cell := pgx.Identifier{l.creds[directory.RouterRole].Name}.Sanitize(), pgx.Identifier{l.creds[directory.CellRole].Name}.Sanitize()
	db := dbName(t, l.url)
	for _, sql := range []string{`GRANT DELETE ON directory.tenant TO erp_router`, `GRANT TRUNCATE ON web.session TO erp_web`,
		`GRANT DELETE ON directory.cell TO ` + router, // A database's ACL is the server's (pg_shdepend's dbid 0): granted to
		// logins of the test's own alone, never to a privilege role, which
		// every other package's Setup would see meanwhile.
		`GRANT CREATE ON DATABASE ` + db + ` TO ` + router, `GRANT CREATE ON DATABASE ` + db + ` TO ` + cell,
		// To PUBLIC, every login's: a table, a schema, the database and schema public.
		`GRANT DELETE ON directory.placement TO PUBLIC`, `GRANT USAGE ON SCHEMA directory TO PUBLIC`, `GRANT SELECT ON directory.issuer TO PUBLIC`,
		`GRANT CREATE ON DATABASE ` + db + ` TO PUBLIC`, `GRANT CREATE ON SCHEMA public TO PUBLIC`} {
		if err := exec(ctx, l.owner, database.Scope{}, sql); err != nil {
			t.Fatal(err)
		}
	}
	old := l.creds[directory.CellRole]
	changed := pgtest.Login{Name: old.Name, Password: uuid.NewString()}
	l.creds[directory.CellRole] = changed
	if err := directory.Setup(ctx, l.url, l.all(t)...); err != nil {
		t.Fatalf("again: %v", err)
	}
	for _, c := range []struct {
		dir *directory.Postgres
		sql string
	}{{l.router, `DELETE FROM directory.tenant`}, {l.web, `TRUNCATE web.session`}, {l.router, `DELETE FROM directory.cell`},
		{l.router, `CREATE SCHEMA mine`}, {l.cell, `CREATE SCHEMA mine`},
		{l.router, `DELETE FROM directory.placement`}, {l.web, `SELECT FROM directory.issuer`}, {l.web, `CREATE SCHEMA mine`},
		{l.web, `CREATE TABLE public.mine (x int)`}} {
		err := exec(ctx, c.dir, database.Scope{}, c.sql)
		if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "42501" {
			t.Errorf("%s, given by hand: got %v, want it taken back (42501)", c.sql, err)
		}
	}
	dir, err := directory.OpenWith(ctx, old.As(l.url), nil)
	if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "28P01" {
		if dir != nil {
			dir.Close()
		}
		t.Errorf("the old password: got %v, want it refused (28P01)", err)
	}
	if dir, err = directory.OpenWith(ctx, changed.As(l.url), nil); err != nil {
		t.Fatalf("the new password: got %v, want it taken", err)
	}
	dir.Close()
	var stored string
	pgtest.Scan(t, `SELECT rolpassword FROM pg_authid WHERE rolname = $1`, []any{changed.Name}, &stored)
	if !strings.HasPrefix(stored, "SCRAM-SHA-256$4096:") || strings.Contains(stored, changed.Password) {
		t.Errorf("stored: got %q, want a SCRAM-SHA-256 verifier", stored)
	}
}

// TestSetupRefusesLogins: a login is made least privileged or not at all.
func TestSetupRefusesLogins(t *testing.T) {
	ctx := context.Background()
	// Made before the database, so dropped after it: one owns a table there.
	createdb, member, admin, owner := pgtest.NewLogin(t), pgtest.NewLogin(t), pgtest.NewLogin(t), pgtest.NewLogin(t)
	u := pgtest.EmptyDatabase(t)
	var me string
	pgtest.Scan(t, `SELECT current_user`, nil, &me)
	id := func(l pgtest.Login) string { return pgx.Identifier{l.Name}.Sanitize() }
	pgtest.Exec(t, `CREATE ROLE `+id(createdb)+` LOGIN CREATEDB; CREATE ROLE `+id(member)+` LOGIN; CREATE ROLE `+id(admin)+` LOGIN; CREATE ROLE `+id(owner)+` LOGIN`)
	if err := directory.Setup(ctx, u); err != nil { // the roles exist
		t.Fatal(err)
	}
	dir, err := directory.OpenWith(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := exec(ctx, dir, database.Scope{}, `CREATE TABLE public.owned (x int); ALTER TABLE public.owned OWNER TO `+id(owner)); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `GRANT erp_cell TO `+id(member)+`; GRANT erp_cell TO `+id(admin)+` WITH ADMIN OPTION`)
	for name, c := range map[string]directory.Login{
		"no name":                        {Password: "p", Role: directory.CellRole},
		"no password":                    {Name: "erp_x_login", Role: directory.CellRole},
		"another role":                   {Name: "erp_x_login", Password: "p", Role: "erp_app"},
		"a privilege role's name":        {Name: directory.WebRole, Password: "p", Role: directory.CellRole},
		"a password SASLprep may change": {Name: "erp_x_login", Password: "pässword", Role: directory.CellRole},
		"the owner":                      {Name: me, Password: "p", Role: directory.CellRole},
		"a right to create":              {Name: createdb.Name, Password: "p", Role: directory.CellRole},
		"a member of another":            {Name: member.Name, Password: "p", Role: directory.RouterRole},
		"able to grant its role":         {Name: admin.Name, Password: "p", Role: directory.CellRole},
		"owning objects":                 {Name: owner.Name, Password: "p", Role: directory.CellRole},
	} {
		t.Run(name, func(t *testing.T) {
			if err := directory.Setup(ctx, u, c); !errors.Is(err, directory.ErrLogin) {
				t.Errorf("got %v, want ErrLogin", err)
			}
		})
	}
}

// TestSetupRefusesOwners: a login owning the database, a privilege role
// owning an object in it, either holding a right on an object Grants
// does not name, or PUBLIC holding one on an object kind pg_shdepend does
// not record it for, is refused; each in a database of its own, which the
// refusal leaves unusable.
func TestSetupRefusesOwners(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct{ sql, want string }{
		"a login owning the database":     {`ALTER DATABASE {db} OWNER TO {login}`, "owned or granted [{login}: database"},
		"a privilege role owning a table": {`CREATE TABLE public.owned (x int); ALTER TABLE public.owned OWNER TO erp_cell`, "erp_cell: table owned (o)"},
		"a stray right of a role":         {`CREATE TABLE public.stray (x int); GRANT SELECT ON public.stray TO erp_cell`, "erp_cell: table stray (a)"},
		"a stray right of a login":        {`CREATE TABLE public.stray (x int); GRANT SELECT ON public.stray TO {login}`, "{login}: table stray (a)"},
		"a schema made by hand":           {`CREATE SCHEMA ops`, "other schemas [ops]"},
		// PUBLIC's, every login's, which pg_shdepend does not record.
		"a large object PUBLIC may read": {`DO $$ BEGIN EXECUTE format('GRANT SELECT ON LARGE OBJECT %s TO PUBLIC', lo_create(0)); END $$`,
			"PUBLIC's rights [large object "},
		"a foreign-data wrapper PUBLIC may use": {`CREATE FOREIGN DATA WRAPPER mine; GRANT USAGE ON FOREIGN DATA WRAPPER mine TO PUBLIC`,
			"PUBLIC's rights [foreign-data wrapper mine]"},
		"a foreign server PUBLIC may use": {`CREATE FOREIGN DATA WRAPPER mine; CREATE SERVER mine FOREIGN DATA WRAPPER mine; GRANT USAGE ON FOREIGN SERVER mine TO PUBLIC`,
			"PUBLIC's rights [foreign server mine]"},
		"a language PUBLIC may use": {`CREATE TRUSTED LANGUAGE plmine HANDLER plpgsql_call_handler; REVOKE USAGE ON LANGUAGE plmine FROM PUBLIC;
			GRANT USAGE ON LANGUAGE plmine TO PUBLIC`, "PUBLIC's rights [language plmine]"},
		"large objects unchecked for the login": {`ALTER ROLE {login} SET lo_compat_privileges = on`,
			"PUBLIC's rights [large objects unchecked (lo_compat_privileges set for {login} in every database)]"},
		"large objects unchecked in the database": {`ALTER DATABASE {db} SET lo_compat_privileges = on`,
			"large objects unchecked (lo_compat_privileges set for every role in this database)"},
		"a trusted language PUBLIC uses by default": {`CREATE TRUSTED LANGUAGE plmine HANDLER plpgsql_call_handler`, "PUBLIC's rights [language plmine]"},
	} {
		t.Run(name, func(t *testing.T) {
			login := pgtest.NewLogin(t) // before the database, so dropped after it
			u := pgtest.EmptyDatabase(t)
			pgtest.Exec(t, `CREATE ROLE `+pgx.Identifier{login.Name}.Sanitize()+` LOGIN`)
			dir, err := directory.Open(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			sql := strings.NewReplacer("{db}", dbName(t, u), "{login}", pgx.Identifier{login.Name}.Sanitize()).Replace(c.sql)
			if err := exec(ctx, dir, database.Scope{}, sql); err != nil {
				t.Fatal(err)
			}
			want := strings.ReplaceAll(c.want, "{login}", login.Name)
			if err := directory.Setup(ctx, u, directory.Login{Name: login.Name, Password: "p", Role: directory.CellRole}); !errors.Is(err, directory.ErrLogin) || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want ErrLogin naming %q", err, want)
			}
		})
	}
}

// TestOwnersFunctionsAreNotTheLogins: a SECURITY DEFINER function the
// owner made, in schema public or in directory, runs as the owner; no
// login may call it after Setup (PUBLIC executes functions by default).
func TestOwnersFunctionsAreNotTheLogins(t *testing.T) {
	ctx := context.Background()
	l := setUpLogins(t)
	for _, sql := range []string{
		`CREATE FUNCTION public.purge() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`,
		`CREATE FUNCTION directory.purge() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`,
	} {
		if err := exec(ctx, l.owner, database.Scope{}, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := directory.Setup(ctx, l.url, l.all(t)...); err != nil {
		t.Fatalf("again: %v", err)
	}
	stores := map[string]*directory.Postgres{directory.RouterRole: l.router, directory.CellRole: l.cell, directory.WebRole: l.web}
	for role, dir := range stores {
		for _, sql := range []string{`SELECT public.purge()`, `SELECT directory.purge()`} {
			t.Run(role+": "+sql, func(t *testing.T) {
				err := exec(ctx, dir, database.Scope{}, sql)
				if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "42501" {
					t.Errorf("got %v, want it refused (42501)", err)
				}
			})
		}
	}
}

// TestPrivilegeRoleInAnother: a privilege role made a member of another
// role is refused. The roles are the server's, so the grant is never
// committed: checked inside the transaction that makes it, then rolled
// back, out of other packages' sight.
func TestPrivilegeRoleInAnother(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	c := pgtest.NewLogin(t)
	login := directory.Login{Name: c.Name, Password: c.Password, Role: directory.CellRole}
	if err := directory.Setup(ctx, u, login); err != nil {
		t.Fatal(err)
	}
	dir, err := directory.OpenWith(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	errDone := errors.New("rolled back")
	err = dir.Pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `GRANT erp_web TO erp_cell`); err != nil {
			return err
		}
		if err := directory.CheckLogin(ctx, tx, login); !errors.Is(err, directory.ErrLogin) {
			t.Errorf("got %v, want ErrLogin", err)
		}
		return errDone
	})
	if !errors.Is(err, errDone) {
		t.Fatal(err)
	}
}

// TestSetupRefusesServerWideRights: PUBLIC given a right on a server
// parameter, or every large object opened to all by
// lo_compat_privileges, is refused. Both are the server's, so neither is
// committed: checked inside the transaction that makes it, then rolled
// back, out of other packages' sight.
func TestSetupRefusesServerWideRights(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	c := pgtest.NewLogin(t)
	login := directory.Login{Name: c.Name, Password: c.Password, Role: directory.CellRole}
	if err := directory.Setup(ctx, u, login); err != nil {
		t.Fatal(err)
	}
	dir, err := directory.OpenWith(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	errDone := errors.New("rolled back")
	for name, c := range map[string]struct{ sql, want string }{
		"a parameter PUBLIC may set":   {`GRANT SET ON PARAMETER session_replication_role TO PUBLIC`, "PUBLIC's rights [parameter session_replication_role]"},
		"large objects open to all":    {`SET LOCAL lo_compat_privileges = on`, "PUBLIC's rights [large objects unchecked (lo_compat_privileges = on)]"},
		"a parameter PUBLIC may alter": {`GRANT ALTER SYSTEM ON PARAMETER work_mem TO PUBLIC`, "PUBLIC's rights [parameter work_mem]"},
	} {
		t.Run(name, func(t *testing.T) {
			err := dir.Pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, c.sql); err != nil {
					return err
				}
				if err := directory.CheckLogin(ctx, tx, login); !errors.Is(err, directory.ErrLogin) || !strings.Contains(err.Error(), c.want) {
					t.Errorf("got %v, want ErrLogin naming %q", err, c.want)
				}
				return errDone
			})
			if !errors.Is(err, errDone) {
				t.Fatal(err)
			}
		})
	}
}

// dbName is the quoted name of the database u names.
func dbName(t *testing.T, u string) string {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return pgx.Identifier{strings.TrimPrefix(parsed.Path, "/")}.Sanitize()
}

// TestSetupDatabaseFailures: every statement of a setup failing in turn
// is reported, and so is a database out of reach.
func TestSetupDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	c := pgtest.NewLogin(t)
	pool, err := database.Open(ctx, u, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	logins := []directory.Login{{Name: c.Name, Password: c.Password, Role: directory.CellRole}}
	for n := 1; ; n++ {
		err := directory.SetupIn(ctx, func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
		}, logins)
		if err == nil {
			if n == 1 {
				t.Error("got success without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want %v", n, err, pgtest.ErrInjected)
		}
	}
	if err := directory.Setup(ctx, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"); err == nil {
		t.Error("out of reach: got it set up, want an error")
	}
	empty := pgtest.EmptyDatabase(t)
	if dir, err := directory.OpenWith(ctx, empty, nil); err == nil || !errors.As(err, new(*pgconn.PgError)) {
		if dir != nil {
			dir.Close()
		}
		t.Errorf("a database not set up: got %v, want the missing schema reported", err)
	}
}
