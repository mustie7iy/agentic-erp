package cash

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Exporter sends the exports queued (T53h; ADR-0051 §14) as the bank file
// transmitter sends bank files (ADR-0038, T44e), on the same SFTP
// transport: a round finds the tenants with exports due, then, per export,
// claims it in one short transaction of its tenant (FOR UPDATE SKIP
// LOCKED, so replicas share the queue), builds its file and resolves its
// channel, uploads with no transaction open, and records the outcome in a
// second transaction, guarded by the attempt it claimed. A failed attempt
// is tried again after exportBackoff; a refusal (a problem below 500) or
// the last attempt's failure leaves the export failed. A claim whose
// outcome never came (the replica stopped) lapses and is claimed again:
// the file is named by its content, so an upload that finds it there
// (sftpclient.ErrExists) has delivered it, and it is recorded once.
type Exporter struct {
	Endpoints  *Endpoints
	Pool       *database.Pool
	AppRole    string        // erp_app: claims and records in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants with exports due
	Interval   time.Duration // idle wait between rounds; default 5s
	Upload     time.Duration // bound of one upload; default 2 minutes
	Log        *slog.Logger  // where failed attempts are reported; nil: not reported
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

// ExporterActor is the principal of the exporter's changes, in every
// tenant; who asked for an export is kept on it.
var ExporterActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:cash-exporter"))

// exportBackoff is the wait after each failed attempt, as for bank files:
// 1, 5 and 15 minutes, 1 and 4 hours; the sixth attempt's failure is final.
var exportBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}

// maxExportAttempts is how often an export is claimed.
var maxExportAttempts = len(exportBackoff) + 1

// exportLease is how many upload bounds a claim lasts before it lapses.
const exportLease = 5

// exportFinishBound is how long recording an upload's outcome may take
// once the worker's context has ended.
const exportFinishBound = 10 * time.Second

// exportClaim is an export claimed for upload.
type exportClaim struct {
	x        Export
	attempt  int
	endpoint sftpclient.Endpoint
	file     exportFile
	present  bool   // the upload found the file there already
	channel  string // the channel's name
}

func (w *Exporter) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, w.Pool, w.Begin, s)
}

func (w *Exporter) upload() time.Duration {
	if w.Upload <= 0 {
		return 2 * time.Minute
	}
	return w.Upload
}

func (w *Exporter) job(tenant uuid.UUID, step string) uow.Job {
	return uow.Job{Begin: w.begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "cash: " + step + " export", Now: w.Now}
}

