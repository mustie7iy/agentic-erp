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
// temporary objects either: TEMPORARY is taken from PUBLIC.
const Grants = `
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_router') THEN CREATE ROLE erp_router NOLOGIN; END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_cell') THEN CREATE ROLE erp_cell NOLOGIN; END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;
DO $$ BEGIN EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database()); END $$;
REVOKE ALL ON ALL TABLES IN SCHEMA directory, web FROM erp_router, erp_cell, erp_web;
REVOKE ALL ON SCHEMA directory, web FROM erp_router, erp_cell, erp_web;
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

// checkLogin refuses a login that may do more than its role: one an
// operator made a superuser, gave a right to create roles or databases,
// to replicate or to bypass row security, made a member of another role
// or able to grant its own, or that owns objects in the directory's
// database; and a privilege role made a member of any role. Setup does not
// take such rights back: someone gave them, and the release stops until
// they look.
func checkLogin(ctx context.Context, tx pgx.Tx, l Login) error {
	var attributes, admin, owns bool
	var others []string
	err := tx.QueryRow(ctx, `SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls,
		EXISTS (SELECT FROM pg_auth_members m WHERE m.member = r.oid AND m.admin_option),
		EXISTS (SELECT FROM pg_class WHERE relowner = r.oid) OR EXISTS (SELECT FROM pg_namespace WHERE nspowner = r.oid)
			OR EXISTS (SELECT FROM pg_proc WHERE proowner = r.oid) OR EXISTS (SELECT FROM pg_type WHERE typowner = r.oid),
		ARRAY(SELECT p.rolname || ' in ' || g.rolname FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles p ON p.oid = m.member
			WHERE (m.member = r.oid AND g.rolname <> $2) OR p.rolname = ANY($3) ORDER BY 1)
		FROM pg_roles r WHERE r.rolname = $1`, l.Name, l.Role, []string{RouterRole, CellRole, WebRole}).Scan(&attributes, &admin, &owns, &others)
	if err != nil {
		return fmt.Errorf("login %s: read its rights: %w", l.Name, err)
	}
	if attributes || admin || owns || len(others) > 0 {
		return fmt.Errorf("%w %s: holds more than %s (role attributes %t, admin of its role %t, owns objects %t, memberships %v)",
			ErrLogin, l.Name, l.Role, attributes, admin, owns, others)
	}
	return nil
}
