package directory

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
	// it reads tenants, cells, issuers and placements, sets a tenant's
	// status and cell (SetStatus, Move) and records placements (Place).
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
// after taking back what the roles held on the directory's tables, so a
// right dropped here is dropped from the database too. Updates are
// granted on the columns written, never the keys; no role may truncate,
// reference, trigger or delete but the cell its own issuers' claims.
const Grants = `
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_router') THEN CREATE ROLE erp_router NOLOGIN; END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_cell') THEN CREATE ROLE erp_cell NOLOGIN; END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;
REVOKE ALL ON ALL TABLES IN SCHEMA directory FROM erp_router, erp_cell;
REVOKE ALL ON SCHEMA web FROM erp_router, erp_cell;
GRANT USAGE ON SCHEMA directory TO erp_router, erp_cell;
GRANT SELECT ON directory.tenant, directory.cell, directory.issuer, directory.placement TO erp_router;
GRANT UPDATE (cell, api_url, status, version, updated_at) ON directory.tenant TO erp_router;
GRANT INSERT (idempotency_key, cell) ON directory.placement TO erp_router;
GRANT SELECT, INSERT (tenant_id, region, cell, api_url, status), UPDATE (region, cell, api_url, status, version, updated_at) ON directory.tenant TO erp_cell;
GRANT SELECT, INSERT (name, region, api_url, control_url, accepting), UPDATE (region, api_url, control_url, accepting, updated_at) ON directory.cell TO erp_cell;
GRANT SELECT, INSERT (issuer, audience, tenant_id, cell, provider_id),
  UPDATE (cell, provider_id, updated_at, login_code, client_id, auth_method, credential_ref, scope), DELETE ON directory.issuer TO erp_cell;`

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

// ensureLogin creates the login, or sets its password, and makes it a
// member of its role; then refuses it if it is anything more. PostgreSQL's
// format quotes the name and the password; neither is spliced in here.
func ensureLogin(ctx context.Context, tx pgx.Tx, l Login) error {
	roles := []string{RouterRole, CellRole, WebRole}
	if !slices.Contains(roles, l.Role) || l.Name == "" || slices.Contains(roles, l.Name) || l.Password == "" {
		return fmt.Errorf("%w %q: want a name that is no privilege role, a password and one of the roles %v", ErrLogin, l.Name, roles)
	}
	var stmt string
	err := tx.QueryRow(ctx, `SELECT format(CASE WHEN EXISTS (SELECT FROM pg_roles WHERE rolname = $1) THEN 'ALTER ROLE %I LOGIN PASSWORD %L'
		ELSE 'CREATE ROLE %I LOGIN PASSWORD %L' END, $1::text, $2::text) WHERE $1 <> current_user`, l.Name, l.Password).Scan(&stmt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w %q: the owner Setup connects as is no client's login", ErrLogin, l.Name)
	}
	if err == nil {
		_, err = tx.Exec(ctx, stmt)
	}
	if err == nil {
		_, err = tx.Exec(ctx, "GRANT "+pgx.Identifier{l.Role}.Sanitize()+" TO "+pgx.Identifier{l.Name}.Sanitize())
	}
	if err != nil {
		return fmt.Errorf("login %s: %w", l.Name, err)
	}
	return checkLogin(ctx, tx, l)
}

// checkLogin refuses a login that may do more than its role: one an
// operator made a superuser, gave a right to create roles or databases,
// to replicate or to bypass row security, or made a member of another
// role. Setup does not take such rights back: someone gave them, and the
// release stops until they look.
func checkLogin(ctx context.Context, tx pgx.Tx, l Login) error {
	var more bool
	var others []string
	err := tx.QueryRow(ctx, `SELECT r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls,
		ARRAY(SELECT g.rolname::text FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid AND g.rolname <> $2 ORDER BY 1)
		FROM pg_roles r WHERE r.rolname = $1`, l.Name, l.Role).Scan(&more, &others)
	if err != nil {
		return fmt.Errorf("login %s: read its rights: %w", l.Name, err)
	}
	if more || len(others) > 0 {
		return fmt.Errorf("%w %s: holds more than %s (role attributes %t, member of %v)", ErrLogin, l.Name, l.Role, more, others)
	}
	return nil
}