// exporter is the exporter's principal in the tenant.
func exporter(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: ExporterActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// tenants returns the tenants with exports due.
func (w *Exporter) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM cash.export WHERE status = 'pending' AND next_attempt_at <= $1`, clock.Now(w.Now))
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("cash: list the tenants with exports due: %w", err)
	}
	return tenants, nil
}

// RunOnce sends every due export and returns how many it handled. A
// tenant whose export fails is left for the next round; the others go on.
func (w *Exporter) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("cash exporter: %w", err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		for ctx.Err() == nil { // a stopping worker claims nothing more
			did, err := w.one(ctx, t)
			if err != nil {
				errs = append(errs, fmt.Errorf("cash exporter: tenant %s: %w", t, err))
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

// Run sends exports until ctx ends; errors are logged and retried after
// the interval.
func (w *Exporter) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, 5*time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "cash export failed")
}

// one handles the tenant's next due export and reports whether there was
// one: claimed and uploaded, or settled without an upload (a refusal, a
// lapsed last attempt).
func (w *Exporter) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var c *exportClaim
	did := false
	if err := w.job(tenant, "claim").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var err error
		c, did, err = w.claim(ctx, tx, tenant)
		return did, err
	}); err != nil || c == nil {
		return did, err
	}
	sendErr := w.send(ctx, c)
	// The treasury system may have the file: its outcome is recorded even
	// when the worker is stopping, within a bound, as the transmitter's.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exportFinishBound)
	defer cancel()
	return true, w.job(tenant, "finish").Tx(fctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return w.finish(ctx, tx, tenant, c, sendErr)
	})
}

// claim locks the tenant's next due export and decides what becomes of it.
func (w *Exporter) claim(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (*exportClaim, bool, error) {
	now := clock.Now(w.Now)
	x, err := scanExport(tx.QueryRow(ctx, exportSelect+` WHERE x.status = 'pending' AND x.next_attempt_at <= $1
		ORDER BY x.next_attempt_at, x.id LIMIT 1 FOR UPDATE OF x SKIP LOCKED`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cash: claim an export: %w", err)
	}
	var c *exportClaim
	err = uow.Act(ctx, tx, exporter(tenant), w.Now, func(ctx context.Context) error {
		var err error
		c, err = w.prepare(ctx, tx, x, now)
		return err
	})
	return c, true, err
}

// prepare builds the export's file and resolves its channel, and claims it
// for upload; a channel that cannot be used, or a file that cannot be
// built, is a failed attempt, and a channel no longer set up for the
// export's company and format a refusal. A last attempt whose claim lapsed
// is made again as the last: the file is named by its content, so sending
// it again either delivers it or finds it there, and the outcome a lost
// commit hid is recovered rather than taken for a failure.
func (w *Exporter) prepare(ctx context.Context, tx pgx.Tx, x Export, now time.Time) (*exportClaim, error) {
	c := &exportClaim{x: x, attempt: min(x.Attempts+1, maxExportAttempts)}
	var err error
	if c.file, err = w.Endpoints.exportFile(ctx, tx, x); err != nil {
		return nil, w.retry(ctx, tx, c, err, now)
	}
	if c.channel, err = w.Endpoints.stillFor(ctx, x, c.file.company); err != nil {
		return nil, w.retry(ctx, tx, c, err, now)
	}
	if c.endpoint, err = w.Endpoints.Treasury.Endpoint(ctx, x.Connector); err != nil {
		return nil, w.retry(ctx, tx, c, err, now)
	}
	if _, err := tx.Exec(ctx, `UPDATE cash.export SET attempts = $2, next_attempt_at = $3, file_name = $4, sha256 = $5, version = version + 1 WHERE id = $1`,
		x.ID, c.attempt, now.Add(exportLease*w.upload()), c.file.name, c.file.sha256); err != nil {
		return nil, fmt.Errorf("cash: claim export %s: %w", x.ID, err)
	}
	return c, nil
}

// stillFor refuses a channel that, since the export was queued, no longer
// exports its company or no longer takes its format: a company's figures
// go only to a channel set up for it (ADR-0051 As built, T53h). The
// channel's status and settings are its endpoint's to check. It gives the
// channel's name.
func (e *Endpoints) stillFor(ctx context.Context, x Export, company string) (string, error) {
	ch, found, err := e.Treasury.Channel(ctx, x.Connector)
	if err != nil {
		return "", fmt.Errorf("cash: treasury channel %s of export %s: %w", x.Connector, x.ID, err)
	}
	if !found || !slices.Contains(ch.Companies, company) || ch.Format != x.Format {
		return "", problem.Unprocessable("cash.export_channel_invalid", "The connector no longer exports company "+company+" as "+x.Format+"; ask for an export again.")
	}
	return ch.Name, nil
}

// exportFile is the export's file: its snapshot's rows in its format.
func (e *Endpoints) exportFile(ctx context.Context, tx pgx.Tx, x Export) (exportFile, error) {
	s, err := scanSnapshot(tx.QueryRow(ctx, snapshotSelect+` WHERE s.id = $1`, x.Snapshot))
	if err != nil {
		return exportFile{}, fmt.Errorf("cash: snapshot %s of export %s: %w", x.Snapshot, x.ID, err)
	}
	codes, err := e.companyCodes(ctx, []uuid.UUID{s.LegalEntity})
	if err != nil {
		return exportFile{}, fmt.Errorf("cash: export %s: %w", x.ID, err)
	}
	s.CompanyCode = codes[s.LegalEntity]
	rows, err := e.exportRows(ctx, tx, s)
	if err != nil {
		return exportFile{}, err
	}
	return renderExport(s.CompanyCode, s.Date, x.Format, rows)
}

// send uploads the claimed file, within the upload bound. A file of its
// name there already is the same file: delivered.
func (w *Exporter) send(ctx context.Context, c *exportClaim) error {
	ctx, cancel := context.WithTimeout(ctx, w.upload())
	defer cancel()
	err := c.endpoint.Upload(ctx, c.file.name, c.file.content)
	if c.present = errors.Is(err, sftpclient.ErrExists); err == nil || c.present {
		return nil
	}
	detail := "The treasury system's SFTP server did not take the file; it is sent again."
	if c.attempt >= maxExportAttempts {
		detail = "The treasury system's SFTP server did not take the file on the last attempt."
	}
	return problem.Unavailable(detail, 0).WithCause(err)
}

// finish records the upload's outcome, if the claim still holds: a claim
// that lapsed was claimed again, and that claim records its own.
func (w *Exporter) finish(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c *exportClaim, sendErr error) (bool, error) {
	var status string
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT status, attempts FROM cash.export WHERE id = $1 FOR UPDATE`, c.x.ID).Scan(&status, &attempts); err != nil {
		return false, fmt.Errorf("cash: read export %s after its upload: %w", c.x.ID, err)
	}
	if status != ExportPending || attempts != c.attempt {
		return false, nil
	}
	err := uow.Act(ctx, tx, exporter(tenant), w.Now, func(ctx context.Context) error {
		if sendErr != nil {
			return w.retry(ctx, tx, c, sendErr, clock.Now(w.Now))
		}
		return w.delivered(ctx, tx, c)
	})
	return err == nil, err
}

