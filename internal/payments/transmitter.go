package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Transmitter sends the bank files asked for (T44e; ADR-0038), as the
// outbox relay publishes events (ADR-0007): a round finds the tenants with
// due files, then, per file, claims it in one short transaction of its
// tenant (FOR UPDATE SKIP LOCKED, so replicas share the queue), uploads it
// with no transaction open, and records the outcome in a second one,
// guarded by the claim it made. A failed attempt is tried again after
// transmitBackoff; a refusal (a problem below 500) or the last attempt's
// failure leaves the file failed, with bank_file_transmission_failed. A
// claim whose outcome never came (the replica stopped) lapses and the file
// is queued again: the sweep.
type Transmitter struct {
	Endpoints  *Endpoints
	Pool       *database.Pool
	AppRole    string        // erp_app: claims and records in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants with due files
	Interval   time.Duration // idle wait between rounds; default 5s
	Upload     time.Duration // bound of one upload; default 2 minutes
	Log        *slog.Logger  // where failed attempts are reported; nil: not reported
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

// TransmitterActor is the principal of the transmitter's changes, in every
// tenant; who asked for the file is kept on it and in its sent record.
var TransmitterActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:bank-file-transmitter"))

// transmitBackoff is the wait after each failed attempt: 1, 5 and 15
// minutes, 1 and 4 hours. The sixth attempt's failure is final, about 5h20m
// after the first.
var transmitBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}

// maxAttempts is how often a file is claimed for one request.
var maxAttempts = int16(len(transmitBackoff) + 1) // #nosec G115 -- five waits

// claimLease is how many upload bounds a claim lasts: the upload has ended
// long before the sweep queues its file again, so two uploads of one file
// never overlap.
const claimLease = 5

// queued is a due file as claimed: its status, attempts so far, and the
// request it serves.
type queued struct {
	id        uuid.UUID
	status    string
	attempts  int16
	operation uuid.UUID
	by        uuid.UUID
	live      bool // the operation is still queued or running, not cancelled
}

// claim is a file claimed for upload.
type claim struct {
	f         BankFile
	q         queued
	attempt   int16
	conn      Connector
	connector *uuid.UUID
	file      File
}

func (w *Transmitter) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, w.Pool, w.Begin, s)
}

func (w *Transmitter) upload() time.Duration {
	if w.Upload <= 0 {
		return 2 * time.Minute
	}
	return w.Upload
}

func (w *Transmitter) job(tenant uuid.UUID, step string) uow.Job {
	return uow.Job{Begin: w.begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "payments: " + step + " bank file", Now: w.Now}
}

func actor(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: TransmitterActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// tenants returns the tenants with files due: to send, or claimed with
// the claim lapsed.
func (w *Transmitter) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM payments.bank_file
		WHERE status IN ('to_send', 'sending') AND next_attempt_at <= $1`, clock.Now(w.Now))
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("payments: list the tenants with bank files to send: %w", err)
	}
	return tenants, nil
}

// RunOnce sends every due file and returns how many it handled. A tenant
// whose file fails is left for the next round; the others go on.
func (w *Transmitter) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("bank file transmitter: %w", err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		for {
			did, err := w.one(ctx, t)
			if err != nil {
				errs = append(errs, fmt.Errorf("bank file transmitter: tenant %s: %w", t, err))
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

// Run sends files until ctx ends; errors are logged and retried after the
// interval.
func (w *Transmitter) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, 5*time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "bank file transmission failed")
}

// one handles the tenant's next due file and reports whether there was
// one: claimed and uploaded, or settled without an upload (a duplicate, a
// withdrawn request, a lapsed claim, a refusal).
func (w *Transmitter) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var c *claim
	did := false
	if err := w.job(tenant, "claim").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var err error
		c, did, err = w.claim(ctx, tx, tenant)
		return did, err
	}); err != nil || c == nil {
		return did, err
	}
	sendErr := w.send(ctx, c)
	return true, w.job(tenant, "finish").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return w.finish(ctx, tx, tenant, c, sendErr)
	})
}

// claim locks the tenant's next due file and decides what becomes of it.
func (w *Transmitter) claim(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (*claim, bool, error) {
	now := clock.Now(w.Now)
	var q queued
	err := tx.QueryRow(ctx, `SELECT f.id, f.status, f.attempts, f.transmit_operation_id, f.transmit_requested_by, o.status IN ('queued', 'running')
		FROM payments.bank_file f JOIN platform.operation o ON o.tenant_id = f.tenant_id AND o.id = f.transmit_operation_id
		WHERE f.status IN ('to_send', 'sending') AND f.next_attempt_at <= $1
		ORDER BY f.next_attempt_at, f.id LIMIT 1 FOR UPDATE OF f SKIP LOCKED`, now).Scan(&q.id, &q.status, &q.attempts, &q.operation, &q.by, &q.live)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("payments: claim a bank file: %w", err)
	}
	f, err := scanFile(tx.QueryRow(ctx, fileSelect+` WHERE f.id = $1`, q.id))
	if err != nil {
		return nil, false, fmt.Errorf("payments: claim bank file %s: %w", q.id, err)
	}
	var c *claim
	err = uow.Act(ctx, tx, actor(tenant), w.Now, func(ctx context.Context) error {
		switch {
		case !q.live:
			return withdraw(ctx, tx, f, q)
		case q.status == FileSending:
			return w.lapsed(ctx, tx, f, q, now)
		}
		var err error
		c, err = w.prepare(ctx, tx, tenant, f, q, now)
		return err
	})
	return c, true, err
}

// withdraw returns a file whose transmission was cancelled before it was
// claimed to created: nothing was sent.
func withdraw(ctx context.Context, tx pgx.Tx, f BankFile, q queued) error {
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'created', attempts = 0, next_attempt_at = NULL, transmit_operation_id = NULL,
		transmit_requested_by = NULL, version = version + 1 WHERE id = $1`, f.ID); err != nil {
		return fmt.Errorf("payments: withdraw bank file %s: %w", f.ID, err)
	}
	return record(ctx, "payments.bank_file.withdraw", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": q.operation, "from_status": q.status})
}

