package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Catalog answers other contexts' questions about the permission
// catalog and the tenant's segregation-of-duties rules, in the request's
// transaction: the control matrix says whether the permission or the
// rule a control is linked to is still there (ADR-0078 §8, grc.Access).
type Catalog struct{}

// Permissions returns the description of each of the codes the
// permission catalog holds, keyed by code; a code the catalog does not
// hold is absent.
func (Catalog) Permissions(ctx context.Context, codes []string) (map[string]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT code, description FROM ref.permission WHERE code = ANY($1)`, codes)
	out := map[string]string{}
	var code, description string
	if _, err := pgx.ForEachRow(rows, []any{&code, &description}, func() error { out[code] = description; return nil }); err != nil {
		return nil, fmt.Errorf("identity: descriptions of %d permissions: %w", len(codes), err)
	}
	return out, nil
}

// RuleValidity is a segregation-of-duties rule as another context shows
// it beside its id: its pair, why it conflicts and its validity, ValidTo
// exclusive and nil while the rule has no end.
type RuleValidity struct {
	PermissionA, PermissionB, Description string
	ValidFrom                             time.Time
	ValidTo                               *time.Time
}

// SodRules returns the tenant's segregation-of-duties rules with the
// ids, keyed by id, whether or not they are in force; an id the tenant
// has no rule of is absent.
func (Catalog) SodRules(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]RuleValidity, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, permission_a, permission_b, description, valid_from, valid_to FROM identity.sod_rule WHERE id = ANY($1)`, ids)
	type rule struct {
		id uuid.UUID
		RuleValidity
	}
	rules, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rule, error) {
		var x rule
		return x, r.Scan(&x.id, &x.PermissionA, &x.PermissionB, &x.Description, &x.ValidFrom, &x.ValidTo)
	})
	if err != nil {
		return nil, fmt.Errorf("identity: %d segregation-of-duties rules: %w", len(ids), err)
	}
	out := make(map[uuid.UUID]RuleValidity, len(rules))
	for _, x := range rules {
		out[x.id] = x.RuleValidity
	}
	return out, nil
}
