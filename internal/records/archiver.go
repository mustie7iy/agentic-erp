package records

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Archiver writes the archiving runs queued (ADR-0079 §6-§7, T99b), as the
// cash exporter sends exports (internal/cash/exporter.go): a round finds
// the tenants with queued runs, then, per run, claims it in a
// transaction of its tenant (FOR UPDATE SKIP LOCKED, so replicas share
// the queue), checks that its requester may still act and that its year
// is still closed, builds the file and its manifest, writes both to
// object storage and completes the run, all in that transaction. Objects
// are written once under the run's id: a round run again after a commit
// whose acknowledgement was lost finds the run still queued and the same
// bytes under its keys, which it takes as written, so a run has one file
// and is completed and published once. A business problem fails the run;
// the store not answering leaves it queued for the next round.
type Archiver struct {
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

// ArchiverActor is the principal of the archiver's changes, in every
// tenant; who asked for a run is kept on it.
var ArchiverActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:records-archiver"))

func (w *Archiver) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, w.Pool, w.Begin, s)
}

// tenants returns the tenants with queued runs.
func (w *Archiver) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM records.archiving_run WHERE status = 'queued' AND tenant_id IN `+database.ActiveTenants)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("records: list the tenants with queued archiving runs: %w", err)
	}
	return tenants, nil
}

// RunOnce writes every queued run and returns how many it settled. A
// tenant whose run fails on the machinery is left for the next round;
// the others go on.
func (w *Archiver) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("records archiver: %w", err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		for ctx.Err() == nil { // a stopping worker claims nothing more
			did, err := w.one(ctx, t)
			if err != nil {
				errs = append(errs, fmt.Errorf("records archiver: tenant %s: %w", t, err))
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

// Run writes runs until ctx ends; errors are logged and retried after the
// interval.
func (w *Archiver) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, 5*time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "records archiving failed")
}

// one settles the tenant's next queued run and reports whether there was
// one.
func (w *Archiver) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	did := false
	err := uow.Job{Begin: w.begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "records: archiving run", Now: w.Now}.Tx(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE status = 'queued' ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`))
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("records: claim an archiving run: %w", err)
			}
			did = true
			actor := reqctx.Principal{ID: ArchiverActor, Kind: reqctx.KindSystem, TenantID: tenant}
			return true, uow.Act(ctx, tx, actor, w.Now, func(ctx context.Context) error { return w.settle(ctx, tx, r, w.archive(ctx, tx, tenant, r)) })
		})
	return did, err
}

// settle fails the run on a business problem (one below 500), which
// another round would meet again; any other error is the machinery's,
// returned so that the run stays queued.
func (w *Archiver) settle(ctx context.Context, tx pgx.Tx, r Run, err error) error {
	var p *problem.Problem
	if err == nil || !errors.As(err, &p) || p.Status >= 500 {
		return err
	}
	if w.Log != nil {
		w.Log.WarnContext(ctx, "archiving run failed", "archiving_run", r.ID, "code", p.Code, "detail", p.Detail)
	}
	if _, err := tx.Exec(ctx, `UPDATE records.archiving_run SET status = 'failed', failure_code = $2, version = version + 1 WHERE id = $1`, r.ID, p.Code); err != nil {
		return fmt.Errorf("records: fail archiving run %s: %w", r.ID, err)
	}
	return audit.Record(ctx, "records.archiving_run.fail", "records.archiving_run", r.ID, map[string]any{"legal_entity_id": r.LegalEntity,
		"document_kind": r.Kind, "fiscal_year": r.FiscalYear, "code": p.Code, "detail": p.Detail, "requested_by": r.RequestedBy})
}

