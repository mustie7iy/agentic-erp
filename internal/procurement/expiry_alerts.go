package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// alerting is what a round alerts by: the tenant's configuration, if it
// has one, and the companies' dates.
type alerting struct {
	doc        *config.Config
	configured bool
	days       companyDays
}

// alertDue publishes ContractExpiring for the tenant's active contracts
// that end within its notice of their company's date and that no live
// renewal continues, each once, and returns how many. A configuration
// activated before the notice existed has none, and a tenant may have no
// configuration yet: nothing is alerted then, and the round says so;
// expiry goes on. Running it twice alerts nothing twice: an alerted
// contract is not picked again, and the database refuses a second alert.
func (x *ContractExpirer) alertDue(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time, a alerting) (int, error) {
	if !a.configured {
		x.Log.WarnContext(ctx, "the tenant has no configuration: contracts are not alerted before they expire", "tenant", actor.TenantID)
		return 0, nil
	}
	days := a.doc.Purchasing.ContractExpiryNoticeDays
	if days == 0 {
		x.Log.WarnContext(ctx, "the active configuration has no purchasing.contract_expiry_notice_days: contracts are not alerted before they expire",
			"tenant", actor.TenantID)
		return 0, nil
	}
	entities, dates := a.days.args()
	rows, _ := tx.Query(ctx, `SELECT c.id, c.legal_entity_id, c.valid_to - d.today FROM procurement.contract c
			JOIN unnest($2::uuid[], $3::date[]) AS d(legal_entity_id, today) ON d.legal_entity_id = c.legal_entity_id
		WHERE c.status = 'active' AND c.expiry_alerted_on IS NULL AND c.valid_to >= d.today AND c.valid_to <= d.today + $1::int
			AND NOT EXISTS (SELECT FROM procurement.contract r WHERE r.tenant_id = c.tenant_id AND r.renewal_of_id = c.id AND r.status <> 'cancelled')
		ORDER BY c.id FOR UPDATE OF c`, days, entities, dates)
	due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[expiring])
	if err != nil {
		return 0, fmt.Errorf("procurement: list contracts to alert in tenant %s: %w", actor.TenantID, err)
	}
	n := 0
	for _, d := range due {
		alerted, err := alertContract(ctx, tx, actor, now, d, a.days[d.LegalEntityID])
		if err != nil {
			return 0, err
		}
		if alerted {
			n++
		}
	}
	return n, nil
}

// expiring is a contract to alert and the days until it ends.
type expiring struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	DaysLeft      int
}

// alertContract records the alert on the contract on its company's date,
// publishes ContractExpiring with that date and the zone and audits it,
// and reports whether it did. The update
// checks again that no live renewal continues the contract: a renewal
// locks the contract it renews without changing it, so one committed
// while the list of contracts to alert waited for the lock is seen only
// by this later statement.
func alertContract(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, now time.Time, d expiring, day companyDay) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE procurement.contract c SET expiry_alerted_on = $3, version = version + 1, updated_by = $2
		WHERE c.id = $1 AND c.status = 'active' AND c.expiry_alerted_on IS NULL AND NOT EXISTS (SELECT FROM procurement.contract r
			WHERE r.tenant_id = c.tenant_id AND r.renewal_of_id = c.id AND r.status <> 'cancelled')`, d.ID, actor.ID, day.date)
	if err != nil {
		return false, fmt.Errorf("procurement: alert contract %s: %w", d.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	c, err := scanContract(tx.QueryRow(ctx, contractSelect+` WHERE c.id = $1`, d.ID))
	if err != nil {
		return false, fmt.Errorf("procurement: alert contract %s: %w", d.ID, err)
	}
	payload := map[string]any{"contract_id": d.ID, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID, "supplier_id": c.SupplierID,
		"valid_to": c.ValidTo.Format(time.DateOnly), "days_left": d.DaysLeft, "consumed_value": money.EventAmount(c.ConsumedValue, c.Currency, c.Exponent),
		"target_value": money.EventAmount(c.TargetValue, c.Currency, c.Exponent), "alerted_on": day.date.Format(time.DateOnly), "time_zone": day.zone}
	if _, err := outbox.Write(ctx, tx, actor.TenantID, actor.ID, "", event("contract_expiring", "contract", d.ID, payload)); err != nil {
		return false, err
	}
	return true, audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "procurement.contract.alert_expiry", ObjectType: "procurement.contract", ObjectID: d.ID,
		Outcome: "succeeded", Detail: map[string]any{"valid_to": c.ValidTo.Format(time.DateOnly), "days_left": d.DaysLeft,
			"alerted_on": day.date.Format(time.DateOnly), "time_zone": day.zone}})
}
