// Package store is the semantic layer's store-backed source (ADR-0044):
// it reads a tenant's snapshot pointer under the tenant's RLS, asks
// erp-analytics over mTLS for the pre-aggregated groups a query needs, and
// hands them to analytics.Execute, which computes the metrics as it does
// from the database. When the store cannot answer — no snapshot, a
// snapshot staler than the freshness target or older than the caller
// asks, a plan the reader refuses, or erp-analytics failing, busy or
// timing out — the query is answered from the database instead, and the
// answer's lineage says which source it came from and as of when.
//
// It speaks the reader's wire types alone and links no cgo (ADR-0032,
// amended): erp-server stays static.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Target is the freshness target: p95 of fifteen minutes from posting to
// checked_at (ADR-0044).
const Target = 15 * time.Minute

// Database is the PostgreSQL path, the semantic layer's analytics.Source.
type Database interface {
	Facts(ctx context.Context, m *analytics.Model, asOf time.Time) (*analytics.Facts, error)
}

// Querier asks erp-analytics for a plan's groups (Client).
type Querier interface {
	Query(ctx context.Context, req wire.Request) (*wire.Result, error)
}

// Source answers queries from the store, or from the database when the
// store cannot.
type Source struct {
	Reader   Querier
	Database Database
	// Datasets are the fact datasets of the fact types the principal may
	// see; a plan over another is refused by the reader.
	Datasets []string
	// Requires are the permissions the database path's reads need
	// (analytics.Source.Permissions): a caller lacking one is refused
	// before the store names a dimension, as the database path's read is
	// refused, so neither path names an account or vendor the caller may
	// not read. Nil checks none.
	Requires []string
	Target   time.Duration    // the freshness target; default Target
	Now      func() time.Time // the as_of of a database answer; nil: the wall clock
	Log      *slog.Logger     // why an answer came from the database; nil: discarded
}

// Snapshot is the tenant's pointer as the request's transaction sees it,
// with what the freshness rule needs.
type Snapshot struct {
	TenantID    uuid.UUID
	ManifestKey string
	CheckedAt   *time.Time
	// Fresh: checked_at within the target of the database's clock, and no
	// journal entry posted longer ago than the target at or above the
	// watermark (one a long transaction committed after the run).
	Fresh bool
	// Leading is the tenant's leading ledger ("" when it has none).
	Leading string
}

// snapshotSQL reads the pointer, judged on checked_at and the watermark
// together (ADR-0044), and the leading ledger, under the tenant's RLS.
const snapshotSQL = `SELECT s.tenant_id, s.manifest_key, s.checked_at,
		s.checked_at IS NOT NULL AND s.checked_at >= now() - $1 * interval '1 millisecond'
		AND NOT EXISTS (SELECT FROM ledger.journal_entry e WHERE e.tenant_id = s.tenant_id AND e.posted_xid >= s.watermark
			AND e.posted_at < now() - $1 * interval '1 millisecond'),
		coalesce((SELECT g.id::text FROM ledger.ledger g WHERE g.tenant_id = s.tenant_id AND g.is_leading), '')
	FROM analytics.snapshot s`

// ReadSnapshot reads the tenant's snapshot in tx, which row-level security
// scopes to the tenant; ok is false when the tenant has none.
func ReadSnapshot(ctx context.Context, tx pgx.Tx, target time.Duration) (snap Snapshot, ok bool, err error) {
	err = tx.QueryRow(ctx, snapshotSQL, target.Milliseconds()).Scan(&snap.TenantID, &snap.ManifestKey, &snap.CheckedAt, &snap.Fresh, &snap.Leading)
	if errors.Is(err, pgx.ErrNoRows) {
		return snap, false, nil
	}
	if err != nil {
		return snap, false, fmt.Errorf("analytics: read the snapshot pointer: %w", err)
	}
	return snap, true, nil
}

func (s *Source) target() time.Duration {
	if s.Target <= 0 {
		return Target
	}
	return s.Target
}

// Facts reads what q needs for principal p: from the store when its
// snapshot is fresh and checked no earlier than notBefore (zero: the
// freshness target alone), else from the database. It runs in a request's
// unit of work, whose transaction is scoped to the tenant.
func (s *Source) Facts(ctx context.Context, m *analytics.Model, p analytics.Principal, q analytics.Query, notBefore time.Time) (*analytics.Facts, error) {
	f, reason, err := s.fromStoreIfFresh(ctx, m, p, q, notBefore)
	if f != nil || err != nil {
		return f, err
	}
	s.log().InfoContext(ctx, "analytics answered from the database", "reason", reason)
	f, err = s.Database.Facts(ctx, m, s.now())
	if err != nil {
		return nil, fmt.Errorf("analytics: database facts: %w", err)
	}
	return f, nil
}

