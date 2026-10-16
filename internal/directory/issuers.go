package directory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The directory's issuers (ADR-0031): which tenant an active identity
// provider's issuer and audience belong to, across cells. A cell claims the
// pair when a provider is registered, before it commits the registration,
// so two cells cannot register it for two tenants; it gives the pair up
// when the provider is retired, and every round replaces the cell's claims
// with its tenants' active providers, so a claim whose registration failed,
// or a release that did not reach the directory, heals itself. The router
// resolves a token's tenant from them (T37d). The cell's providers are read
// by Sync.Providers, which the server wires to the identity context, so the
// directory does not depend on it.

// ErrIssuerTaken means another tenant holds the issuer with the audience.
var ErrIssuerTaken = errors.New("directory: the issuer with this audience belongs to another tenant")

// Issuer is a claim: an issuer and audience, the tenant they belong to and
// the cell that claimed them.
type Issuer struct {
	Issuer   string
	Audience string
	TenantID uuid.UUID
	Cell     string
	// ProviderID is the registration that claims the pair: a release gives
	// up only its own registration's claim.
	ProviderID uuid.UUID
}

// Issuers is a directory that keeps issuers (the Postgres one).
type Issuers interface {
	ClaimIssuer(ctx context.Context, is Issuer) error
	ReleaseIssuer(ctx context.Context, is Issuer) error
	SyncIssuers(ctx context.Context, cell string, issuers []Issuer) (taken int, err error)
}

// claimSQL takes the pair for the tenant, or refreshes the tenant's own
// claim; another tenant's claim is left alone and nothing is returned.
const claimSQL = `INSERT INTO directory.issuer (issuer, audience, tenant_id, cell, provider_id) VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (issuer, audience) DO UPDATE SET cell = EXCLUDED.cell, provider_id = EXCLUDED.provider_id, updated_at = now()
	WHERE directory.issuer.tenant_id = EXCLUDED.tenant_id
	RETURNING true`

func claimIn(ctx context.Context, tx pgx.Tx, is Issuer) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, claimSQL, is.Issuer, is.Audience, is.TenantID, is.Cell, is.ProviderID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim issuer %s %s: %w", is.Issuer, is.Audience, err)
	}
	return true, nil
}

// ClaimIssuer takes the issuer with the audience for the tenant;
// ErrIssuerTaken when another tenant holds them.
func (p *Postgres) ClaimIssuer(ctx context.Context, is Issuer) error {
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		ok, err := claimIn(ctx, tx, is)
		if err == nil && !ok {
			err = ErrIssuerTaken
		}
		return err
	})
	if err != nil && !errors.Is(err, ErrIssuerTaken) {
		return fmt.Errorf("directory: %w", err)
	}
	return err
}

