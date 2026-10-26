package identity

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// GetCurrentPrincipal implements GET /v1/me (ADR-0052 G2): who the caller
// is, the companies it may act in and the permissions it may use, as
// Authorize and the contexts' company checks decide them. Advisory: the
// API still decides every call.
func (e *Endpoints) GetCurrentPrincipal(ctx context.Context) (gen.GetCurrentPrincipalRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	me, err := getPrincipal(ctx, tx, p.ID, false)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	usable, barred, err := authz.Effective(ctx)
	if err != nil {
		return nil, err
	}
	out := gen.CurrentPrincipal{ID: me.ID, TenantID: p.TenantID, Kind: me.Kind, DisplayName: me.DisplayName, AllCompanies: access.All,
		LegalEntityIds: []uuid.UUID{}, Permissions: nonNil(usable), BarredPermissions: nonNil(barred)}
	if !access.All {
		out.LegalEntityIds = slices.SortedFunc(slices.Values(access.IDs), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	}
	out.Email.SetToNull()
	if me.Email != nil {
		out.Email.SetTo(*me.Email)
	}
	out.AutonomyLevel.SetToNull()
	if me.Kind == Agent {
		var level int16 // an agent without a profile is at level 0 (authz.Level)
		if me.Profile != nil {
			level = me.Profile.AutonomyLevel
		}
		out.AutonomyLevel.SetTo(int32(level))
	}
	return &out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ListPrincipalNames implements GET /v1/principal-names (ADR-0052 G3): the
// name and kind of each principal of the tenant among ids, in their
// order, each once. Row-level security keeps the read to the caller's
// tenant; an id of another tenant, or of none, is left out alike.
func (e *Endpoints) ListPrincipalNames(ctx context.Context, params gen.ListPrincipalNamesParams) (gen.ListPrincipalNamesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	// CollectRows below returns the query's error.
	rows, _ := tx.Query(ctx, `SELECT p.id, p.display_name, p.kind
		FROM (SELECT id, min(n) AS n FROM unnest($1::uuid[]) WITH ORDINALITY AS asked (id, n) GROUP BY id) asked
		JOIN identity.principal p ON p.id = asked.id
		ORDER BY asked.n`, params.Ids)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gen.PrincipalName, error) {
		var n gen.PrincipalName
		return n, r.Scan(&n.ID, &n.DisplayName, &n.Kind)
	})
	if err != nil {
		return nil, fmt.Errorf("identity: principal names: %w", err)
	}
	return &gen.PrincipalNames{Items: items}, nil
}
