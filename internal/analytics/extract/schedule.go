package extract

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// changing are the contexts whose events can change a dataset.
var changing = []string{"tenancy", "partner", "ledger", "procurement", "payables"}

// MarkChanged marks the tenants of a relayed batch of a context's events
// due an extract (ADR-0044). It is the outbox relay's Relayed hook: it runs
// in the transaction that marks the batch published, as the relay's role,
// once per batch, never in the transactions that write the data. The mark
// takes the clock at the insert, after the relay read the events and so
// after their data committed: a run claimed later sees that data.
func MarkChanged(ctx context.Context, tx pgx.Tx, schema string, tenants []uuid.UUID) error {
	if !slices.Contains(changing, schema) {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO analytics.extract_due AS d (tenant_id, changed_at) SELECT t, clock_timestamp() FROM (SELECT DISTINCT unnest($1::uuid[])) u (t) ORDER BY t
		ON CONFLICT (tenant_id) DO UPDATE SET changed_at = greatest(d.changed_at, excluded.changed_at)`, tenants); err != nil {
		return fmt.Errorf("analytics: mark %d tenants changed: %w", len(tenants), err)
	}
	return nil
}

// Scheduler runs the extracts of the tenants marked changed: each at most
// once per Every, so a busy tenant is extracted every Every and a quiet one
// not at all. Replicas claim tenants with FOR UPDATE SKIP LOCKED; a claim
// is its run's lease, so a run that failed or crashed is retried after
// Every. The database's clock times the claims, as it times the marks. A
// claim records the primary's WAL position, past which the run waits for
// the replica to have replayed before it reads there (Extractor.RunAt).
//
// After a tenant's run it collects the files the tenant's superseded
// manifests left (Collect), and a run that finds the watermark ahead of
// the database rebuilds the tenant's store when the primary is behind it
// too (a restore) and is retried when only the replica was. Every
// SweepEvery it marks due every active tenant whose store was not found
// current within the freshness target, so a lost event delays data and
// never loses it (ADR-0044).
type Scheduler struct {
	Extractor  Extractor
	Begin      database.BeginFunc // the primary
	WorkerRole string             // erp_worker: claims across tenants
	Every      time.Duration      // at most one run per tenant per Every; default 5 minutes
	Interval   time.Duration      // idle wait between rounds; default 30 seconds
	SweepEvery time.Duration      // between sweeps; default 1 hour; negative: never (tests whose database other tests share)
	Target     time.Duration      // the freshness target the sweep applies; default 15 minutes
	Now        func() time.Time   // when the last sweep ran; nil: the wall clock
	Log        worker.Logger

	swept time.Time // the last sweep of this scheduler
}

func (s *Scheduler) every() time.Duration {
	if s.Every <= 0 {
		return 5 * time.Minute
	}
	return s.Every
}

// RunOnce sweeps when one is due, then runs every due tenant's extract and
// returns how many ran. A tenant whose run fails is left for its next
// claim; the others go on.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	if err := s.sweepIfDue(ctx); err != nil {
		errs = append(errs, err)
	}
	claimed := []uuid.UUID{} // not nil, which would be NULL to the query; once per round: a run longer than Every does not claim its tenant again
	for {
		c, ok, err := s.claim(ctx, claimed)
		if err != nil || !ok {
			return n, errors.Join(append(errs, err)...)
		}
		claimed = append(claimed, c.tenant)
		ran, err := s.runTenant(ctx, c)
		if ran {
			n++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
}

// runTenant runs a claimed tenant's extract, rebuilding its store first
// when the database is behind it, records the run done unless entries wait
// above its cutoff, and collects what superseded manifests left.
func (s *Scheduler) runTenant(ctx context.Context, c claim) (bool, error) {
	res, err := s.Extractor.RunAt(ctx, c.tenant, c.lsn)
	if errors.Is(err, ErrBehind) {
		if err = s.rebuildIfAhead(ctx, c.tenant, err); err == nil {
			res, err = s.Extractor.RunAt(ctx, c.tenant, c.lsn)
		}
	}
	if err != nil {
		return false, err
	}
	// Entries waiting above the cutoff keep the tenant due.
	if !res.Pending {
		err = s.done(ctx, c.tenant, c.started)
	}
	if _, cerr := s.Extractor.Collect(ctx, c.tenant); cerr != nil {
		err = errors.Join(err, cerr)
	}
	return true, err
}

// rebuildIfAhead rebuilds the tenant's store when its watermark is ahead
// of the primary (the database restored under it); ahead of the replica
// alone, the replica lags and behind is returned, for a later claim.
func (s *Scheduler) rebuildIfAhead(ctx context.Context, tenant uuid.UUID, behind error) error {
	ahead, err := readAhead(ctx, s.Begin, s.WorkerRole)
	if err != nil {
		return err
	}
	if !slices.Contains(ahead, tenant) {
		return behind
	}
	return Rebuild(ctx, s.Begin, s.WorkerRole, s.Extractor.Store, tenant)
}

// sweepIfDue marks stale tenants due once per SweepEvery of this
// scheduler; replicas sweeping together mark the same tenants, which is
// one mark.
func (s *Scheduler) sweepIfDue(ctx context.Context) error {
	every := s.SweepEvery
	switch {
	case every < 0:
		return nil
	case every == 0:
		every = time.Hour
	}
	now := clock.Now(s.Now)
	if !s.swept.IsZero() && now.Sub(s.swept) < every {
		return nil
	}
	if _, err := Sweep(ctx, s.Begin, s.WorkerRole, s.Target); err != nil {
		return err
	}
	s.swept = now
	return nil
}

// Sweep marks due every active tenant whose store was not found current
// within target (default 15 minutes), its pointer missing or its
// checked_at older, and returns how many it marked.
func Sweep(ctx context.Context, begin database.BeginFunc, workerRole string, target time.Duration) (int64, error) {
	if target <= 0 {
		target = 15 * time.Minute
	}
	var n int64
	job := uow.Job{Begin: begin, Name: "analytics: sweep", Scope: database.Scope{Role: workerRole}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		tag, err := tx.Exec(ctx, `INSERT INTO analytics.extract_due AS d (tenant_id, changed_at)
			SELECT t.id, clock_timestamp() FROM tenancy.tenant t
			WHERE t.status = 'active' AND NOT EXISTS (SELECT FROM analytics.snapshot s WHERE s.tenant_id = t.id
				AND s.checked_at >= now() - $1 * interval '1 millisecond')
			ORDER BY t.id
			ON CONFLICT (tenant_id) DO UPDATE SET changed_at = greatest(d.changed_at, excluded.changed_at)`, target.Milliseconds())
		if err != nil {
			return false, fmt.Errorf("analytics: sweep stale tenants: %w", err)
		}
		n = tag.RowsAffected()
		return true, nil
	})
	return n, err
}

// Run runs due extracts until ctx ends; errors are logged and retried.
func (s *Scheduler) Run(ctx context.Context) {
	worker.Loop(ctx, s.Interval, 30*time.Second, func(ctx context.Context) (bool, error) {
		_, err := s.RunOnce(ctx)
		return false, err
	}, s.Log, "analytics extract failed")
}

// claim is a tenant's claimed run: when it was claimed, and the primary's
// WAL position then.
type claim struct {
	tenant  uuid.UUID
	started time.Time
	lsn     string
}

// claim takes the tenant changed longest ago that is due and not claimed in
// the last Every, other than those this round claimed, and records the
// claim's time and WAL position.
func (s *Scheduler) claim(ctx context.Context, claimed []uuid.UUID) (claim, bool, error) {
	var c claim
	var ok bool
	job := uow.Job{Begin: s.Begin, Name: "analytics: claim", Scope: database.Scope{Role: s.WorkerRole}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		err := tx.QueryRow(ctx, `UPDATE analytics.extract_due d SET started_at = now(), claimed_lsn = pg_current_wal_lsn() FROM (
				SELECT tenant_id FROM analytics.extract_due
				WHERE changed_at > coalesce(done_at, '-infinity') AND coalesce(started_at, '-infinity') <= now() - $1 * interval '1 millisecond'
					AND tenant_id <> ALL($2::uuid[])
				ORDER BY changed_at LIMIT 1 FOR UPDATE SKIP LOCKED) due
			WHERE d.tenant_id = due.tenant_id RETURNING d.tenant_id, d.started_at, d.claimed_lsn::text`, s.every().Milliseconds(), claimed).Scan(&c.tenant, &c.started, &c.lsn)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("analytics: claim an extract: %w", err)
		}
		ok = true
		return true, nil
	})
	return c, ok, err
}

// done records a finished run: the changes marked before its claim are in
// the store. A claim taken over since (the run outlasted Every) is left to
// its own run.
func (s *Scheduler) done(ctx context.Context, tenant uuid.UUID, started time.Time) error {
	job := uow.Job{Begin: s.Begin, Name: "analytics: done", Scope: database.Scope{Role: s.WorkerRole}}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, `UPDATE analytics.extract_due SET done_at = started_at WHERE tenant_id = $1 AND started_at = $2`,
			tenant, started); err != nil {
			return false, fmt.Errorf("analytics: record the extract of tenant %s: %w", tenant, err)
		}
		return true, nil
	})
}
