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

// ExpirerActor is the job's id in audit trails: a system actor.
var ExpirerActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:contract-expirer"))

// ContractExpirer expires contracts past their validity in rounds.
type ContractExpirer struct {
	Pool       *database.Pool
	AppRole    string        // erp_app: expires in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants with contracts due across the cell
	Interval   time.Duration // between rounds; default 1 hour
	Log        *slog.Logger
	Settings   Settings // the tenant's active configuration, for the notice
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
	// Now dates the audit entries; nil means the wall clock. Tests fix it.
	// Which contracts are due is the database's day, as valid_to is.
	Now func() time.Time
}

func (x *ContractExpirer) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, x.Pool, x.Begin, s)
}

// due lists the tenants holding active contracts past their validity, or
// not yet alerted and ending within the longest notice (365 days), as the
// worker role; the day is the database's. The second is a deliberate
// superset: the worker role cannot read the tenant's notice, nor need it
// see renewals, so the tenant's round picks the contracts to alert.
func (x *ContractExpirer) due(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := x.begin(ctx, database.Scope{Role: x.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM procurement.contract WHERE status = 'active'
		AND (valid_to < current_date OR (expiry_alerted_on IS NULL AND valid_to <= current_date + 365)) ORDER BY tenant_id`)
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
// within the notice, each with its event and audit entry.
func (x *ContractExpirer) Expire(ctx context.Context, tenant uuid.UUID) (Round, error) {
	tx, err := x.begin(ctx, database.Scope{Role: x.AppRole, TenantID: tenant})
	if err != nil {
		return Round{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := database.SetActor(ctx, tx, ExpirerActor); err != nil {
		return Round{}, err
	}
	actor := reqctx.Principal{ID: ExpirerActor, Kind: reqctx.KindSystem, TenantID: tenant}
	now := clock.Now(x.Now).UTC()
	var r Round
	if r.Expired, err = expireDue(ctx, tx, actor, now); err != nil {
		return Round{}, err
	}
	if r.Alerted, err = x.alertDue(ctx, tx, actor, now); err != nil {
		return Round{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Round{}, fmt.Errorf("procurement: commit contract expiry in tenant %s: %w", tenant, err)
	}
	return r, nil
}

// expireDue expires the tenant's active contracts past their validity.
func expireDue(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time) (int, error) {
	rows, _ := tx.Query(ctx, `SELECT id FROM procurement.contract WHERE status = 'active' AND valid_to < current_date ORDER BY id FOR UPDATE`)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("procurement: list contracts due in tenant %s: %w", actor.TenantID, err)
	}
	for _, id := range ids {
		if err := expireContract(ctx, tx, actor, now, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// expireContract expires one contract, publishes ContractExpired and
// audits it.
func expireContract(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time, id uuid.UUID) error {
	c, err := scanContract(tx.QueryRow(ctx, contractSelect+` WHERE c.id = $1`, id))
	if err != nil {
		return fmt.Errorf("procurement: expire contract %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.contract SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, Expired, actor.ID); err != nil {
		return fmt.Errorf("procurement: expire contract %s: %w", id, err)
	}
	payload := map[string]any{"contract_id": id, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID, "supplier_id": c.SupplierID,
		"valid_to": c.ValidTo.Format(time.DateOnly), "consumed_value": money.EventAmount(c.ConsumedValue, c.Currency, c.Exponent), "previous_status": Active, "status": Expired}
	if _, err := outbox.Write(ctx, tx, actor.TenantID, actor.ID, "", event("contract_expired", "contract", id, payload)); err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "procurement.contract.expire", ObjectType: "procurement.contract", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"from_status": Active, "valid_to": c.ValidTo.Format(time.DateOnly)}})
}

// RunOnce runs a round for every tenant with contracts due across the
// cell and returns what it did.
func (x *ContractExpirer) RunOnce(ctx context.Context) (Round, error) {
	tenants, err := x.due(ctx)
	if err != nil {
		return Round{}, err
	}
	var r Round
	for _, t := range tenants {
		k, err := x.Expire(ctx, t)
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
