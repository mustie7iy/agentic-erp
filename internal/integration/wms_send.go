package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// WMSExchange is the exchange with warehouse systems (S2q1; ADR-0082 As
// built S2q), transport systems (S2r) and trade compliance services (S2s),
// the same messages of their own kinds. "WMS" in its name, its port's (Warehouse), settings'
// (WMSSettings), queue's and table's (integration.wms_message) names the
// machinery S2q built, which both kinds share, not the kind; renaming the
// exported names and the table is churn without a consumer. It works as
// the cash exporter sends its exports: a round finds the
// tenants with messages due, then, per message, claims it in one short
// transaction of its tenant (FOR UPDATE SKIP LOCKED, so replicas share
// the queue) and reads its connector, resolves the credentials and sends
// with no transaction open, and records the outcome in a second
// transaction, guarded by the row version its claim wrote (ADR-0008). A
// failed attempt is tried again after wmsBackoff; the warehouse system's
// refusal or the last attempt's failure leaves the message failed, a
// worklist (listWMSMessages?status=failed). A claim whose outcome never
// came (the replica stopped) lapses and is claimed again: the WMS takes a
// message id it holds as delivered, so the message is sent again safely.
// Its other half polls each active wms, tms and gts connector for what its
// WMS reports back and posts them (wms_receive.go).
type WMSExchange struct {
	Pool       *database.Pool
	AppRole    string // erp_app: claims, records and posts in the tenant's scope
	WorkerRole string // erp_worker: finds the tenants with messages due or connectors to poll
	Secrets    Secrets
	// Adapter gives the warehouse of a connector's adapter (WMSAdapters).
	Adapter func(WMSConnection) (Warehouse, error)
	// Deliveries posts a goods issue a WMS reports (shipping, wired by the
	// composition root).
	Deliveries WMSDeliveries
	// Orders posts a goods receipt a WMS reports (procurement, wired by
	// the composition root; S2q2).
	Orders WMSOrders
	// Shipments records a shipment a TMS reports (shipping, wired by the
	// composition root; S2r).
	Shipments TMSDeliveries
	// Screenings records a trade compliance service's answer (shipping,
	// wired by the composition root; S2s).
	Screenings GTSDeliveries
	// RefuseStandIn refuses the stand-in's adapter at use (production): a
	// connector stored before, or changed since, that the API would refuse.
	RefuseStandIn bool
	Interval      time.Duration // idle wait between send rounds; default 2s
	PollInterval  time.Duration // wait between polls of the connectors; default 10s
	Log           *slog.Logger  // where failed attempts are reported; nil: not reported
	Now           func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

// wmsBackoff is the wait after each failed attempt, as for exports: 1, 5
// and 15 minutes, 1 and 4 hours; the sixth attempt's failure is final.
var wmsBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}

// maxWMSAttempts is how often a message is claimed.
var maxWMSAttempts = len(wmsBackoff) + 1

// wmsLease is how long a claim lasts before it lapses: a credentials read
// and a call, each up to WMSCallTimeout, with room to record.
const wmsLease = 3 * WMSCallTimeout

// wmsRefused is the warehouse system refusing a message: final.
const wmsRefused = "integration.wms_refused"

// wmsOutcomeUnknown fails a message whose last attempt's claim lapsed.
const wmsOutcomeUnknown = "integration.wms_outcome_unknown"

// wmsFinishBound is how long recording a call's outcome may take once the
// worker's context has ended.
const wmsFinishBound = 10 * time.Second

// wmsClaim is a message claimed for sending.
type wmsClaim struct {
	tenant, id, connector uuid.UUID
	messageID, kind       string
	number                string // the document's: the delivery's or the purchase order's
	body                  []byte
	attempt               int
	version               int32 // the row version the claim wrote
	conn                  WMSConnection
	secretRef             string
}

func (w *WMSExchange) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, w.Pool, w.Begin, s)
}

func (w *WMSExchange) job(tenant uuid.UUID, step string) uow.Job {
	return uow.Job{Begin: w.begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "integration: " + step, Now: w.Now}
}

