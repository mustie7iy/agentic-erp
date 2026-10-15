package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Scheduled verification (T21, ADR-0011): a worker recomputes every tenant's
// audit chain and checks it against its anchors in rounds, records each
// round in audit.verification, and raises an alarm (a log line, an event, a
// gauge) the first time a chain stops verifying. Verification on request
// (:verify) stays for operators; the worker turns tampering into something
// noticed without anyone asking.

// VerifierActor is the worker's id in events: a system actor.
var VerifierActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:audit-verifier"))

// Verifier verifies every tenant's chain in rounds.
type Verifier struct {
	Pool       *database.Pool
	Store      blobstore.Store // where anchors are; nil: anchors are not checked
	AppRole    string          // erp_app: verifies and records in the tenant's scope
	WorkerRole string          // erp_worker: finds tenants across the cell
	Interval   time.Duration   // between rounds; default 1 hour
	Log        *slog.Logger
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (v *Verifier) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, v.Pool, v.Begin, s)
}

func (v *Verifier) now() time.Time {
	return clock.Now(v.Now).UTC()
}

// Verification is one round's result for one tenant.
type Verification struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	VerifiedAt     time.Time `json:"verified_at"`
	HeadSeq        int64     `json:"head_seq"`
	Checked        int64     `json:"checked"`
	AnchorsChecked int       `json:"anchors_checked"`
	BrokenAt       int64     `json:"broken_at"` // 0 when intact
	OK             bool      `json:"ok"`
}

// tenants lists the tenants with a chain, as the worker role.
func (v *Verifier) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := v.begin(ctx, database.Scope{Role: v.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT tenant_id FROM audit.chain_head ORDER BY tenant_id`)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return tenants, fmt.Errorf("audit: list tenants with a chain: %w", err)
	}
	return tenants, nil
}

// Verify recomputes one tenant's whole chain, checks every anchor, records
// the result and, when the chain does not verify where the previous
// verification did, publishes AuditChainTampered.
func (v *Verifier) Verify(ctx context.Context, tenant uuid.UUID) (Verification, error) {
	tx, err := v.begin(ctx, database.Scope{Role: v.AppRole, TenantID: tenant})
	if err != nil {
		return Verification{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	res, err := trail.VerifyRange(ctx, tx, tenant, 1, 0)
	if err != nil {
		return Verification{}, err
	}
	anchors, brokenAnchor, err := checkAnchors(ctx, tx, v.Store, tenant, 1, 0)
	if err != nil {
		return Verification{}, err
	}
	if brokenAnchor != 0 && (res.BrokenAt == 0 || brokenAnchor < res.BrokenAt) {
		res.BrokenAt = brokenAnchor
	}
	ver := Verification{TenantID: tenant, VerifiedAt: v.now(), HeadSeq: res.LastSeq, Checked: res.Checked, AnchorsChecked: anchors, BrokenAt: res.BrokenAt, OK: res.BrokenAt == 0}
	var previouslyOK *bool
	if err := tx.QueryRow(ctx, `SELECT ok FROM audit.verification WHERE tenant_id = $1 ORDER BY verified_at DESC LIMIT 1`, tenant).Scan(&previouslyOK); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, fmt.Errorf("audit: read last verification of tenant %s: %w", tenant, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit.verification (tenant_id, verified_at, head_seq, checked, anchors_checked, broken_at, ok) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenant, ver.VerifiedAt, ver.HeadSeq, ver.Checked, ver.AnchorsChecked, nullable(ver.BrokenAt), ver.OK); err != nil {
		return Verification{}, fmt.Errorf("audit: record verification of tenant %s: %w", tenant, err)
	}
	if !ver.OK && (previouslyOK == nil || *previouslyOK) {
		// The alarm: once, when a chain that verified stops doing so.
		if _, err := outbox.Write(ctx, tx, tenant, VerifierActor, "", outbox.Event{Context: "platform", Name: "audit_chain_tampered", AggregateType: "audit.chain", AggregateID: tenant,
			Payload: map[string]any{"tenant_id": tenant, "broken_at": ver.BrokenAt, "head_seq": ver.HeadSeq, "verified_at": ver.VerifiedAt}}); err != nil {
			return Verification{}, err
		}
		v.Log.ErrorContext(ctx, "audit chain does not verify", "tenant_id", tenant, "broken_at", ver.BrokenAt, "head_seq", ver.HeadSeq)
	}
	if err := tx.Commit(ctx); err != nil {
		return Verification{}, fmt.Errorf("audit: commit verification of tenant %s: %w", tenant, err)
	}
	return ver, nil
}

func nullable(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// LatestVerifications returns the latest scheduled verification of each of
// the tenants that has one (what the control plane shows of a tenant's
// chain), in the caller's transaction.
func LatestVerifications(ctx context.Context, tx pgx.Tx, tenants []uuid.UUID) (map[uuid.UUID]Verification, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT ON (tenant_id) tenant_id, verified_at, head_seq, checked, anchors_checked, coalesce(broken_at, 0), ok
		FROM audit.verification WHERE tenant_id = ANY($1) ORDER BY tenant_id, verified_at DESC`, tenants)
	latest := map[uuid.UUID]Verification{}
	var v Verification
	if _, err := pgx.ForEachRow(rows, []any{&v.TenantID, &v.VerifiedAt, &v.HeadSeq, &v.Checked, &v.AnchorsChecked, &v.BrokenAt, &v.OK}, func() error {
		latest[v.TenantID] = v
		return nil
	}); err != nil {
		return nil, fmt.Errorf("audit: read the latest verifications of %d tenants: %w", len(tenants), err)
	}
	return latest, nil
}

// RunOnce verifies every tenant and returns how many verified and how many
// did not; the first database failure stops the round.
func (v *Verifier) RunOnce(ctx context.Context) (ok, broken int, err error) {
	tenants, err := v.tenants(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range tenants {
		ver, err := v.Verify(ctx, t)
		if err != nil {
			return ok, broken, fmt.Errorf("verifying tenant %s: %w", t, err)
		}
		if ver.OK {
			ok++
		} else {
			broken++
		}
	}
	return ok, broken, nil
}

// Run rounds until ctx ends, logging failures.
func (v *Verifier) Run(ctx context.Context) {
	worker.Loop(ctx, v.Interval, time.Hour, func(ctx context.Context) (bool, error) {
		ok, broken, err := v.RunOnce(ctx)
		if err == nil {
			v.Log.InfoContext(ctx, "audit verification round", "verified", ok, "broken", broken)
		}
		return false, err
	}, v.Log, "audit verification round failed")
}