// fromStoreIfFresh answers from the store when it may, else says why not.
// Without a reader (no erp-analytics configured) the pointer is not read.
func (s *Source) fromStoreIfFresh(ctx context.Context, m *analytics.Model, p analytics.Principal, q analytics.Query, notBefore time.Time) (*analytics.Facts, string, error) {
	snap, why, err := s.fresh(ctx, notBefore)
	if err != nil || why != "" {
		return nil, why, err
	}
	f, failed := s.fromStore(ctx, m, p, q, snap)
	return f, reason(failed), nil
}

// fresh is the tenant's snapshot when the store may answer, else why it
// may not ("" when it may). Without a reader (no erp-analytics
// configured) the pointer is not read.
func (s *Source) fresh(ctx context.Context, notBefore time.Time) (Snapshot, string, error) {
	if s.Reader == nil {
		return Snapshot{}, "no erp-analytics configured", nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Snapshot{}, "", err
	}
	if err := s.permitted(ctx); err != nil {
		return Snapshot{}, "", err
	}
	snap, ok, err := ReadSnapshot(ctx, tx, s.target())
	switch {
	case err != nil:
		return snap, "", err
	case !ok:
		return snap, "no snapshot", nil
	case !snap.Fresh:
		return snap, "snapshot stale", nil
	case snap.CheckedAt.Before(notBefore):
		return snap, "snapshot older than asked", nil
	}
	return snap, "", nil
}

// permitted refuses a caller lacking one of Requires, or barred from one
// by a segregation-of-duties rule, with the problem the API answers the
// database path's read with: one permission at a time, in the order that
// path reads, as each of its reads is authorized before the next
// (authz.Require, ADR-0010 As built 2026-11-02).
func (s *Source) permitted(ctx context.Context) error {
	for _, p := range s.Requires {
		if err := authz.Require(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// reason is why the store could not answer, which the database then does.
func reason(failed error) string {
	if failed == nil {
		return ""
	}
	return failed.Error()
}

func (s *Source) now() time.Time { return clock.Now(s.Now) }

func (s *Source) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// fromStore asks the reader for each base metric's groups.
func (s *Source) fromStore(ctx context.Context, m *analytics.Model, p analytics.Principal, q analytics.Query, snap Snapshot) (*analytics.Facts, error) {
	ledger := q.Ledger
	if ledger == "" {
		ledger = snap.Leading
	}
	plans, err := Plans(m, q, ledger)
	if err != nil {
		return nil, err
	}
	scope := wire.Scope{AllCompanies: p.Companies == nil, Companies: p.Companies, Datasets: s.Datasets}
	f := &analytics.Facts{Aggregated: map[string][]analytics.Fact{}, Exponents: map[string]int{}, Companies: map[string]string{}, Source: analytics.SourceStore}
	for _, name := range slices.Sorted(maps.Keys(plans)) {
		res, err := s.Reader.Query(ctx, wire.Request{TenantID: snap.TenantID, ManifestKey: snap.ManifestKey, Scope: scope, Plan: plans[name]})
		if err != nil {
			return nil, err
		}
		// The oldest of the datasets read: the reader refuses a manifest
		// whose datasets differ, so they are one as_of in practice.
		if f.AsOf.IsZero() || res.AsOf.Before(f.AsOf) {
			f.AsOf = res.AsOf
		}
		f.DataThrough = max(f.DataThrough, res.DataThrough)
		if f.Aggregated[name], err = groups(plans[name], res, f.Exponents); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// groups turns a plan's rows into facts Execute folds in as they are. A
// count without groups is one row counting 0, which the database path has
// no fact for. A row that does not fit the plan, or a currency with two
// exponents, is refused, as the database path refuses it (ADR-0005).
func groups(p wire.Plan, res *wire.Result, exponents map[string]int) ([]analytics.Fact, error) {
	out := make([]analytics.Fact, 0, len(res.Rows))
	for _, r := range res.Rows {
		if len(r.By) != len(p.By) {
			return nil, fmt.Errorf("analytics: erp-analytics answered %d groups for %d dimensions", len(r.By), len(p.By))
		}
		if p.Aggregate == "count_distinct" && r.Count == 0 {
			continue
		}
		dims := make(map[string]string, len(p.By))
		for i, d := range p.By {
			dims[d] = r.By[i]
		}
		if r.Currency != "" {
			if e, seen := exponents[r.Currency]; seen && e != r.Exponent {
				return nil, fmt.Errorf("analytics: erp-analytics answered %s with exponents %d and %d", r.Currency, e, r.Exponent)
			}
			exponents[r.Currency] = r.Exponent
		}
		out = append(out, analytics.Fact{Dims: dims, Amount: r.Amount, Currency: r.Currency, Count: r.Count})
	}
	return out, nil
}
