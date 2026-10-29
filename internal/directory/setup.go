package directory

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// The directory's privilege roles (T72h, ADR-0052 D1): what each of its
// clients may do, derived from the statements this package and
// internal/web/session run for it. None of them may log in; each client
// connects as a login of its own that is a member of its role alone
// (Setup), never as the database's owner, the RDS master user, which
// only Setup uses.
const (
	// RouterRole is the router's, the global control plane's with it:
	// it reads tenants, cells, issuers and placements, enters a tenant it
	// provisioned at once (Upsert), sets a tenant's status and cell
	// (SetStatus, Move) and records placements (Place).
	RouterRole = "erp_router"
	// CellRole is a cell's: it records its tenants (Upsert, Reinstate), its
	// own entry (UpsertCell) and its identity providers' claims (ClaimIssuer,
	// ClaimWebClient, ReleaseIssuer, SyncIssuers), and reads them back.
	CellRole = "erp_cell"
	// WebRole is erp-web's: its sessions and the web clients' view alone
	// (WebSchema).
	WebRole = "erp_web"
)

// Grants are the privilege roles' rights, given again by every Setup
// after taking back whatever the roles held on the directory's and the web
// schemas and their tables, so a right dropped here, or given by hand, is
// gone after the next release. Inserts and updates are granted on the
// columns written, never a key but the ones a new row needs; no role may
// truncate, reference or trigger, and only the cell deletes (its issuers'
// claims) and erp-web (its sessions). Nobody but the owner may make
// temporary objects either, nor anything on the database: CREATE and
// TEMPORARY are taken from PUBLIC, every right on the database from the
// roles (PUBLIC's CONNECT stays), and every right on schema public from
// all; checkLogin refuses a schema of anyone's beyond those, whose
// PUBLIC rights nothing here would take back.
const Grants = `
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_router') THEN CREATE ROLE erp_router NOLOGIN; END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_cell') THEN CREATE ROLE erp_cell NOLOGIN; END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;
DO $$ BEGIN
  EXECUTE format('REVOKE CREATE, TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
  EXECUTE format('REVOKE ALL ON DATABASE %I FROM erp_router, erp_cell, erp_web', current_database());
END $$;
-- PUBLIC is revoked too, since a right given to it is every login's, and
-- pg_shdepend, which checkLogin reads for the roles and the logins, does
-- not record it. Routines and sequences: PUBLIC executes functions by
-- default. The directory uses nothing in schema public, which a dropped
-- one makes this fail, closed.
REVOKE ALL ON ALL TABLES IN SCHEMA directory, web FROM PUBLIC, erp_router, erp_cell, erp_web;
REVOKE ALL ON ALL ROUTINES IN SCHEMA directory, web FROM PUBLIC;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA directory, web FROM PUBLIC;
REVOKE ALL ON SCHEMA directory, web FROM PUBLIC, erp_router, erp_cell, erp_web;
REVOKE ALL ON SCHEMA public FROM PUBLIC, erp_router, erp_cell, erp_web;
GRANT USAGE ON SCHEMA directory TO erp_router, erp_cell;
GRANT SELECT ON directory.tenant, directory.cell, directory.issuer, directory.placement TO erp_router;
GRANT INSERT (tenant_id, region, cell, api_url, status), UPDATE (region, cell, api_url, status, version, updated_at) ON directory.tenant TO erp_router;
GRANT INSERT (idempotency_key, cell) ON directory.placement TO erp_router;
GRANT SELECT, INSERT (tenant_id, region, cell, api_url, status), UPDATE (region, cell, api_url, status, version, updated_at) ON directory.tenant TO erp_cell;
GRANT SELECT, INSERT (name, region, api_url, control_url, accepting), UPDATE (region, api_url, control_url, accepting, updated_at) ON directory.cell TO erp_cell;
GRANT SELECT, INSERT (issuer, audience, tenant_id, cell, provider_id),
  UPDATE (cell, provider_id, updated_at, login_code, client_id, auth_method, credential_ref, scope), DELETE ON directory.issuer TO erp_cell;
GRANT USAGE ON SCHEMA web TO erp_web;
GRANT SELECT, INSERT, UPDATE, DELETE ON web.session TO erp_web;
GRANT SELECT ON web.login_client TO erp_web;`

// Login is a client's login to the directory: its name, its password
// (operators set it in the secret store; Setup sets it again each run, so
// a changed one takes effect) and the privilege role it is a member of.
type Login struct {
	Name, Password, Role string
}

// ErrLogin means a login cannot be made least privileged: a name that is
// not a login's, no password, or a role that already holds more than its
// privilege role gives it.
var ErrLogin = errors.New("directory: login")

// Setup makes the directory's schema and its privilege roles' grants, and
// each login, as the database's owner (url names the RDS master user):
// what the erp-global chart's hook Job runs before each release (erp-router
// setup-directory), and development and tests before their first opener.
// Every step leaves what is already there as it is, so a second run
// changes nothing but the logins' passwords.
func Setup(ctx context.Context, url string, logins ...Login) error {
	pool, err := database.Open(ctx, url, database.Settings{MaxConns: 1})
	if err != nil {
		return fmt.Errorf("directory: setup: %w", err)
	}
	defer pool.Close()
	return setupIn(ctx, pool.InTx, logins)
}

