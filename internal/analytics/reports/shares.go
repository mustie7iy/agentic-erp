package reports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	// A second active share of a report with one person or role (ADR-0076
	// §7), however many requests make it at once.
	problem.RegisterConstraint("report_share_once", func() *problem.Problem {
		return problem.Conflict("analytics.report_share_exists", "The report is already shared with this person or role; end that share first to share it again.")
	})
}

// The statuses of a share.
const (
	ShareActive = "active"
	ShareEnded  = "ended"
)

// Share is a row of analytics.report_share with its report's title.
type Share struct {
	ID          uuid.UUID
	ReportID    uuid.UUID
	ReportTitle string
	PrincipalID *uuid.UUID
	RoleID      *uuid.UUID
	Status      string
	Version     int32
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
	UpdatedBy   uuid.UUID
}

// ShareSet is whom a report is shared with: a person or a role.
type ShareSet struct {
	PrincipalID *uuid.UUID
	RoleID      *uuid.UUID
}

// shareSelect reads shares with their report's title; the join is inside,
// so the keyset's columns are the share's.
const shareSelect = `SELECT id, report_id, title, principal_id, role_id, status, version, created_at, created_by, updated_at, updated_by
	FROM (SELECT s.id, s.report_id, r.title, s.principal_id, s.role_id, s.status, s.version, s.created_at, s.created_by, s.updated_at, s.updated_by
		FROM analytics.report_share s JOIN analytics.report r ON r.tenant_id = s.tenant_id AND r.id = s.report_id) shares`

func scanShare(row pgx.Row) (Share, error) {
	var s Share
	err := row.Scan(&s.ID, &s.ReportID, &s.ReportTitle, &s.PrincipalID, &s.RoleID, &s.Status, &s.Version, &s.CreatedAt, &s.CreatedBy,
		&s.UpdatedAt, &s.UpdatedBy)
	return s, errs.Wrapf(err, "scan report share")
}

