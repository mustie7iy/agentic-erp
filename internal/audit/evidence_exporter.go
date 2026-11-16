package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// EvidenceExporter writes the evidence exports queued (T101a), as the
// records context's archiver writes its runs: a round finds the tenants
// with queued exports, then, per export, claims it in a transaction of
// its tenant (FOR UPDATE SKIP LOCKED, so replicas share the queue),
// checks that its requester may still act, builds its files and
// manifest, writes them to object storage and completes the export, all
// in that transaction. A file is kept under its content's SHA-256 and the
// manifest once under the export's id: a round run again after a commit
// whose acknowledgement was lost finds the export still queued and the
// manifest written, which it completes the export with, so an export has
// one manifest and is completed and published once. A business problem
// fails the export; the store not answering leaves it queued for the next
// round.
type EvidenceExporter struct {
	Endpoints  *Endpoints
	Requesters Requesters
	Pool       *database.Pool
	AppRole    string        // erp_app: claims and writes in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants with queued exports
	Interval   time.Duration // idle wait between rounds; default 5s
	Log        *slog.Logger  // where failed rounds are reported; nil: not reported
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

// ExporterActor is the principal of the exporter's changes, in every
// tenant; who asked for an export is kept on it.
var ExporterActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:audit-evidence-exporter"))

func (w *EvidenceExporter) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, w.Pool, w.Begin, s)
}

