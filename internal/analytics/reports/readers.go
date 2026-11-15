package reports

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// reader is who reads a report (ADR-0076 §7): the caller, whether shares
// reach them (a person: a share reaches no agent or service, even
// through a role it holds) and the roles they hold today.
type reader struct {
	id     uuid.UUID
	person bool
	roles  []uuid.UUID
}

// readable is the condition that the report r is read by the reader whose
// args are $1 to $3: its owner, or a person it is shared with, directly or
// through a role they hold, by an active share.
const readable = `(r.owner_id = $1 OR ($2 AND EXISTS (SELECT FROM analytics.report_share s WHERE s.tenant_id = r.tenant_id
	AND s.report_id = r.id AND s.status = 'active' AND (s.principal_id = $1 OR s.role_id = ANY($3)))))`

// args are the reader as readable's $1 to $3.
func (v reader) args() []any { return []any{v.id, v.person, v.roles} }

// access is the reader's access to a report they read.
func (v reader) access(r Report) string {
	if r.OwnerID == v.id {
		return Owner
	}
	return Viewer
}

// owner is a reader of their own reports alone: the worker acting as a
// report's owner.
func owner(id uuid.UUID) reader { return reader{id: id, roles: []uuid.UUID{}} }

// readerOf is the caller as a reader, with the roles they hold today.
func (e *Endpoints) readerOf(ctx context.Context, p reqctx.Principal) (reader, error) {
	v := owner(p.ID)
	if p.Kind != reqctx.KindHuman {
		return v, nil
	}
	roles, err := e.Grantees.RolesHeld(ctx, p.ID)
	if err != nil {
		return reader{}, fmt.Errorf("analytics: roles of %s: %w", p.ID, err)
	}
	v.person, v.roles = true, roles
	return v, nil
}

// request is the request's transaction, its caller and the caller as a
// reader.
func (e *Endpoints) request(ctx context.Context) (pgx.Tx, reqctx.Principal, reader, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, reqctx.Principal{}, reader{}, err
	}
	v, err := e.readerOf(ctx, p)
	return tx, p, v, err
}

// companies are the codes of the companies the caller in ctx may read
// now: what a run of theirs covers, and what a kept run they read must
// stay within (ADR-0076 §7).
func (e *Endpoints) companies(ctx context.Context) ([]string, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := e.Companies.Codes(ctx, access)
	if err != nil {
		return nil, fmt.Errorf("analytics: companies of the caller: %w", err)
	}
	// None is an empty set, never NULL: runSelect's containment reads
	// NULL as a run kept before T95c.
	return append([]string{}, codes...), nil
}
