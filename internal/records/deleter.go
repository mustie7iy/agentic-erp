package records

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Deleter blanks the subjects of the deletion runs queued (ADR-0079 §12,
// T100c), as the Archiver writes archiving runs: a round finds the
// tenants with a queued run, then settles one subject per transaction of
// its tenant. Each transaction claims the run (FOR UPDATE SKIP LOCKED, so
// replicas share the queue), checks that its requester may still act,
// takes its next pending subject FOR UPDATE, blanks the partner through
// the partner context's port, and marks the subject blanked (audited and
// published as PersonalDataDeleted) or skipped when its block was lifted
// or set again since; with no subject pending it completes the run. A
// round run again after a commit whose acknowledgement was lost finds the
// subject settled and goes on with the next, so a subject is blanked and
// published once. A business problem fails the run; any other error
// leaves it queued for the next round.
type Deleter struct {
	Endpoints  *Endpoints
	Requesters Requesters
	Pool       *database.Pool
	AppRole    string        // erp_app: claims and writes in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants with queued runs
	Interval   time.Duration // idle wait between rounds; default 5s
	Log        *slog.Logger  // where failed rounds are reported; nil: not reported
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

// DeleterActor is the principal of the deleter's changes, in every
// tenant; who asked for a run is kept on it.
var DeleterActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:records-deleter"))

func (w *Deleter) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, w.Pool, w.Begin, s)
}

// tenants returns the tenants with a queued deletion run.
func (w *Deleter) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM records.deletion_run WHERE status = 'queued' AND tenant_id IN `+database.ActiveTenants)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("records: list the tenants with queued deletion runs: %w", err)
	}
	return tenants, nil
}

// RunOnce settles every pending subject and run and returns how many
// steps it took. A tenant whose step fails on the machinery is left for
// the next round; the others go on.
func (w *Deleter) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("records deleter: %w", err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		for ctx.Err() == nil { // a stopping worker claims nothing more
			did, err := w.one(ctx, t)
			if err != nil {
				errs = append(errs, fmt.Errorf("records deleter: tenant %s: %w", t, err))
				break
			}
			if !did {
				break
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}

// Run settles runs until ctx ends; errors are logged and retried after
// the interval.
func (w *Deleter) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, 5*time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "records deletion failed")
}

// one takes the next step of the tenant's queued run, a subject or the
// run's completion, and reports whether there was one.
func (w *Deleter) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	did := false
	err := uow.Job{Begin: w.begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "records: deletion run", Now: w.Now}.Tx(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			r, err := scanDeletionRun(tx.QueryRow(ctx, deletionRunSelect+` WHERE status = 'queued' ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`))
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("records: claim a deletion run: %w", err)
			}
			did = true
			actor := reqctx.Principal{ID: DeleterActor, Kind: reqctx.KindSystem, TenantID: tenant}
			return true, uow.Act(ctx, tx, actor, w.Now, func(ctx context.Context) error { return w.settle(ctx, tx, r, w.step(ctx, tx, r)) })
		})
	return did, err
}

// settle fails the run on a business problem (one below 500), which
// another round would meet again; any other error is the machinery's,
// returned so that the run stays queued.
func (w *Deleter) settle(ctx context.Context, tx pgx.Tx, r DeletionRun, err error) error {
	var p *problem.Problem
	if err == nil || !errors.As(err, &p) || p.Status >= 500 {
		return err
	}
	if w.Log != nil {
		w.Log.WarnContext(ctx, "deletion run failed", "deletion_run", r.ID, "code", p.Code, "detail", p.Detail)
	}
	if _, err := tx.Exec(ctx, `UPDATE records.deletion_run SET status = 'failed', failure_code = $2, version = version + 1 WHERE id = $1`, r.ID, p.Code); err != nil {
		return fmt.Errorf("records: fail deletion run %s: %w", r.ID, err)
	}
	return audit.Record(ctx, "records.deletion_run.fail", "records.deletion_run", r.ID, map[string]any{"code": p.Code, "detail": p.Detail,
		"requested_by": r.RequestedBy})
}

// step settles the run's next pending subject, by number, or completes
// the run when none is pending: once its requester may still act.
func (w *Deleter) step(ctx context.Context, tx pgx.Tx, r DeletionRun) error {
	if err := w.Requesters.MayAct(ctx, tx, r.RequestedBy); err != nil {
		return fmt.Errorf("records: requester of deletion run %s: %w", r.ID, err)
	}
	var s Subject
	err := tx.QueryRow(ctx, `SELECT partner_id, partner_number, blocked_at FROM records.deletion_run_subject
		WHERE run_id = $1 AND status = 'pending' ORDER BY partner_number, partner_id LIMIT 1 FOR UPDATE`, r.ID).Scan(&s.PartnerID, &s.Number, &s.BlockedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w.complete(ctx, tx, r)
	}
	if err != nil {
		return fmt.Errorf("records: the next subject of deletion run %s: %w", r.ID, err)
	}
	blanked, err := w.Endpoints.Privacy.Blank(ctx, s.PartnerID, s.BlockedAt)
	if err != nil {
		return fmt.Errorf("records: blank business partner %s of deletion run %s: %w", s.Number, r.ID, err)
	}
	status, action := SubjectBlanked, "records.deletion_run.blank"
	if !blanked {
		status, action = SubjectSkipped, "records.deletion_run.skip"
	}
	if _, err := tx.Exec(ctx, `UPDATE records.deletion_run_subject SET status = $3, settled_at = $4 WHERE run_id = $1 AND partner_id = $2`,
		r.ID, s.PartnerID, status, clock.Now(w.Now)); err != nil {
		return fmt.Errorf("records: settle business partner %s of deletion run %s: %w", s.Number, r.ID, err)
	}
	detail := map[string]any{"partner_id": s.PartnerID, "partner_number": s.Number, "requested_by": r.RequestedBy}
	if !blanked {
		return audit.Record(ctx, action, "records.deletion_run", r.ID, detail)
	}
	return audit.Record(ctx, action, "records.deletion_run", r.ID, detail,
		outbox.Event{Context: "records", Name: "personal_data_deleted", AggregateType: "partner.business_partner", AggregateID: s.PartnerID,
			Payload: map[string]any{"deletion_run_id": r.ID, "partner_id": s.PartnerID, "requested_by": r.RequestedBy}})
}

// complete marks the run completed, its subjects all settled, and audits
// it with their counts.
func (w *Deleter) complete(ctx context.Context, tx pgx.Tx, r DeletionRun) error {
	var blanked, skipped int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'blanked'), count(*) FILTER (WHERE status = 'skipped')
		FROM records.deletion_run_subject WHERE run_id = $1`, r.ID).Scan(&blanked, &skipped); err != nil {
		return fmt.Errorf("records: count the subjects of deletion run %s: %w", r.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE records.deletion_run SET status = 'completed', completed_at = $2, version = version + 1 WHERE id = $1`,
		r.ID, clock.Now(w.Now)); err != nil {
		return fmt.Errorf("records: complete deletion run %s: %w", r.ID, err)
	}
	return audit.Record(ctx, "records.deletion_run.complete", "records.deletion_run", r.ID, map[string]any{"subject_count": r.Count,
		"blanked": blanked, "skipped": skipped, "requested_by": r.RequestedBy})
}
