package procurement

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Scheduling agreements (ADR-0070, T83a): a contract of its own kind
// whose delivery schedule is entered once it is active. A schedule line
// asks for a quantity of one of the agreement's items on a date within
// the agreement's validity. Its value is computed from the item's price,
// which an active contract keeps, and is not stored.

// The kinds of a contract, fixed when it is created.
const (
	KindContract            = "contract"
	KindSchedulingAgreement = "scheduling_agreement"
)

// contractKind is the kind a contract is created as through the API: the
// one asked for, which must be one there is, KindContract for none.
func contractKind(kind string) (string, error) {
	switch kind {
	case "", KindContract:
		return KindContract, nil
	case KindSchedulingAgreement:
		return kind, nil
	}
	return "", invalid("procurement.invalid_kind", "kind", "contract or scheduling_agreement")
}

// ScheduleLine is a row of procurement.schedule_line. Its quantity is a
// decimal string in the unit of the agreement item it asks for.
type ScheduleLine struct {
	ID, ContractID uuid.UUID
	No             int32
	ItemNo         int32
	DeliveryDate   time.Time
	Quantity       string
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

const scheduleLineSelect = `SELECT l.id, l.contract_id, l.line_no, l.contract_item_no, l.delivery_date, trim_scale(l.quantity)::text, l.version,
		l.created_at, l.created_by, l.updated_at, l.updated_by
	FROM procurement.schedule_line l`

func scanScheduleLine(row pgx.Row) (ScheduleLine, error) {
	var l ScheduleLine
	if err := row.Scan(&l.ID, &l.ContractID, &l.No, &l.ItemNo, &l.DeliveryDate, &l.Quantity, &l.Version, &l.CreatedAt, &l.CreatedBy, &l.UpdatedAt,
		&l.UpdatedBy); err != nil {
		return ScheduleLine{}, fmt.Errorf("scan schedule line: %w", err)
	}
	return l, nil
}

// scheduled is what line l of agreement c asks for: the agreement's item
// in the line's quantity, due on its date. A line always names a live
// item of its agreement (schedule_line_item_fkey, schedule_line_of_live_item),
// and an active contract's items do not change.
func (c Contract) scheduled(l ScheduleLine) Item {
	it := c.Items[slices.IndexFunc(c.Items, func(it Item) bool { return it.No == l.ItemNo })]
	it.Quantity, it.DeliveryDate = l.Quantity, &l.DeliveryDate
	return it
}

// agreement reads contract id under its row lock for a change of its
// schedule (action): it must be a scheduling agreement that is active.
// Changes of one agreement's schedule so run one after another, and wait
// for a change of the contract itself.
func agreement(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string) (Contract, error) {
	c, err := getContract(ctx, tx, id, true)
	if err != nil {
		return Contract{}, err
	}
	if c.Kind != KindSchedulingAgreement {
		return Contract{}, problem.Conflict("procurement.not_a_scheduling_agreement",
			fmt.Sprintf("Contract %s is of the kind %s; only a scheduling agreement has a delivery schedule.", c.DocumentNumber, c.Kind))
	}
	if c.Status != Active {
		return Contract{}, statusConflict("scheduling agreement", c.Status, action, Active)
	}
	return c, nil
}

// NewScheduleLine is the input of createScheduleLine.
type NewScheduleLine struct {
	ItemNo         int32
	DeliveryDate   time.Time
	Quantity, Unit string
}

// check validates a schedule line against agreement c: an item of the
// agreement in that item's unit, a positive quantity, a date within the
// agreement's validity and a value in the range of an amount.
func (in NewScheduleLine) check(c Contract) error {
	if !slices.ContainsFunc(c.Items, func(it Item) bool { return it.No == in.ItemNo && it.Unit == in.Unit }) {
		return invalid("procurement.invalid_contract_item", "contract_item_no", "an item of scheduling agreement "+c.DocumentNumber+" in the same unit")
	}
	if err := quantity(in.Quantity, "quantity"); err != nil {
		return err
	}
	if in.DeliveryDate.Before(c.ValidFrom) || in.DeliveryDate.After(c.ValidTo) {
		return invalid("procurement.schedule_line_outside_validity", "delivery_date", fmt.Sprintf("scheduling agreement %s is valid from %s to %s",
			c.DocumentNumber, c.ValidFrom.Format(time.DateOnly), c.ValidTo.Format(time.DateOnly)))
	}
	if _, err := itemsTotal([]Item{c.scheduled(ScheduleLine{ItemNo: in.ItemNo, Quantity: in.Quantity})}); err != nil {
		return invalid("procurement.amount_too_large", "quantity.value", "the line's value, quantity × unit price ÷ price unit, must be in the range of an amount")
	}
	return nil
}

// createScheduleLine enters a line of an active scheduling agreement's
// delivery schedule, numbered after the agreement's last: the contract is
// read under its row lock, so two lines entered at once are numbered one
// after the other. It returns the agreement, which the line is shown
// beside.
func (e *Endpoints) createScheduleLine(ctx context.Context, contract uuid.UUID, in NewScheduleLine) (Contract, ScheduleLine, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	c, err := agreement(ctx, tx, contract, "entering a schedule line")
	if err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	if err := in.check(c); err != nil {
		return Contract{}, ScheduleLine{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.schedule_line (tenant_id, id, contract_id, line_no, contract_item_no, delivery_date, quantity,
			created_by, updated_by)
		SELECT $1, $2, $3, coalesce(max(l.line_no), 0) + 1, $4, $5, $6::numeric, $7, $7 FROM procurement.schedule_line l WHERE l.contract_id = $3`,
		p.TenantID, id, c.ID, in.ItemNo, in.DeliveryDate, in.Quantity, p.ID); err != nil {
		return Contract{}, ScheduleLine{}, fmt.Errorf("procurement: enter a schedule line of contract %s: %w", c.ID, err)
	}
	l, err := scanScheduleLine(tx.QueryRow(ctx, scheduleLineSelect+` WHERE l.id = $1`, id))
	if err != nil {
		return Contract{}, ScheduleLine{}, fmt.Errorf("procurement: read schedule line %s: %w", id, err)
	}
	return c, l, e.scheduleChanged(ctx, c, l, "create", "created")
}

// scheduleChanged audits a change of line l of agreement c and publishes
// ScheduleLineChanged, naming the agreement's company by its code and
// its supplier by id and number, never by name (product owner
// 2026-11-05).
func (e *Endpoints) scheduleChanged(ctx context.Context, c Contract, l ScheduleLine, action, change string) error {
	le, err := e.legalEntity(ctx, c.LegalEntityID)
	if err != nil {
		return err
	}
	named, err := e.Partners.Names(ctx, []uuid.UUID{c.SupplierID})
	if err != nil {
		return fmt.Errorf("procurement: supplier of contract %s: %w", c.ID, err)
	}
	it := c.scheduled(l)
	value := money.EventAmount(total([]Item{it}), c.Currency, c.Exponent)
	changed := map[string]any{"schedule_line_id": l.ID, "contract_id": c.ID, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID,
		"legal_entity_code": le.Code, "supplier_id": c.SupplierID, "supplier_number": named[c.SupplierID].Number, "line_no": l.No,
		"contract_item_no": l.ItemNo, "material_key": it.MaterialKey, "delivery_date": l.DeliveryDate.Format(time.DateOnly),
		"quantity": map[string]string{"value": l.Quantity, "unit": it.Unit}, "amount": value, "change": change}
	detail := map[string]any{"contract_id": c.ID, "line_no": l.No, "contract_item_no": l.ItemNo, "delivery_date": l.DeliveryDate.Format(time.DateOnly),
		"quantity": l.Quantity, "unit": it.Unit, "amount": value}
	return audit.Record(ctx, "procurement.schedule_line."+action, "procurement.schedule_line", l.ID, detail,
		event("schedule_line_changed", "schedule_line", l.ID, changed))
}
