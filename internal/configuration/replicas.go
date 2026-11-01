package configuration

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Mixed versions (ADR-0038 rollout, 06 §6.4): during a rolling upgrade an
// older replica reads the active document tolerantly and ignores a key its
// version does not know, so a control such as companies[].release_dual_limit
// would hold on some replicas and not on others (an older :release has no
// approval check). Every replica therefore records the keys it understands
// in config.replica (Replica), and an activation whose document sets a key
// some running replica lacks is refused (409 config.replica_lacks_key)
// until that replica is upgraded or gone.

const (
	// ReplicaInterval is how often a replica records that it runs.
	ReplicaInterval = 30 * time.Second
	// ReplicaExpiry is how long after its last record a replica that
	// stopped without deleting its row (a crash) still counts; until then
	// an activation it would ignore a key of waits.
	ReplicaExpiry = 5 * time.Minute
	// leaveTimeout bounds deleting the row on stopping, well inside a pod's
	// termination grace: a row not deleted expires anyway.
	leaveTimeout = 10 * time.Second
)

// Replica records this replica, its version and the configuration keys it
// understands, while it runs. Register before serving; Run until stopping.
type Replica struct {
	Pool       *database.Pool
	WorkerRole string         // erp_worker: writes the cell's replicas
	Build      buildinfo.Info // the version recorded
	Interval   time.Duration  // between records; default ReplicaInterval
	Log        *slog.Logger
	// Now measures how long records have been failing; nil means
	// time.Now. Whether a record is current is the database's clock's
	// call alone, never a replica's.
	Now func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc

	id uuid.UUID
}

// Register records the replica, under an id of its own.
func (r *Replica) Register(ctx context.Context) error {
	r.id = uuid.Must(uuid.NewV7())
	return r.record(ctx)
}

// Run records the replica every Interval until ctx ends, then deletes its
// row so that it stops counting at once. A record that fails is tried at
// the next interval; once none has succeeded for half of ReplicaExpiry,
// Run fails, since the replica would soon stop counting while it still
// serves, and the server stops (a critical worker).
func (r *Replica) Run(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = ReplicaInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	last := r.now()
	for {
		select {
		case <-ctx.Done():
			r.leave(context.WithoutCancel(ctx))
			return nil
		case <-tick.C:
		}
		err := r.record(ctx)
		switch {
		case err == nil:
			last = r.now()
		case ctx.Err() != nil: // stopping: the next round leaves
		case r.now().Sub(last) >= ReplicaExpiry/2:
			return fmt.Errorf("configuration: the replica has not been recorded since %s: %w", last.UTC().Format(time.RFC3339), err)
		default:
			r.Log.WarnContext(ctx, "recording the replica failed; trying again", "error", err)
		}
	}
}

func (r *Replica) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// record writes the replica's row, sighted now, and removes the rows of
// replicas that no longer count.
func (r *Replica) record(ctx context.Context) error {
	tx, err := database.BeginOr(ctx, r.Pool, r.Begin, database.Scope{Role: r.WorkerRole})
	if err != nil {
		return fmt.Errorf("configuration: record the replica: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `INSERT INTO config.replica (id, version, revision, config_keys) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET seen_at = now()`, r.id, r.Build.Version, r.Build.Revision, config.Keys()); err != nil {
		return fmt.Errorf("configuration: record the replica: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM config.replica WHERE seen_at < now() - make_interval(secs => $1)`, ReplicaExpiry.Seconds()); err != nil {
		return fmt.Errorf("configuration: remove the replicas that stopped: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("configuration: record the replica: %w", err)
	}
	return nil
}

// leave deletes the replica's row; if that fails, the row stops counting
// at its expiry, which is why the failure is only logged.
func (r *Replica) leave(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, leaveTimeout)
	defer cancel()
	tx, err := database.BeginOr(ctx, r.Pool, r.Begin, database.Scope{Role: r.WorkerRole})
	if err == nil {
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `DELETE FROM config.replica WHERE id = $1`, r.id); err == nil {
			err = tx.Commit(ctx)
		}
	}
	if err != nil {
		r.Log.WarnContext(ctx, "removing the replica's record failed; it stops counting when it expires", "error", err, "expiry", ReplicaExpiry)
	}
}

// replicasKnow refuses a document that sets a key some running replica
// does not understand, naming each such key and the versions that lack
// it, with 409 config.replica_lacks_key.
func replicasKnow(ctx context.Context, tx pgx.Tx, doc *config.Config) error {
	rows, _ := tx.Query(ctx, `SELECT k, string_agg(DISTINCT r.name, ', ' ORDER BY r.name)
		FROM unnest($1::text[]) k JOIN (SELECT version || CASE WHEN revision = '' THEN '' ELSE ' (' || revision || ')' END AS name, config_keys
			FROM config.replica WHERE seen_at >= now() - make_interval(secs => $2)) r ON NOT k = ANY (r.config_keys)
		GROUP BY k ORDER BY k`, doc.Used(), ReplicaExpiry.Seconds())
	lacking, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key, Versions string }])
	if err != nil {
		return fmt.Errorf("configuration: compare the document's keys with the running replicas': %w", err)
	}
	if len(lacking) == 0 {
		return nil
	}
	errs := make([]problem.FieldError, len(lacking))
	names := make([]string, len(lacking))
	for i, l := range lacking {
		names[i] = l.Key
		errs[i] = problem.FieldError{Field: l.Key, Code: "replica_lacks_key",
			Message: "Running replicas of version " + l.Versions + " do not know this key and would ignore it."}
	}
	p := problem.Conflict("config.replica_lacks_key",
		"The configuration sets "+strings.Join(names, ", ")+", which a running replica's version does not know (it would read the configuration without it); activate it once every replica runs a version that knows it.")
	p.Errors = errs
	return p
}
