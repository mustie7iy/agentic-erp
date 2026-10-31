package procurement

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Contract expiry: an active contract whose valid_to has passed moves to
// expired by a scheduled job (the contract's ContractExpired says so), as a
// system actor, recorded in the tenant's audit trail. Orders already refuse
// a contract past its validity; the job makes the status say what the
// dates already said, and publishes it. The same job alerts an active
// contract that no live renewal continues once it ends within
// purchasing.contract_expiry_notice_days (ContractExpiring, T47g), once:
// expiry_alerted_on is set once (000066).
//
// Both are judged on each contract's company's date in its time zone
// (T72n6, ADR-0051 as built), not the database's UTC current_date: a
// Toronto contract whose last day is 17 April is still active at 22:00
// that evening (02:00 UTC on the 18th), and a Dubai one has expired at
// 01:00 on the 18th (21:00 UTC on the 17th).

// ExpirerActor is the job's id in audit trails: a system actor.
var ExpirerActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:contract-expirer"))

// ContractExpirer expires contracts past their validity in rounds.
type ContractExpirer struct {
	Pool       *database.Pool
	AppRole    string        // erp_app: expires in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants with contracts due across the cell
	Interval   time.Duration // between rounds; default 1 hour
	Log        *slog.Logger
	Settings   Settings // the tenant's active configuration, for the notice and the companies' zones
	// LegalEntities gives each contract's company code, whose zone dates
	// the round.
	LegalEntities LegalEntities
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
	// Now is the round's instant: the audit entries' time and, in each
	// company's zone, the day contracts are judged on; nil means the wall
	// clock. Tests fix it.
	Now func() time.Time
}

func (x *ContractExpirer) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, x.Pool, x.Begin, s)
}

// due lists the tenants holding active contracts that may be past their
// validity, or not yet alerted and ending within the longest notice (365
// days), as the worker role. Both are deliberate supersets: the worker
// role cannot read the tenant's zones and notice, nor need it see
// renewals, so the tenant's round picks the contracts. No zone's date is
// more than a day after UTC's (UTC+14 is the furthest ahead), so a
// contract ending on UTC's date may be past its validity already, and one
// ending 366 days after it within a notice.
func (x *ContractExpirer) due(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	tx, err := x.begin(ctx, database.Scope{Role: x.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM procurement.contract WHERE status = 'active'
		AND (valid_to <= $1::date OR (expiry_alerted_on IS NULL AND valid_to <= $1::date + 366)) ORDER BY tenant_id`, clock.DayIn(now, time.UTC))
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("procurement: list tenants with contracts due: %w", err)
	}
	return tenants, nil
}

// Round is what a round did.
type Round struct {
	Expired, Alerted int
}

// Expire runs one tenant's round in one transaction: it moves its active
// contracts past their validity to expired, and alerts those ending
// within the notice, each with its event and audit entry, every contract
// judged on its company's date at the round.
func (x *ContractExpirer) Expire(ctx context.Context, tenant uuid.UUID) (Round, error) {
	return x.expire(ctx, tenant, clock.Now(x.Now).UTC())
}

func (x *ContractExpirer) expire(ctx context.Context, tenant uuid.UUID, now time.Time) (Round, error) {
	tx, err := x.begin(ctx, database.Scope{Role: x.AppRole, TenantID: tenant})
	if err != nil {
		return Round{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := database.SetActor(ctx, tx, ExpirerActor); err != nil {
		return Round{}, err
	}
	actor := reqctx.Principal{ID: ExpirerActor, Kind: reqctx.KindSystem, TenantID: tenant}
	scoped := reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor)
	doc, configured, err := x.document(scoped)
	if err != nil {
		return Round{}, err
	}
	days, err := x.companyDays(scoped, tx, doc, now)
	if err != nil {
		return Round{}, err
	}
	var r Round
	if r.Expired, err = expireDue(ctx, tx, actor, now, days); err != nil {
		return Round{}, err
	}
	if r.Alerted, err = x.alertDue(ctx, tx, actor, now, alerting{doc: doc, configured: configured, days: days}); err != nil {
		return Round{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Round{}, fmt.Errorf("procurement: commit contract expiry in tenant %s: %w", tenant, err)
	}
	return r, nil
}

// expireDue expires the tenant's active contracts whose valid_to is before
// their company's date.
func expireDue(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time, days companyDays) (int, error) {
	entities, dates := days.args()
	rows, _ := tx.Query(ctx, `SELECT c.id FROM procurement.contract c JOIN unnest($1::uuid[], $2::date[]) AS d(legal_entity_id, today)
			ON d.legal_entity_id = c.legal_entity_id
		WHERE c.status = 'active' AND c.valid_to < d.today ORDER BY c.id FOR UPDATE OF c`, entities, dates)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("procurement: list contracts due in tenant %s: %w", actor.TenantID, err)
	}
	for _, id := range ids {
		if err := expireContract(ctx, tx, actor, now, days, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// expireContract expires one contract, publishes ContractExpired with the
// company's date that judged it and the zone, and audits it.
func expireContract(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time, days companyDays, id uuid.UUID) error {
	c, err := scanContract(tx.QueryRow(ctx, contractSelect+` WHERE c.id = $1`, id))
	if err != nil {
		return fmt.Errorf("procurement: expire contract %s: %w", id, err)
	}
	day := days[c.LegalEntityID]
	if _, err := tx.Exec(ctx, `UPDATE procurement.contract SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, Expired, actor.ID); err != nil {
		return fmt.Errorf("procurement: expire contract %s: %w", id, err)
	}
	payload := map[string]any{"contract_id": id, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID, "supplier_id": c.SupplierID,
		"valid_to": c.ValidTo.Format(time.DateOnly), "consumed_value": money.EventAmount(c.ConsumedValue, c.Currency, c.Exponent), "previous_status": Active, "status": Expired,
		"expired_on": day.date.Format(time.DateOnly), "time_zone": day.zone}
	if _, err := outbox.Write(ctx, tx, actor.TenantID, actor.ID, "", event("contract_expired", "contract", id, payload)); err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "procurement.contract.expire", ObjectType: "procurement.contract", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"from_status": Active, "valid_to": c.ValidTo.Format(time.DateOnly), "expired_on": day.date.Format(time.DateOnly), "time_zone": day.zone}})
}

// RunOnce runs a round for every tenant with contracts due across the
// cell and returns what it did.
func (x *ContractExpirer) RunOnce(ctx context.Context) (Round, error) {
	now := clock.Now(x.Now).UTC()
	tenants, err := x.due(ctx, now)
	if err != nil {
		return Round{}, err
	}
	var r Round
	for _, t := range tenants {
		k, err := x.expire(ctx, t, now)
		if err != nil {
			return r, fmt.Errorf("procurement: run contract expiry: %w", err)
		}
		r.Expired, r.Alerted = r.Expired+k.Expired, r.Alerted+k.Alerted
	}
	return r, nil
}

// Run rounds until ctx ends, logging failures and work done.
func (x *ContractExpirer) Run(ctx context.Context) {
	worker.Loop(ctx, x.Interval, time.Hour, func(ctx context.Context) (bool, error) {
		r, err := x.RunOnce(ctx)
		if r != (Round{}) && !worker.Failed(ctx, err) {
			x.Log.InfoContext(ctx, "contract expiry round", "expired", r.Expired, "alerted", r.Alerted)
		}
		return false, err
	}, x.Log, "contract expiry round failed")
}