// dueTenants returns the tenants with messages due.
func (w *WMSExchange) dueTenants(ctx context.Context) ([]uuid.UUID, error) {
	tenants, err := database.Tenants(ctx, w.Pool, w.Begin, w.WorkerRole,
		`SELECT DISTINCT tenant_id FROM integration.wms_message WHERE status = 'pending' AND next_attempt_at <= $1 AND tenant_id IN `+database.ActiveTenants,
		clock.Now(w.Now))
	if err != nil {
		return nil, fmt.Errorf("integration: list the tenants with wms messages due: %w", err)
	}
	return tenants, nil
}

func (w *WMSExchange) sends() worker.Queue {
	return worker.Queue{Name: "wms exchange", Tenants: w.dueTenants, One: w.sendOne}
}

// SendOnce sends every due message and returns how many it handled.
func (w *WMSExchange) SendOnce(ctx context.Context) (int, error) { return w.sends().RunOnce(ctx) }

// Run sends and polls until ctx ends; errors are logged and retried after
// the interval.
func (w *WMSExchange) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() {
		worker.Loop(ctx, w.PollInterval, 10*time.Second, func(ctx context.Context) (bool, error) {
			_, err := w.PollOnce(ctx)
			return false, err
		}, w.Log, "wms poll failed")
	})
	worker.Loop(ctx, w.Interval, 2*time.Second, w.sends().Round, w.Log, "wms send failed")
	wg.Wait()
}

// sendOne handles the tenant's next due message and reports whether there
// was one: claimed and sent, or settled without a call (a refusal, a
// lapsed last attempt).
func (w *WMSExchange) sendOne(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var c *wmsClaim
	did := false
	if err := w.job(tenant, "claim a wms message").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var err error
		c, did, err = w.claim(ctx, tx, tenant)
		return did, err
	}); err != nil || c == nil {
		return did, err
	}
	sendErr := w.send(ctx, c)
	// The WMS may have the message: its outcome is recorded even when the
	// worker is stopping, within a bound.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wmsFinishBound)
	defer cancel()
	return true, w.job(tenant, "record a wms message's send").Tx(fctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return w.finish(ctx, tx, c, sendErr)
	})
}