// share reads a share of the report, locked for its end when forUpdate.
func share(ctx context.Context, tx pgx.Tx, report, id uuid.UUID, forUpdate bool) (Share, error) {
	q := shareSelect + ` WHERE report_id = $1 AND id = $2`
	if forUpdate {
		// The share's own row: nothing references it, and the report's row
		// is not locked by an end, so an end and a change of the report
		// never wait for each other in opposite order.
		q = `SELECT id, report_id, '', principal_id, role_id, status, version, created_at, created_by, updated_at, updated_by
			FROM analytics.report_share WHERE report_id = $1 AND id = $2 FOR UPDATE`
	}
	s, err := scanShare(tx.QueryRow(ctx, q, report, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Share{}, problem.NotFoundOf("report share")
	case err != nil:
		return Share{}, fmt.Errorf("analytics: read share %s of report %s: %w", id, report, err)
	}
	return s, nil
}

// check refuses a share with neither a person nor a role, or with both.
func (in ShareSet) check() error {
	if (in.PrincipalID == nil) == (in.RoleID == nil) {
		return problem.Unprocessable("request.validation_failed", "A report is shared with a person or a role, one of the two.",
			problem.FieldError{Field: "principal_id", Code: "invalid", Message: "give principal_id or role_id, one of the two"})
	}
	return nil
}

// grantee refuses whom the owner of r may not share it with: the owner,
// a principal or role the tenant does not have, an inactive role, and
// anyone but a person.
func (e *Endpoints) grantee(ctx context.Context, r Report, in ShareSet) error {
	if id := in.RoleID; id != nil {
		roles, err := e.Grantees.Roles(ctx, []uuid.UUID{*id})
		if err != nil {
			return fmt.Errorf("analytics: role %s: %w", *id, err)
		}
		if !roles[*id].Active {
			return problem.ReferenceNotFound("role_id", "role", "no active role with this id")
		}
		return nil
	}
	id := *in.PrincipalID
	if id == r.OwnerID {
		return problem.Unprocessable("analytics.report_share_owner", "Report "+r.Title+" is yours: it is shared with others.")
	}
	kinds, err := e.Grantees.Kinds(ctx, []uuid.UUID{id})
	if err != nil {
		return fmt.Errorf("analytics: principal %s: %w", id, err)
	}
	switch kind, ok := kinds[id]; {
	case !ok:
		return problem.ReferenceNotFound("principal_id", "principal", "no principal with this id")
	case kind != reqctx.KindHuman:
		return problem.Unprocessable("analytics.report_share_not_person", "A report is shared with people: this principal is a "+kind+".")
	}
	return nil
}

// CreateShare shares one of the caller's active reports with a person or
// a role (ADR-0076 §7), the report's row locked as for any change of it.
func (e *Endpoints) CreateShare(ctx context.Context, report uuid.UUID, in ShareSet) (Share, error) {
	tx, p, v, err := e.request(ctx)
	if err != nil {
		return Share{}, err
	}
	if err := in.check(); err != nil {
		return Share{}, err
	}
	r, err := owned(ctx, tx, v, report, true)
	if err != nil {
		return Share{}, err
	}
	if r.Status != Active {
		return Share{}, errRemoved(r.Title)
	}
	if err := e.grantee(ctx, r, in); err != nil {
		return Share{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO analytics.report_share (tenant_id, id, report_id, principal_id, role_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, p.TenantID, id, r.ID, in.PrincipalID, in.RoleID, p.ID); err != nil {
		return Share{}, fmt.Errorf("analytics: share report %s: %w", r.ID, err)
	}
	s, err := share(ctx, tx, r.ID, id, false)
	if err != nil {
		return Share{}, err
	}
	return s, recordShare(ctx, "create", s)
}

// EndShare ends a share of one of the caller's reports, once, with the
// caller's version of it: from then on it reaches no one.
func (e *Endpoints) EndShare(ctx context.Context, report, id uuid.UUID, ifMatch func(int32) error) (Share, error) {
	tx, p, v, err := e.request(ctx)
	if err != nil {
		return Share{}, err
	}
	if _, err := owned(ctx, tx, v, report, false); err != nil {
		return Share{}, err
	}
	s, err := share(ctx, tx, report, id, true)
	if err != nil {
		return Share{}, err
	}
	if err := ifMatch(s.Version); err != nil {
		return Share{}, err
	}
	if s.Status != ShareActive {
		return Share{}, problem.Conflict("analytics.report_share_ended", "The share is ended already: it reaches no one.")
	}
	if _, err := tx.Exec(ctx, `UPDATE analytics.report_share SET status = 'ended', version = version + 1, updated_by = $2
		WHERE id = $1`, id, p.ID); err != nil {
		return Share{}, fmt.Errorf("analytics: end share %s of report %s: %w", id, report, err)
	}
	ended, err := share(ctx, tx, report, id, false)
	if err != nil {
		return Share{}, err
	}
	return ended, recordShare(ctx, "end", ended)
}

var shareSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// Shares lists a page of the shares of one of the caller's reports, the
// earliest first, filtered by status ("": every status).
func (e *Endpoints) Shares(ctx context.Context, report uuid.UUID, status string, page pagination.Request) ([]Share, *pagination.Keyset, error) {
	tx, _, v, err := e.request(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := owned(ctx, tx, v, report, false); err != nil {
		return nil, nil, err
	}
	ks, err := e.Pages.Keyset(shareSort, fmt.Sprintf("listReportShares|%s|%s", report, status), page)
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: page of report shares: %w", err)
	}
	q, args := ks.SQL(shareSelect+` WHERE report_id = $1 AND ($2 = '' OR status = $2)`, []any{report, status})
	rows, _ := tx.Query(ctx, q, args...)
	shares, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Share, error) { return scanShare(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("analytics: list shares of report %s: %w", report, err)
	}
	return shares, ks, nil
}

// recordShare adds the audit record of a share's change (ADR-0011); a
// share has no event: nothing outside the context reads it.
func recordShare(ctx context.Context, action string, s Share) error {
	if err := audit.Record(ctx, "analytics.report_share."+action, "analytics.report_share", s.ID,
		map[string]any{"report_id": s.ReportID, "principal_id": s.PrincipalID, "role_id": s.RoleID}); err != nil {
		return fmt.Errorf("analytics: record %s of share %s: %w", action, s.ID, err)
	}
	return nil
}

// shareNames are the names of the people and roles shares are with.
type shareNames struct {
	people map[uuid.UUID]string
	roles  map[uuid.UUID]Role
}

// namesOf reads the names of the shares' people and roles, one call each.
func (e *Endpoints) namesOf(ctx context.Context, shares []Share) (shareNames, error) {
	var people, roles []uuid.UUID
	for _, s := range shares {
		if s.PrincipalID != nil {
			people = append(people, *s.PrincipalID)
		} else {
			roles = append(roles, *s.RoleID)
		}
	}
	names, err := e.People.Names(ctx, people)
	if err != nil {
		return shareNames{}, fmt.Errorf("analytics: names of %d people shared with: %w", len(people), err)
	}
	named, err := e.Grantees.Roles(ctx, roles)
	if err != nil {
		return shareNames{}, fmt.Errorf("analytics: %d roles shared with: %w", len(roles), err)
	}
	return shareNames{people: names, roles: named}, nil
}

// shareToAPI is a share as the API gives it, with whom it is with named.
func shareToAPI(s Share, n shareNames) gen.ReportShare {
	out := gen.ReportShare{ID: s.ID, Version: s.Version, CreatedAt: s.CreatedAt, CreatedBy: s.CreatedBy, UpdatedAt: s.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(s.UpdatedBy), ReportID: s.ReportID, ReportTitle: s.ReportTitle, Status: s.Status}
	if id := s.PrincipalID; id != nil {
		out.PrincipalID, out.PrincipalName = gen.NewOptUUID(*id), gen.NewOptString(n.people[*id])
	} else {
		r := n.roles[*s.RoleID]
		out.RoleID, out.RoleCode, out.RoleName = gen.NewOptUUID(*s.RoleID), gen.NewOptString(r.Code), gen.NewOptString(r.Name)
	}
	return out
}

// oneShare is a single share as the API gives it, with its ETag.
func (e *Endpoints) oneShare(ctx context.Context, s Share, err error) (*gen.ReportShareHeaders, error) {
	if err != nil {
		return nil, err
	}
	n, err := e.namesOf(ctx, []Share{s})
	if err != nil {
		return nil, err
	}
	return &gen.ReportShareHeaders{ETag: gen.NewOptString(etag.Format(s.Version)), Response: shareToAPI(s, n)}, nil
}

// CreateReportShare implements POST /v1/reports/{id}/shares.
func (e *Endpoints) CreateReportShare(ctx context.Context, req *gen.ReportShareCreate, params gen.CreateReportShareParams) (gen.CreateReportShareRes, error) {
	s, err := e.CreateShare(ctx, params.ID, ShareSet{PrincipalID: apitypes.Ptr[uuid.UUID](req.PrincipalID), RoleID: apitypes.Ptr[uuid.UUID](req.RoleID)})
	return e.oneShare(ctx, s, err)
}

// EndReportShare implements POST /v1/reports/{id}/shares/{share_id}:end.
func (e *Endpoints) EndReportShare(ctx context.Context, params gen.EndReportShareParams) (gen.EndReportShareRes, error) {
	s, err := e.EndShare(ctx, params.ID, params.ShareID, etag.IfMatch(params.IfMatch))
	return e.oneShare(ctx, s, err)
}

// ListReportShares implements GET /v1/reports/{id}/shares.
func (e *Endpoints) ListReportShares(ctx context.Context, params gen.ListReportSharesParams) (gen.ListReportSharesRes, error) {
	shares, ks, err := e.Shares(ctx, params.ID, params.Status.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	items, next, err := apitypes.PageWith(ks, shares, func(s Share) (string, uuid.UUID) { return pagination.Instant(s.CreatedAt), s.ID },
		func(page []Share) (shareNames, error) { return e.namesOf(ctx, page) }, shareToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ReportSharePage{Items: items, NextPageToken: next}, nil
}
