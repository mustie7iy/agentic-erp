package extract

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
// Every. The database's clock times the claims, as it times the marks.
// Until the extract waits for the replica to replay the claim's position
// (T55f), its Replica must be the primary: a lagging replica would find
// nothing new, and the run would mark the change done (ADR-0044).
type Scheduler struct {
	Extractor  Extractor
	Begin      database.BeginFunc // the primary
	WorkerRole string             // erp_worker: claims across tenants
	Every      time.Duration      // at most one run per tenant per Every; default 5 minutes
	Interval   time.Duration      // idle wait between rounds; default 30 seconds
	Log        worker.Logger
}

func (s *Scheduler) every() time.Duration {
	if s.Every <= 0 {
		return 5 * time.Minute
	}
	return s.Every
}

// RunOnce runs every due tenant's extract and returns how many ran. A tenant
// whose run fails is left for its next claim; the others go on.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	claimed := []uuid.UUID{} // not nil, which would be NULL to the query; once per round: a run longer than Every does not claim its tenant again
	for {
		tenant, started, ok, err := s.claim(ctx, claimed)
		if err != nil || !ok {
			return n, errors.Join(append(errs, err)...)
		}
		claimed = append(claimed, tenant)
		res, err := s.Extractor.Run(ctx, tenant)
		if err == nil {
			n++
			// Entries waiting above the cutoff keep the tenant due.
			if !res.Pending {
				err = s.done(ctx, tenant, started)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
}

// Run runs due extracts until ctx ends; errors are logged and retried.
func (s *Scheduler) Run(ctx context.Context) {
	worker.Loop(ctx, s.Interval, 30*time.Second, func(ctx context.Context) (bool, error) {
		_, err := s.RunOnce(ctx)
		return false, err
	}, s.Log, "analytics extract failed")
}

// claim takes the tenant changed longest ago that is due and not claimed in
// the last Every, other than those this round claimed, and records the
// claim's time.
func (s *Scheduler) claim(ctx context.Context, claimed []uuid.UUID) (uuid.UUID, time.Time, bool, error) {
	var tenant uuid.UUID
	var started time.Time
	var ok bool
	job := uow.Job{Begin: s.Begin, Name: "analytics: claim", Scope: database.Scope{Role: s.WorkerRole}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		err := tx.QueryRow(ctx, `UPDATE analytics.extract_due d SET started_at = now() FROM (
				SELECT tenant_id FROM analytics.extract_due
				WHERE changed_at > coalesce(done_at, '-infinity') AND coalesce(started_at, '-infinity') <= now() - $1 * interval '1 millisecond'
					AND tenant_id <> ALL($2::uuid[])
				ORDER BY changed_at LIMIT 1 FOR UPDATE SKIP LOCKED) due
			WHERE d.tenant_id = due.tenant_id RETURNING d.tenant_id, d.started_at`, s.every().Milliseconds(), claimed).Scan(&tenant, &started)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("analytics: claim an extract: %w", err)
		}
		ok = true
		return true, nil
	})
	return tenant, started, ok, err
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