// lapsed queues again a file whose claim lapsed without an outcome (the
// sweep), or fails it when that was its last attempt.
func (w *Transmitter) lapsed(ctx context.Context, tx pgx.Tx, f BankFile, q queued, now time.Time) error {
	if q.attempts >= maxAttempts {
		return failFile(ctx, tx, f, q, q.attempts, problem.Unavailable("The transmitter stopped without an outcome on the last attempt.", 0))
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'to_send', next_attempt_at = $2, version = version + 1 WHERE id = $1`, f.ID, now); err != nil {
		return fmt.Errorf("payments: queue bank file %s again: %w", f.ID, err)
	}
	return record(ctx, "payments.bank_file.requeue", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": q.operation, "attempts": q.attempts})
}

// prepare resolves the file's channel and content and claims it for
// upload; a file its bank already took is transmitted without one.
func (w *Transmitter) prepare(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, f BankFile, q queued, now time.Time) (*claim, error) {
	c := &claim{f: f, q: q, attempt: q.attempts + 1}
	var err error
	if c.conn, c.connector, err = w.Endpoints.channel(ctx, tx, f.HouseBankID); err != nil {
		return nil, w.retry(ctx, tx, c, err, now)
	}
	dup, err := sent(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, transmitted(ctx, tx, c, true)
	}
	if c.file, err = w.Endpoints.bankFile(ctx, tx, tenant, f); err != nil {
		return nil, w.retry(ctx, tx, c, err, now)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'sending', attempts = $2, next_attempt_at = $3, version = version + 1 WHERE id = $1`,
		f.ID, c.attempt, now.Add(claimLease*w.upload())); err != nil {
		return nil, fmt.Errorf("payments: claim bank file %s: %w", f.ID, err)
	}
	return c, nil
}

// bankFile is the file as the bank receives it: named after its company,
// run and id, its content checked against the hash recorded at release.
func (e *Endpoints) bankFile(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, f BankFile) (File, error) {
	r, err := load(ctx, tx, f.RunID, false)
	if err != nil {
		return File{}, err
	}
	le, err := e.entityOf(ctx, r)
	if err != nil { // errNoEntity included: an existing run's missing entity is a 500, as on the download
		return File{}, err
	}
	content, err := e.fileOf(ctx, tx, tenant, f)
	if err != nil {
		return File{}, err
	}
	return File{ID: f.ID, Name: le.Code + "-" + r.DocumentNumber + "-" + f.ID.String() + ".xml", Content: content}, nil
}

// send uploads the claimed file, within the upload bound.
func (w *Transmitter) send(ctx context.Context, c *claim) error {
	ctx, cancel := context.WithTimeout(ctx, w.upload())
	defer cancel()
	if err := c.conn.Transmit(ctx, c.file); err != nil {
		return problem.Unavailable("The bank connector did not take the file; it is sent again.", 0).WithCause(err)
	}
	return nil
}

