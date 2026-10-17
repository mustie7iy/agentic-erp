package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// alertDue publishes ContractExpiring for the tenant's active contracts
// that end within its notice and that no live renewal continues, each
// once, and returns how many. A configuration activated before the notice
// existed has none, and a tenant may have no configuration yet: nothing is
// alerted then, and the round says so; expiry goes on. Running it
// twice alerts nothing twice: an alerted contract is not picked again, and
// the database refuses a second alert.
func (x *ContractExpirer) alertDue(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time) (int, error) {
	doc, _, err := x.Settings.Document(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor))
	if problem.From(err).Code == "config.not_bootstrapped" {
		x.Log.WarnContext(ctx, "the tenant has no configuration: contracts are not alerted before they expire", "tenant", actor.TenantID)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	days := doc.Purchasing.ContractExpiryNoticeDays
	if days == 0 {
		x.Log.WarnContext(ctx, "the active configuration has no purchasing.contract_expiry_notice_days: contracts are not alerted before they expire",
			"tenant", actor.TenantID)
		return 0, nil
	}
	rows, _ := tx.Query(ctx, `SELECT c.id, c.valid_to - current_date FROM procurement.contract c WHERE c.status = 'active' AND c.expiry_alerted_on IS NULL
		AND c.valid_to >= current_date AND c.valid_to <= current_date + $1::int AND NOT EXISTS (SELECT FROM procurement.contract r
			WHERE r.tenant_id = c.tenant_id AND r.renewal_of_id = c.id AND r.status <> 'cancelled')
		ORDER BY c.id FOR UPDATE OF c`, days)
	due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[expiring])
	if err != nil {
		return 0, fmt.Errorf("procurement: list contracts to alert in tenant %s: %w", actor.TenantID, err)
	}
	for _, d := range due {
		if err := alertContract(ctx, tx, actor, now, d); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// expiring is a contract to alert and the days until it ends.
type expiring struct {
	ID       uuid.UUID
	DaysLeft int
}

// alertContract records the alert on the contract, publishes
// ContractExpiring and audits it.
func alertContract(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time, d expiring) error {
	c, err := scanContract(tx.QueryRow(ctx, contractSelect+` WHERE c.id = $1`, d.ID))
	if err != nil {
		return fmt.Errorf("procurement: alert contract %s: %w", d.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.contract SET expiry_alerted_on = current_date, version = version + 1, updated_by = $2 WHERE id = $1`,
		d.ID, actor.ID); err != nil {
		return fmt.Errorf("procurement: alert contract %s: %w", d.ID, err)
	}
	payload := map[string]any{"contract_id": d.ID, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID, "supplier_id": c.SupplierID,
		"valid_to": c.ValidTo.Format(time.DateOnly), "days_left": d.DaysLeft, "consumed_value": money.EventAmount(c.ConsumedValue, c.Currency, c.Exponent),
		"target_value": money.EventAmount(c.TargetValue, c.Currency, c.Exponent)}
	if _, err := outbox.Write(ctx, tx, actor.TenantID, actor.ID, "", event("contract_expiring", "contract", d.ID, payload)); err != nil {
		return err
	}
	return audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "procurement.contract.alert_expiry", ObjectType: "procurement.contract", ObjectID: d.ID,
		Outcome: "succeeded", Detail: map[string]any{"valid_to": c.ValidTo.Format(time.DateOnly), "days_left": d.DaysLeft}})
}