// queuedTenants returns the tenants with queued exports.
func (w *EvidenceExporter) queuedTenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM audit.evidence_export WHERE status = 'queued' AND tenant_id IN `+database.ActiveTenants)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("audit: list the tenants with queued evidence exports: %w", err)
	}
	return tenants, nil
}

// RunOnce writes every queued export and returns how many it settled. A
// tenant whose export fails on the machinery is left for the next round;
// the others go on.
func (w *EvidenceExporter) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.queuedTenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("evidence exporter: %w", err)
	}
	n := 0
	var failed []error
	for _, t := range tenants {
		for ctx.Err() == nil { // a stopping worker claims nothing more
			did, err := w.one(ctx, t)
			if err != nil {
				failed = append(failed, fmt.Errorf("evidence exporter: tenant %s: %w", t, err))
				break
			}
			if !did {
				break
			}
			n++
		}
	}
	return n, errors.Join(failed...)
}

// Run writes exports until ctx ends; errors are logged and retried after
// the interval.
func (w *EvidenceExporter) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, 5*time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "audit evidence export failed")
}

// one settles the tenant's next queued export and reports whether there
// was one.
func (w *EvidenceExporter) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	did := false
	err := uow.Job{Begin: w.begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "audit: evidence export", Now: w.Now}.Tx(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			x, err := scanExport(tx.QueryRow(ctx, exportSelect+` WHERE status = 'queued' ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`))
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("audit: claim an evidence export: %w", err)
			}
			did = true
			actor := reqctx.Principal{ID: ExporterActor, Kind: reqctx.KindSystem, TenantID: tenant}
			return true, uow.Act(ctx, tx, actor, w.Now, func(ctx context.Context) error { return w.settle(ctx, tx, x, w.export(ctx, tx, tenant, x)) })
		})
	return did, err
}

// settle fails the export on a business problem (one below 500), which
// another round would meet again; any other error is the machinery's,
// returned so that the export stays queued.
func (w *EvidenceExporter) settle(ctx context.Context, tx pgx.Tx, x Export, err error) error {
	var p *problem.Problem
	if err == nil || !errors.As(err, &p) || p.Status >= 500 {
		return err
	}
	if w.Log != nil {
		w.Log.WarnContext(ctx, "evidence export failed", "evidence_export", x.ID, "code", p.Code, "detail", p.Detail)
	}
	if _, err := tx.Exec(ctx, `UPDATE audit.evidence_export SET status = 'failed', failure_code = $2, version = version + 1 WHERE id = $1`, x.ID, p.Code); err != nil {
		return fmt.Errorf("audit: fail evidence export %s: %w", x.ID, err)
	}
	return trail.Record(ctx, "audit.evidence_export.fail", "audit.evidence_export", x.ID, map[string]any{"legal_entity_ids": x.LegalEntities,
		"code": p.Code, "detail": p.Detail, "requested_by": x.RequestedBy})
}

// export writes the export's files and manifest and completes it, once
// its requester may still act.
func (w *EvidenceExporter) export(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, x Export) error {
	e := w.Endpoints
	if err := w.Requesters.MayAct(ctx, tx, x.RequestedBy); err != nil {
		return fmt.Errorf("audit: requester of evidence export %s: %w", x.ID, err)
	}
	xs := []Export{x}
	if err := e.named(ctx, xs); err != nil {
		return err
	}
	x = xs[0]
	ev, err := e.build(ctx, tx, tenant, x)
	if err != nil {
		return err
	}
	for _, name := range fileNames {
		content := ev.files[name]
		if err := e.keep(ctx, blobstore.Key(tenant, blobstore.KindEvidence, fileKey(x.ID, name, sha256.Sum256(content))), content); err != nil {
			return err
		}
	}
	key := manifestKey(tenant, x.ID)
	manifest, err := e.keepManifest(ctx, key, ev.manifest)
	if err != nil {
		return err
	}
	return w.complete(ctx, tx, x, key, manifest)
}

// keep writes a file under its content's hash. A key that holds bytes
// already holds these: the earlier round's of the same content.
func (e *Endpoints) keep(ctx context.Context, key string, content []byte) error {
	err := e.Objects.Put(ctx, key, content, blobstore.PutOptions{ContentType: "application/x-ndjson"})
	if err == nil || errors.Is(err, blobstore.ErrExists) {
		return nil
	}
	return problem.Unavailable("The file store did not take the evidence file; the exporter tries again.", 30).WithCause(err)
}

// keepManifest writes the manifest once and returns the one the store
// holds: an earlier round's, whose transaction was lost, is the export's,
// since the files it names were written before it.
func (e *Endpoints) keepManifest(ctx context.Context, key string, manifest []byte) ([]byte, error) {
	err := e.Objects.Put(ctx, key, manifest, blobstore.PutOptions{ContentType: "application/json"})
	if err == nil {
		return manifest, nil
	}
	if !errors.Is(err, blobstore.ErrExists) {
		return nil, problem.Unavailable("The file store did not take the evidence manifest; the exporter tries again.", 30).WithCause(err)
	}
	stored, _, err := e.Objects.Get(ctx, key)
	if err != nil {
		return nil, problem.Unavailable("The file store did not return the evidence manifest it holds; the exporter tries again.", 30).WithCause(err)
	}
	return stored, nil
}

// complete marks the export completed with its manifest's figures,
// audits and publishes it.
func (w *EvidenceExporter) complete(ctx context.Context, tx pgx.Tx, x Export, key string, manifest []byte) error {
	var m evidenceManifest
	if err := json.Unmarshal(manifest, &m); err != nil || m.EvidenceExportID != x.ID || len(m.Files) != len(fileNames) {
		return evidenceChanged(key)
	}
	sum := sha256.Sum256(manifest)
	counts := map[string]int32{}
	for _, f := range m.Files {
		counts[f.Name] = f.Records
	}
	var broken *int64
	if m.Chain.FirstBrokenSeq != 0 {
		broken = &m.Chain.FirstBrokenSeq
	}
	if _, err := tx.Exec(ctx, `UPDATE audit.evidence_export SET status = 'completed', manifest_key = $2, manifest_sha256 = $3, files = $4,
		audit_event_count = $5, journal_entry_count = $6, approval_decision_count = $7, chain_verified = $8, chain_last_seq = $9, chain_checked = $10,
		chain_anchors_checked = $11, chain_broken_seq = $12, completed_at = $13, version = version + 1 WHERE id = $1`,
		x.ID, key, sum[:], m.Files, counts[auditEventsName], counts[journalEntriesName], counts[decisionsName], m.Chain.Verified, m.Chain.LastSeq,
		m.Chain.Checked, m.Chain.AnchorsChecked, broken, clock.Now(w.Now)); err != nil {
		return fmt.Errorf("audit: complete evidence export %s: %w", x.ID, err)
	}
	figures := map[string]any{"evidence_export_id": x.ID, "legal_entity_ids": x.LegalEntities, "from": m.From, "to": m.To,
		"manifest_sha256": hex.EncodeToString(sum[:]), "audit_event_count": counts[auditEventsName], "journal_entry_count": counts[journalEntriesName],
		"approval_decision_count": counts[decisionsName], "chain_verified": m.Chain.Verified, "chain_last_seq": m.Chain.LastSeq,
		"with_client_addresses": x.WithClientAddresses, "requested_by": x.RequestedBy}
	return trail.Record(ctx, "audit.evidence_export.complete", "audit.evidence_export", x.ID, figures,
		outbox.Event{Context: "platform", Name: "audit_evidence_exported", AggregateType: "audit.evidence_export", AggregateID: x.ID, Payload: figures})
}