// ReleaseIssuer gives up the registration's claim of the issuer with the
// audience; a claim another registration made since (another tenant's, or
// the same tenant's registering the pair again) stays.
func (p *Postgres) ReleaseIssuer(ctx context.Context, is Issuer) error {
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM directory.issuer WHERE issuer = $1 AND audience = $2 AND provider_id = $3`, is.Issuer, is.Audience, is.ProviderID); err != nil {
			return fmt.Errorf("release issuer %s %s: %w", is.Issuer, is.Audience, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("directory: %w", err)
	}
	return nil
}

// ClaimGrace is how old a claim must be before a round of its cell may give
// it up: a claim is made before its registration commits, so a round that
// read the cell's providers just before the commit would otherwise drop
// it, and another cell could then take the pair. A registration's
// transaction is bounded by controlplane.DefaultWriteTimeout (one minute, as
// PostgreSQL's transaction_timeout), so it commits or fails well within
// the grace; provisioning claims last, right before its commit.
const ClaimGrace = 5 * time.Minute

// SyncIssuers makes the cell's claims the given ones: each is claimed, and
// the cell's other claims older than ClaimGrace are given up. It returns
// how many another tenant holds; those are not taken over.
func (p *Postgres) SyncIssuers(ctx context.Context, cell string, issuers []Issuer) (int, error) {
	taken := 0
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		names, audiences := make([]string, len(issuers)), make([]string, len(issuers))
		for i, is := range issuers {
			ok, err := claimIn(ctx, tx, is)
			if err != nil {
				return err
			}
			if !ok {
				taken++
			}
			names[i], audiences[i] = is.Issuer, is.Audience
		}
		if _, err := tx.Exec(ctx, `DELETE FROM directory.issuer WHERE cell = $1 AND updated_at < now() - make_interval(secs => $4)
			AND (issuer, audience) NOT IN (SELECT * FROM unnest($2::text[], $3::text[]))`, cell, names, audiences, ClaimGrace.Seconds()); err != nil {
			return fmt.Errorf("give up stale issuers of cell %s: %w", cell, err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("directory: %w", err)
	}
	return taken, nil
}

// ClaimIssuer claims a provider's issuer and audience for the tenant before
// its registration commits: 409 identity.provider_taken when another tenant
// holds them, 503 when the directory cannot be reached, since the pair
// could not be checked across cells. Without a directory that keeps
// issuers (a single cell) there is nothing to claim.
func (s *Sync) ClaimIssuer(ctx context.Context, tenant, provider uuid.UUID, issuer, audience string) error {
	dir, ok := s.Directory.(Issuers)
	if !ok {
		return nil
	}
	err := dir.ClaimIssuer(ctx, Issuer{Issuer: issuer, Audience: audience, TenantID: tenant, Cell: s.Cell, ProviderID: provider})
	switch {
	case errors.Is(err, ErrIssuerTaken):
		return problem.Conflict("identity.provider_taken", "This issuer is already registered with this audience, by another tenant.").WithCause(err)
	case err != nil:
		return problem.Unavailable("The global directory could not be reached; try again shortly.", 30).WithCause(err)
	}
	return nil
}

// ReleaseIssuer gives up the provider's claim after it was retired.
// Failures are logged: the next round gives it up.
func (s *Sync) ReleaseIssuer(ctx context.Context, tenant, provider uuid.UUID, issuer, audience string) {
	dir, ok := s.Directory.(Issuers)
	if !ok {
		return
	}
	if err := dir.ReleaseIssuer(ctx, Issuer{Issuer: issuer, Audience: audience, TenantID: tenant, Cell: s.Cell, ProviderID: provider}); err != nil {
		s.Log.ErrorContext(ctx, "directory release of an issuer failed; the next round retries", "tenant_id", tenant, "issuer", issuer, "error", err)
	}
}

// syncIssuers replaces the cell's claims with its tenants' active
// providers, read in the round's transaction as the control role.
func (s *Sync) syncIssuers(ctx context.Context, tx pgx.Tx) error {
	dir, ok := s.Directory.(Issuers)
	if !ok || s.Providers == nil {
		return nil
	}
	issuers, err := s.Providers(ctx, tx)
	if err != nil {
		return fmt.Errorf("directory: read identity providers of cell %s: %w", s.Cell, err)
	}
	for i := range issuers {
		issuers[i].Cell = s.Cell
	}
	taken, err := dir.SyncIssuers(ctx, s.Cell, issuers)
	if err != nil {
		return err
	}
	if taken > 0 {
		s.Log.WarnContext(ctx, "identity providers whose issuer and audience another tenant holds in the directory", "cell", s.Cell, "held by another tenant", taken)
	}
	return nil
}

// LookupIssuer lists the claims holding the issuer with one of the
// audiences.
func (p *Postgres) LookupIssuer(ctx context.Context, issuer string, audiences []string) ([]Issuer, error) {
	var out []Issuer
	err := p.inTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT issuer, audience, tenant_id, cell, coalesce(provider_id, '00000000-0000-0000-0000-000000000000')
			FROM directory.issuer WHERE issuer = $1 AND audience = ANY($2) ORDER BY audience`, issuer, audiences)
		var err error
		if out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Issuer]); err != nil {
			return fmt.Errorf("look up issuer %s: %w", issuer, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	return out, nil
}

// IssuerRegistry is the router's authn.Registry: a token's tenant is the
// one whose claim holds its issuer and audience (ADR-0031). The cell checks
// the rest of the registration (its tenant claim) when the token reaches it.
type IssuerRegistry struct {
	Directory *Postgres
}

// Resolve implements authn.Registry.
func (r IssuerRegistry) Resolve(ctx context.Context, issuer string, audiences []string) (authn.Registration, error) {
	claims, err := r.Directory.LookupIssuer(ctx, issuer, audiences)
	switch {
	case err != nil:
		return authn.Registration{}, err
	case len(claims) == 0:
		return authn.Registration{}, authn.ErrUnknownIssuer
	case len(claims) > 1:
		return authn.Registration{}, authn.ErrAmbiguousIssuer
	}
	c := claims[0]
	return authn.Registration{ProviderID: c.ProviderID, TenantID: c.TenantID, Issuer: c.Issuer, Audience: c.Audience}, nil
}