// delivered marks the export delivered, a resend when its channel was
// delivered the same content before, audits and publishes it.
func (w *Exporter) delivered(ctx context.Context, tx pgx.Tx, c *exportClaim) error {
	x := c.x
	// Two exports of one file to one channel, finished by two replicas at
	// once, are told apart in turn: the second sees the first delivered.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cash.export:' || $1::text || ':' || $2::text, 0))`,
		x.Connector, hex.EncodeToString(c.file.sha256)); err != nil {
		return fmt.Errorf("cash: lock the deliveries of %s to %s: %w", c.file.name, x.Connector, err)
	}
	var resend bool
	if err := tx.QueryRow(ctx, `UPDATE cash.export x SET status = 'delivered', next_attempt_at = NULL, delivered_at = $2, version = version + 1,
			resend = EXISTS (SELECT 1 FROM cash.export d WHERE d.connector_id = x.connector_id AND d.sha256 = x.sha256 AND d.status = 'delivered' AND d.id <> x.id)
		WHERE x.id = $1 RETURNING x.resend`, x.ID, clock.Now(w.Now)).Scan(&resend); err != nil {
		return fmt.Errorf("cash: mark export %s delivered: %w", x.ID, err)
	}
	sum := hex.EncodeToString(c.file.sha256)
	return audit.Record(ctx, "cash.export.deliver", "cash.export", x.ID, map[string]any{"cash_snapshot_id": x.Snapshot, "connector_id": x.Connector,
		"file_name": c.file.name, "sha256": sum, "attempts": c.attempt, "resend": resend, "found_there": c.present, "requested_by": x.RequestedBy},
		outbox.Event{Context: "cash", Name: "cash_export_delivered", AggregateType: "cash.export", AggregateID: x.ID,
			Payload: map[string]any{"cash_export_id": x.ID, "cash_snapshot_id": x.Snapshot, "legal_entity_id": x.LegalEntity, "company_code": c.file.company,
				"snapshot_date": x.SnapshotDate.Format(time.DateOnly), "connector_id": x.Connector, "connector_name": c.channel, "format": x.Format, "file_name": c.file.name,
				"sha256": sum, "resend": resend}})
}

// retry queues the export for its next attempt after a failed one, or
// fails it: on a refusal, which another attempt would meet again, and
// after the last attempt.
func (w *Exporter) retry(ctx context.Context, tx pgx.Tx, c *exportClaim, cause error, now time.Time) error {
	p := problem.From(cause)
	if w.Log != nil {
		w.Log.WarnContext(ctx, "cash export attempt failed", "cash_export", c.x.ID, "attempt", c.attempt, "error", cause)
	}
	if p.Status < 500 || c.attempt >= maxExportAttempts {
		return failExport(ctx, tx, c, c.attempt, p)
	}
	next := now.Add(exportBackoff[c.attempt-1])
	if _, err := tx.Exec(ctx, `UPDATE cash.export SET attempts = $2, next_attempt_at = $3, last_error = $4, version = version + 1 WHERE id = $1`,
		c.x.ID, c.attempt, next, p.Code); err != nil {
		return fmt.Errorf("cash: queue export %s for its next attempt: %w", c.x.ID, err)
	}
	return audit.Record(ctx, "cash.export.retry", "cash.export", c.x.ID, map[string]any{"cash_snapshot_id": c.x.Snapshot, "connector_id": c.x.Connector,
		"attempts": c.attempt, "next_attempt_at": next, "code": p.Code})
}

// failExport ends the export: failed, with the problem's code.
func failExport(ctx context.Context, tx pgx.Tx, c *exportClaim, attempts int, p *problem.Problem) error {
	if _, err := tx.Exec(ctx, `UPDATE cash.export SET status = 'failed', attempts = $2, next_attempt_at = NULL, last_error = $3, version = version + 1
		WHERE id = $1`, c.x.ID, attempts, p.Code); err != nil {
		return fmt.Errorf("cash: fail export %s: %w", c.x.ID, err)
	}
	return audit.Record(ctx, "cash.export.fail", "cash.export", c.x.ID, map[string]any{"cash_snapshot_id": c.x.Snapshot, "connector_id": c.x.Connector,
		"attempts": attempts, "code": p.Code, "detail": p.Detail})
}