func setupIn(ctx context.Context, inTx func(context.Context, database.Scope, func(context.Context, pgx.Tx) error) error, logins []Login) error {
	err := inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		// Openers starting together (tests, a restore beside the live one)
		// serialize on an advisory lock: two concurrent CREATE SCHEMA IF NOT
		// EXISTS otherwise race into a unique violation on pg_namespace.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('directory.schema'));`+Schema+Grants); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
		for _, l := range logins {
			if err := ensureLogin(ctx, tx, l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("directory: setup: %w", err)
	}
	return nil
}

// ensureLogin creates the login, or sets its password, makes it a member
// of its role, takes back what was granted to it directly on the
// directory's and the web schemas, and then refuses it if it is anything
// more than its role. The password reaches the server only as its
// SCRAM-SHA-256 verifier, so no log or statistics of a statement holds it
// (a failed statement is logged whole); PostgreSQL's format quotes the
// name and the verifier, neither is spliced in here.
func ensureLogin(ctx context.Context, tx pgx.Tx, l Login) error {
	roles := []string{RouterRole, CellRole, WebRole}
	if !slices.Contains(roles, l.Role) || l.Name == "" || slices.Contains(roles, l.Name) || !printable(l.Password) {
		return fmt.Errorf("%w %q: want a name that is no privilege role, a password of printable ASCII and one of the roles %v", ErrLogin, l.Name, roles)
	}
	var stmt string
	err := tx.QueryRow(ctx, `SELECT format(CASE WHEN EXISTS (SELECT FROM pg_roles WHERE rolname = $1) THEN 'ALTER ROLE %I LOGIN PASSWORD %L'
		ELSE 'CREATE ROLE %I LOGIN PASSWORD %L' END, $1::text, $2::text) WHERE $1 <> current_user`, l.Name, verifier(l.Password)).Scan(&stmt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w %q: the owner Setup connects as is no client's login", ErrLogin, l.Name)
	}
	if err == nil {
		_, err = tx.Exec(ctx, stmt)
	}
	if err == nil {
		login, role := pgx.Identifier{l.Name}.Sanitize(), pgx.Identifier{l.Role}.Sanitize()
		_, err = tx.Exec(ctx, "GRANT "+role+" TO "+login+"; REVOKE ALL ON ALL TABLES IN SCHEMA directory, web FROM "+login+
			"; REVOKE ALL ON SCHEMA directory, web FROM "+login)
	}
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT format('REVOKE ALL ON DATABASE %I FROM %I', current_database(), $1::text)`, l.Name).Scan(&stmt)
	}
	if err == nil {
		_, err = tx.Exec(ctx, stmt)
	}
	if err != nil {
		return fmt.Errorf("login %s: %w", l.Name, err)
	}
	return checkLogin(ctx, tx, l)
}

// printable is a password PostgreSQL's SASLprep leaves as it is, so the
// verifier computed here is the one the server checks against: printable
// ASCII (`openssl rand -hex 32` is).
func printable(password string) bool {
	return password != "" && !strings.ContainsFunc(password, func(r rune) bool { return r < ' ' || r > '~' })
}

// verifier is the password's SCRAM-SHA-256 verifier as PostgreSQL stores
// it (RFC 5802, RFC 7677), with a fresh salt and PostgreSQL's 4096
// iterations: PBKDF2-HMAC-SHA-256 of one block, then the stored and server
// keys.
func verifier(password string) string {
	const iterations = 4096
	salt := make([]byte, 16)
	_, _ = rand.Read(salt) // crypto/rand.Read never returns an error: it crashes the program instead (Go 1.24+)
	mac := func(key, msg []byte) []byte {
		h := hmac.New(sha256.New, key)
		h.Write(msg)
		return h.Sum(nil)
	}
	u := mac([]byte(password), append(slices.Clone(salt), 0, 0, 0, 1))
	salted := slices.Clone(u)
	for range iterations - 1 {
		u = mac([]byte(password), u)
		for i := range salted {
			salted[i] ^= u[i]
		}
	}
	stored := sha256.Sum256(mac(salted, []byte("Client Key")))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac(salted, []byte("Server Key"))))
}

// expectedACL are the objects Grants gives the privilege roles rights on:
// the only dependencies of theirs that checkLogin lets stand.
const expectedACL = `(d.classid = 'pg_class'::regclass AND d.objid IN ('directory.tenant'::regclass, 'directory.cell'::regclass,
		'directory.issuer'::regclass, 'directory.placement'::regclass, 'web.session'::regclass, 'web.login_client'::regclass))
	OR (d.classid = 'pg_namespace'::regclass AND d.objid IN ('directory'::regnamespace, 'web'::regnamespace))`

// publicRights are the rights PUBLIC, and so every login, holds on the
// object kinds pg_shdepend records no right of PUBLIC's for (it records
// roles alone, and PUBLIC is none) and Grants does not take back: large
// objects (pg_largeobject_metadata.lomacl, and lo_compat_privileges,
// which turns their privilege checks off), foreign-data wrappers
// (pg_foreign_data_wrapper.fdwacl), foreign servers
// (pg_foreign_server.srvacl), trusted languages but sql and plpgsql
// (pg_language.lanacl) and the server's parameters (pg_parameter_acl);
// a null ACL is its kind's default (acldefault), and aclexplode gives
// PUBLIC as grantee 0 (PostgreSQL 17 manual, §9.27 table 9.72). They are
// refused, not taken back: the directory makes and uses none of these
// objects, and the owner Setup runs as need not own one someone else
// made, when its REVOKE would only warn (REVOKE, Notes). sql's and
// plpgsql's USAGE, PUBLIC's by default (§5.8 table 5.2), stands: Grants'
// DO blocks need plpgsql's (DO, Notes) where the owner is no superuser,
// as on RDS, and a login, which may create no function, runs a DO block
// with its own rights alone.
const publicRights = `ARRAY(SELECT kind || ' ' || name FROM (
			SELECT 'large object' AS kind, oid::text AS name, coalesce(lomacl, acldefault('L', lomowner)) AS acl FROM pg_largeobject_metadata
			UNION ALL SELECT 'foreign-data wrapper', fdwname::text, coalesce(fdwacl, acldefault('F', fdwowner)) FROM pg_foreign_data_wrapper
			UNION ALL SELECT 'foreign server', srvname::text, coalesce(srvacl, acldefault('S', srvowner)) FROM pg_foreign_server
			UNION ALL SELECT 'language', lanname::text, coalesce(lanacl, acldefault('l', lanowner)) FROM pg_language
				WHERE lanpltrusted AND lanname NOT IN ('sql', 'plpgsql')
			UNION ALL SELECT 'parameter', parname, paracl FROM pg_parameter_acl) o
			WHERE EXISTS (SELECT FROM aclexplode(o.acl) a WHERE a.grantee = 0)
		UNION ALL SELECT 'parameter lo_compat_privileges = on' WHERE current_setting('lo_compat_privileges')::bool
		ORDER BY 1)`

// checkLogin refuses a login that may do more than its role: one an
// operator made a superuser, gave a right to create roles or databases,
// to replicate or to bypass row security, made a member of another role
// or able to grant its own; a privilege role made a member of any role;
// and any dependency PostgreSQL records on the login or a privilege role
// (pg_shdepend: an owned object, the database included, a right on any
// object, shared ones such as databases and parameters too, a policy) in
// this database or the server's, but the rights Grants gives the roles;
// any schema in the database beyond the system's, public, directory and
// web, whose PUBLIC rights Setup does not govern; and any of
// publicRights.
// Setup does not take such rights back: someone gave them, and the
// release stops until they look.
func checkLogin(ctx context.Context, tx pgx.Tx, l Login) error {
	var attributes, admin bool
	var others, deps, schemas, public []string
	err := tx.QueryRow(ctx, `SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls,
		EXISTS (SELECT FROM pg_auth_members m WHERE m.member = r.oid AND m.admin_option),
		ARRAY(SELECT p.rolname || ' in ' || g.rolname FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles p ON p.oid = m.member
			WHERE (m.member = r.oid AND g.rolname <> $2) OR p.rolname = ANY($3) ORDER BY 1),
		ARRAY(SELECT o.rolname || ': ' || coalesce(pg_describe_object(d.classid, d.objid, d.objsubid), d.classid::regclass::text) || ' (' || d.deptype::text || ')'
			FROM pg_shdepend d JOIN pg_roles o ON o.oid = d.refobjid
			WHERE d.refclassid = 'pg_authid'::regclass AND (o.rolname = $1 OR o.rolname = ANY($3))
				AND d.dbid IN (0, (SELECT oid FROM pg_database WHERE datname = current_database()))
				AND NOT (d.deptype = 'a' AND o.rolname = ANY($3) AND (`+expectedACL+`))
			ORDER BY 1),
		ARRAY(SELECT nspname::text FROM pg_namespace WHERE nspname NOT IN ('pg_catalog', 'information_schema', 'public', 'directory', 'web')
			AND nspname NOT LIKE 'pg\_toast%' AND nspname NOT LIKE 'pg\_temp\_%' ORDER BY 1),
		`+publicRights+`
		FROM pg_roles r WHERE r.rolname = $1`, l.Name, l.Role, []string{RouterRole, CellRole, WebRole}).Scan(&attributes, &admin, &others, &deps, &schemas, &public)
	if err != nil {
		return fmt.Errorf("login %s: read its rights: %w", l.Name, err)
	}
	if attributes || admin || len(others) > 0 || len(deps) > 0 || len(schemas) > 0 || len(public) > 0 {
		return fmt.Errorf("%w %s: holds more than %s (role attributes %t, admin of its role %t, memberships %v, owned or granted %v; other schemas %v; PUBLIC's rights %v)",
			ErrLogin, l.Name, l.Role, attributes, admin, others, deps, schemas, public)
	}
	return nil
}