// claim locks the tenant's next due message and claims it for a call,
// once its connector is read; a connector that cannot be used now is a
// failed attempt, and one refused for good fails the message.
func (w *WMSExchange) claim(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (*wmsClaim, bool, error) {
	now := clock.Now(w.Now)
	c := &wmsClaim{tenant: tenant}
	err := tx.QueryRow(ctx, `SELECT id, connector_id, message_id, kind, document_number, body::text, attempts FROM integration.wms_message
		WHERE status = 'pending' AND next_attempt_at <= $1 ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).
		Scan(&c.id, &c.connector, &c.messageID, &c.kind, &c.number, &c.body, &c.attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("integration: claim a wms message: %w", err)
	}
	settled := false // failed, or queued for its next attempt, without a call
	err = uow.Act(ctx, tx, wmsActor(tenant), w.Now, func(ctx context.Context) error {
		if c.attempt >= maxWMSAttempts { // the last attempt's claim lapsed
			c.attempt, settled = maxWMSAttempts, true
			return failWMS(ctx, tx, c, problem.Conflict(wmsOutcomeUnknown,
				"The exchange stopped without an outcome on the last attempt; the "+systemName(connectorKind(c.kind))+" may have the message."))
		}
		c.attempt++
		conn, err := w.connection(ctx, tx, c.connector, connectorKind(c.kind))
		if err != nil {
			settled = true
			return w.retry(ctx, tx, c, err, now)
		}
		c.conn, c.secretRef = conn.WMSConnection, conn.secretRef
		return tx.QueryRow(ctx, `UPDATE integration.wms_message SET attempts = $2, next_attempt_at = $3, version = version + 1 WHERE id = $1 RETURNING version`,
			c.id, c.attempt, now.Add(wmsLease)).Scan(&c.version)
	})
	if err != nil || settled {
		return nil, true, err
	}
	return c, true, nil
}

// wmsTarget is a wms or tms connector read for use, without its
// credentials.
type wmsTarget struct {
	WMSConnection
	id        uuid.UUID
	secretRef string
}

// connectorKind is the kind of connector a message of the kind is
// exchanged with: a transport request a tms one's (S2r), a screening
// request a gts one's (S2s), the others a wms one's.
func connectorKind(messageKind string) string {
	switch messageKind {
	case TMSTransportRequest:
		return TMS
	case GTSScreeningRequest:
		return GTS
	}
	return WMS
}

// notOfKind refuses a message's connector of another kind than its
// messages are exchanged with (changed by hand).
func notOfKind(c Connector, kind string) error {
	detail := "Connector " + c.Name + " is a " + c.Kind + " connector, not a " + kind + " one."
	switch kind {
	case TMS:
		return problem.Unprocessable("integration.not_a_tms", detail)
	case GTS:
		return problem.Unprocessable("integration.not_a_gts", detail)
	}
	return problem.Unprocessable("integration.not_a_wms", detail)
}

// connection reads the connector of a message for use, of the kind its
// messages are exchanged with, failing closed: one that is not an active
// connector of the kind naming a secret_ref, whose settings no longer
// resolve, or is the stand-in in production, is refused.
func (w *WMSExchange) connection(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind string) (wmsTarget, error) {
	c, err := getConnector(ctx, tx, id, false)
	switch {
	case err != nil:
		return wmsTarget{}, err
	case c.Kind != kind:
		return wmsTarget{}, notOfKind(c, kind)
	case c.Status != "active":
		return wmsTarget{}, problem.Conflict("integration.connector_disabled", "The "+systemName(kind)+"'s connector "+c.Name+" is disabled; enable it to use it.")
	case c.SecretRef == nil:
		return wmsTarget{}, problem.Unprocessable("integration.secret_required", "Connector "+c.Name+" names no secret_ref: the "+systemName(kind)+
			"'s credentials are a secret.")
	}
	s, err := plantSettings(c.Kind, c.Settings)
	if err != nil {
		return wmsTarget{}, err
	}
	if w.RefuseStandIn && s.Adapter == StandInAdapter {
		return wmsTarget{}, logisticsStandInRefused(c.Kind)
	}
	return wmsTarget{WMSConnection: WMSConnection{Name: c.Name, Kind: c.Kind, WMSSettings: s}, id: c.ID, secretRef: *c.SecretRef}, nil
}

// warehouse resolves a connector's credentials under the tenant's own
// namespace, with no transaction open, into the warehouse its adapter
// names. A reference that names nothing, or is not the tenant's, is a
// refusal; any other failure (the vault unreachable) is tried again.
func (w *WMSExchange) warehouse(ctx context.Context, tenant uuid.UUID, c wmsTarget) (Warehouse, error) {
	secret, err := tenantSecret(ctx, w.Secrets, tenant, c.secretRef)
	switch {
	case errors.Is(err, secrets.ErrNotFound) || errors.Is(err, secrets.ErrNotTenants):
		return nil, problem.Unprocessable("integration.secret_unavailable",
			"The "+systemName(c.Kind)+"'s credentials ("+c.secretRef+") cannot be resolved.").WithCause(err)
	case err != nil:
		return nil, fmt.Errorf("integration: the credentials of connector %s: %w", c.Name, err)
	}
	c.Secret = secret
	return w.Adapter(c.WMSConnection)
}

// send makes the claimed call, within its bound.
func (w *WMSExchange) send(ctx context.Context, c *wmsClaim) error {
	wh, err := w.warehouse(ctx, c.tenant, wmsTarget{WMSConnection: c.conn, secretRef: c.secretRef})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, WMSCallTimeout)
	defer cancel()
	return wh.Send(ctx, c.messageID, c.body)
}

// finish records the call's outcome, if the claim still holds: the row is
// as the claim left it. A claim that lapsed was claimed again, and that
// claim records its own.
func (w *WMSExchange) finish(ctx context.Context, tx pgx.Tx, c *wmsClaim, sendErr error) (bool, error) {
	var version int32
	if err := tx.QueryRow(ctx, `SELECT version FROM integration.wms_message WHERE id = $1 FOR UPDATE`, c.id).Scan(&version); err != nil {
		return false, fmt.Errorf("integration: read wms message %s after its send: %w", c.messageID, err)
	}
	if version != c.version {
		return false, nil
	}
	err := uow.Act(ctx, tx, wmsActor(c.tenant), w.Now, func(ctx context.Context) error {
		if sendErr != nil {
			return w.retry(ctx, tx, c, sendErr, clock.Now(w.Now))
		}
		if _, err := tx.Exec(ctx, `UPDATE integration.wms_message SET status = 'sent', sent_at = $2, next_attempt_at = NULL, last_error = NULL,
			version = version + 1 WHERE id = $1`, c.id, clock.Now(w.Now)); err != nil {
			return fmt.Errorf("integration: mark wms message %s sent: %w", c.messageID, err)
		}
		return audit.Record(ctx, "integration.wms_message.send", "integration.wms_message", c.id, c.details(map[string]any{"connector_name": c.conn.Name,
			"attempts": c.attempt}))
	})
	return err == nil, err
}

// retry queues the message for its next attempt after a failed one, or
// fails it: on the warehouse system's refusal, which another attempt
// would meet again, and after the last attempt. A connector that cannot
// be used now (disabled, its secret or settings not resolving, the
// stand-in in production) is a failed attempt like an outage: its
// administrator may put it right within the attempts' hours, and a
// message failed for it could not be sent again (Later: by hand).
func (w *WMSExchange) retry(ctx context.Context, tx pgx.Tx, c *wmsClaim, cause error, now time.Time) error {
	p := problem.From(cause)
	if w.Log != nil {
		w.Log.WarnContext(ctx, "wms message attempt failed", "wms_message", c.id, "attempt", c.attempt, "error", cause)
	}
	if p.Code == wmsRefused || c.attempt >= maxWMSAttempts {
		return failWMS(ctx, tx, c, p)
	}
	next := now.Add(wmsBackoff[c.attempt-1])
	if _, err := tx.Exec(ctx, `UPDATE integration.wms_message SET attempts = $2, next_attempt_at = $3, last_error = $4, version = version + 1 WHERE id = $1`,
		c.id, c.attempt, next, p.Code); err != nil {
		return fmt.Errorf("integration: queue wms message %s for its next attempt: %w", c.messageID, err)
	}
	return audit.Record(ctx, "integration.wms_message.retry", "integration.wms_message", c.id, c.details(map[string]any{"attempts": c.attempt,
		"next_attempt_at": next, "code": p.Code}))
}

// failWMS ends the message: failed, with the problem's code.
func failWMS(ctx context.Context, tx pgx.Tx, c *wmsClaim, p *problem.Problem) error {
	if _, err := tx.Exec(ctx, `UPDATE integration.wms_message SET status = 'failed', attempts = $2, next_attempt_at = NULL, last_error = $3,
		version = version + 1 WHERE id = $1`, c.id, c.attempt, p.Code); err != nil {
		return fmt.Errorf("integration: fail wms message %s: %w", c.messageID, err)
	}
	return audit.Record(ctx, "integration.wms_message.fail", "integration.wms_message", c.id, c.details(map[string]any{"attempts": c.attempt,
		"code": p.Code}))
}

// details are an audit record's details of the claimed message: its
// connector, its kind and its document's number, under the name of the
// document's kind, with more.
func (c *wmsClaim) details(more map[string]any) map[string]any {
	key := "outbound_delivery_number"
	if c.kind == WMSInboundReceipt {
		key = "purchase_order_number"
	}
	more["connector_id"], more["kind"], more[key] = c.connector, c.kind, c.number
	return more
}