// finish records the upload's outcome, if the claim still holds: a claim
// that lapsed belongs to the sweep, which queued the file again.
func (w *Transmitter) finish(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c *claim, sendErr error) (bool, error) {
	var status string
	var attempts int16
	if err := tx.QueryRow(ctx, `SELECT status, attempts FROM payments.bank_file WHERE id = $1 FOR UPDATE`, c.f.ID).Scan(&status, &attempts); err != nil {
		return false, fmt.Errorf("payments: read bank file %s after its upload: %w", c.f.ID, err)
	}
	if status != FileSending || attempts != c.attempt {
		return false, nil
	}
	err := uow.Act(ctx, tx, actor(tenant), w.Now, func(ctx context.Context) error {
		if sendErr != nil {
			return w.retry(ctx, tx, c, sendErr, clock.Now(w.Now))
		}
		if err := recordSent(ctx, tx, tenant, c.f, c.connector, c.q.by); err != nil {
			return err
		}
		return transmitted(ctx, tx, c, false)
	})
	return err == nil, err
}

// retry queues the file for its next attempt after a failed one, or fails
// it: on a refusal, which another attempt would meet again, and after the
// last attempt.
func (w *Transmitter) retry(ctx context.Context, tx pgx.Tx, c *claim, cause error, now time.Time) error {
	p := problem.From(cause)
	if w.Log != nil {
		w.Log.WarnContext(ctx, "bank file transmission attempt failed", "bank_file", c.f.ID, "attempt", c.attempt, "error", cause)
	}
	if p.Status < 500 || c.attempt >= maxAttempts {
		return failFile(ctx, tx, c.f, c.q, c.attempt, p)
	}
	next := now.Add(transmitBackoff[c.attempt-1])
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'to_send', attempts = $2, next_attempt_at = $3, last_error = $4, version = version + 1
		WHERE id = $1`, c.f.ID, c.attempt, next, p.Code); err != nil {
		return fmt.Errorf("payments: queue bank file %s for its next attempt: %w", c.f.ID, err)
	}
	return record(ctx, "payments.bank_file.retry", c.f.ID, map[string]any{"bank_file_id": c.f.ID, "operation_id": c.q.operation, "attempts": c.attempt,
		"next_attempt_at": next, "code": p.Code})
}

// failFile ends the request: the file is failed, the event and the operation
// say why, and the file may be asked for again.
func failFile(ctx context.Context, tx pgx.Tx, f BankFile, q queued, attempts int16, p *problem.Problem) error {
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'failed', attempts = $2, next_attempt_at = NULL, last_error = $3, version = version + 1
		WHERE id = $1`, f.ID, attempts, p.Code); err != nil {
		return fmt.Errorf("payments: fail bank file %s: %w", f.ID, err)
	}
	if err := record(ctx, "payments.bank_file.fail", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": q.operation, "attempts": attempts, "code": p.Code},
		fileEvent(f, "bank_file_transmission_failed", map[string]any{"status": FileFailed, "attempts": attempts,
			"error": map[string]string{"code": p.Code, "message": p.Detail}})); err != nil {
		return err
	}
	_, err := operations.Fail(ctx, q.operation, p)
	return settled(err)
}

// transmitted marks the file transmitted, publishes it and completes the
// request's operation.
func transmitted(ctx context.Context, tx pgx.Tx, c *claim, duplicate bool) error {
	f := c.f
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_file SET status = 'transmitted', attempts = $2, next_attempt_at = NULL, last_error = NULL,
		version = version + 1 WHERE id = $1`, f.ID, c.attempt); err != nil {
		return fmt.Errorf("payments: mark bank file %s transmitted: %w", f.ID, err)
	}
	if err := record(ctx, "payments.bank_file.transmit", f.ID, map[string]any{"bank_file_id": f.ID, "operation_id": c.q.operation, "connector_id": c.connector,
		"duplicate": duplicate, "requested_by": c.q.by},
		fileEvent(f, "bank_file_transmitted", map[string]any{"format": f.Format, "sha256": fmt.Sprintf("%x", f.SHA256), "connector_id": c.connector,
			"integration_message_id": nil, "status": FileTransmitted, "duplicate": duplicate})); err != nil {
		return err
	}
	_, err := operations.Succeed(ctx, c.q.operation, "/v1/bank-files/"+f.ID.String(), map[string]any{"bank_file_id": f.ID, "status": FileTransmitted})
	return settled(err)
}

// settled accepts an operation that finished already: cancelled while its
// file was in flight. The file's outcome stands, since the bank has it or
// refused it either way.
func settled(err error) error {
	if err != nil && problem.From(err).Code == "platform.operation_finished" {
		return nil
	}
	return err
}