// archive writes the run's file and manifest and completes it: once its
// requester may still act and its year is still closed.
func (w *Archiver) archive(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, r Run) error {
	e := w.Endpoints
	if err := w.Requesters.MayAct(ctx, tx, r.RequestedBy); err != nil {
		return fmt.Errorf("records: requester of archiving run %s: %w", r.ID, err)
	}
	le, found, err := e.LegalEntities.Entity(ctx, r.LegalEntity)
	if err != nil {
		return fmt.Errorf("records: company of archiving run %s: %w", r.ID, err)
	}
	if !found {
		return fmt.Errorf("records: company %s of archiving run %s is not held by the tenancy context", r.LegalEntity, r.ID)
	}
	closed, err := e.Journal.YearClosed(ctx, r.LegalEntity, r.FiscalYear)
	if err != nil {
		return fmt.Errorf("records: is fiscal year %d of company %s closed: %w", r.FiscalYear, le.Code, err)
	}
	if !closed {
		return yearNotClosed(le.Code, r.FiscalYear)
	}
	a, err := e.build(ctx, tx, tenant, r, le)
	if err != nil {
		return err
	}
	if err := e.keep(ctx, a.key, a.content, "application/x-ndjson"); err != nil {
		return err
	}
	if err := e.keep(ctx, a.metaKey, a.manifest, "application/json"); err != nil {
		return err
	}
	return w.complete(ctx, tx, r, le.Code, a)
}

// keep writes an object under its key. A key that holds the same bytes
// already was written by this run's earlier round, whose transaction was
// lost: it is taken as written. Other bytes there refuse the run.
func (e *Endpoints) keep(ctx context.Context, key string, content []byte, contentType string) error {
	err := e.Objects.Put(ctx, key, content, blobstore.PutOptions{ContentType: contentType})
	if err == nil {
		return nil
	}
	if !errors.Is(err, blobstore.ErrExists) {
		return problem.Unavailable("The file store did not take the archive; the archiver tries again.", 30).WithCause(err)
	}
	stored, _, err := e.Objects.Get(ctx, key)
	if err != nil {
		return problem.Unavailable("The file store did not return the archive it holds; the archiver tries again.", 30).WithCause(err)
	}
	if !bytes.Equal(stored, content) {
		return archiveChanged(key)
	}
	return nil
}

// complete marks the run completed with its file's figures, audits and
// publishes it.
func (w *Archiver) complete(ctx context.Context, tx pgx.Tx, r Run, company string, a archive) error {
	if _, err := tx.Exec(ctx, `UPDATE records.archiving_run SET status = 'completed', object_key = $2, manifest_key = $3, sha256 = $4, manifest_sha256 = $5,
		size_bytes = $6, entry_count = $7, line_count = $8, debit_total = $9, currency = $10, completed_at = $11, version = version + 1 WHERE id = $1`,
		r.ID, a.key, a.metaKey, a.sum[:], a.manifestSum[:], len(a.content), a.entries, a.lines, a.debits, a.currency, clock.Now(w.Now)); err != nil {
		return fmt.Errorf("records: complete archiving run %s: %w", r.ID, err)
	}
	exp, err := currency.Exponent(ctx, tx, a.currency, nil)
	if err != nil {
		return fmt.Errorf("records: archiving run %s: %w", r.ID, err)
	}
	sum, meta := hexOf(a.sum[:]), hexOf(a.manifestSum[:])
	return audit.Record(ctx, "records.archiving_run.complete", "records.archiving_run", r.ID, map[string]any{"legal_entity_id": r.LegalEntity,
		"document_kind": r.Kind, "fiscal_year": r.FiscalYear, "file_name": fileName(r.ID), "sha256": sum, "manifest_sha256": meta,
		"size_bytes": len(a.content), "entry_count": a.entries, "line_count": a.lines, "requested_by": r.RequestedBy},
		outbox.Event{Context: "records", Name: "archiving_run_completed", AggregateType: "records.archiving_run", AggregateID: r.ID,
			Payload: map[string]any{"archiving_run_id": r.ID, "legal_entity_id": r.LegalEntity, "legal_entity_code": company, "document_kind": r.Kind,
				"fiscal_year": r.FiscalYear, "file_name": fileName(r.ID), "sha256": sum, "size_bytes": len(a.content), "manifest_sha256": meta,
				"entry_count": a.entries, "line_count": a.lines, "debit_total": money.EventAmount(a.debits, a.currency, exp), "requested_by": r.RequestedBy}})
}
