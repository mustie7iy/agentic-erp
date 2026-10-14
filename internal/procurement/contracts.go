package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Contract is a row of procurement.contract with its live items and the
// value released to purchase orders so far.
type Contract struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	SupplierID     uuid.UUID
	Currency       string
	Exponent       int
	ValidFrom      time.Time
	ValidTo        time.Time
	TargetValue    money.Minor
	ConsumedValue  money.Minor
	Status         string
	Items          []Item
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

const contractSelect = `SELECT c.id, c.document_number, c.legal_entity_id, c.supplier_id, c.currency, cu.exponent, c.valid_from, c.valid_to,
		c.target_value, coalesce((SELECT sum(value) FROM procurement.contract_release r WHERE r.tenant_id = c.tenant_id AND r.contract_id = c.id), 0)::bigint,
		c.status, c.version, c.created_at, c.created_by, c.updated_at, c.updated_by,
		coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'material_id', i.material_id, 'material_key', i.material_key,
			'description', i.description, 'unit', i.uom_code, 'unit_price', i.unit_price, 'price_unit', i.price_unit, 'account_id', i.account_id)
			ORDER BY i.item_no)
		 FROM procurement.contract_item i WHERE i.tenant_id = c.tenant_id AND i.contract_id = c.id AND NOT i.is_removed), '[]')
	FROM procurement.contract c JOIN ref.currency cu ON cu.code = c.currency`

func scanContract(row pgx.Row) (Contract, error) {
	var c Contract
	if err := row.Scan(&c.ID, &c.DocumentNumber, &c.LegalEntityID, &c.SupplierID, &c.Currency, &c.Exponent, &c.ValidFrom, &c.ValidTo,
		&c.TargetValue, &c.ConsumedValue, &c.Status, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &c.Items); err != nil {
		return Contract{}, fmt.Errorf("scan contract: %w", err)
	}
	return c, nil
}

func getContract(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Contract, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contract{}, err
	}
	q := contractSelect + ` WHERE c.id = $1`
	if lock {
		q += ` FOR UPDATE OF c`
	}
	c, err := scanContract(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(c.LegalEntityID)) {
		return Contract{}, notFound("contract")
	}
	if err != nil {
		return Contract{}, fmt.Errorf("procurement: get contract %s: %w", id, err)
	}
	return c, nil
}

// GetContract returns a contract.
func GetContract(ctx context.Context, id uuid.UUID) (Contract, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	return getContract(ctx, tx, id, false)
}

// contractItems validates contract items: positive prices, accounts the
// legal entity can post to, each material key once.
func (e *Endpoints) contractItems(ctx context.Context, le uuid.UUID, currency string, exp int, in []ItemInput) ([]Item, error) {
	items := make([]Item, len(in))
	keys := map[string]bool{}
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		if err := checkItemNo(it.No, i, field); err != nil {
			return nil, err
		}
		if keys[it.MaterialKey] {
			return nil, invalid("procurement.duplicate_material_key", field+".material_key", "each material key appears once per contract")
		}
		keys[it.MaterialKey] = true
		price, err := amount(it.Price, it.Currency, currency, exp, field+".unit_price", true)
		if err != nil {
			return nil, err
		}
		if err := e.account(ctx, le, it.AccountID, field+".account_id"); err != nil {
			return nil, err
		}
		items[i] = Item{No: int32(i + 1), MaterialID: it.MaterialID, MaterialKey: it.MaterialKey, Description: it.Description, Unit: it.Unit,
			UnitPrice: price, PriceUnit: max(it.PriceUnit, 1), AccountID: it.AccountID}
	}
	return items, nil
}

func writeContractItems(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, items []Item) error {
	n := len(items)
	nos, materials, keys, descriptions, units := make([]int32, n), make([]*uuid.UUID, n), make([]string, n), make([]string, n), make([]string, n)
	prices, priceUnits, accounts := make([]money.Minor, n), make([]int32, n), make([]uuid.UUID, n)
	for i, it := range items {
		nos[i], materials[i], keys[i], descriptions[i], units[i] = it.No, it.MaterialID, it.MaterialKey, it.Description, it.Unit
		prices[i], priceUnits[i], accounts[i] = it.UnitPrice, it.PriceUnit, it.AccountID
	}
	// Removed first, so a material key can move to another item number.
	if _, err := tx.Exec(ctx, `UPDATE procurement.contract_item SET is_removed = true WHERE contract_id = $1 AND NOT is_removed`, id); err != nil {
		return fmt.Errorf("procurement: remove items of contract %s: %w", id, err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO procurement.contract_item AS i (tenant_id, contract_id, item_no, material_id, material_key, description,
			unit_price, price_unit, uom_code, account_id)
		SELECT $1, $2, n, m, k, d, p, pu, u, a
		FROM unnest($3::int[], $4::uuid[], $5::text[], $6::text[], $7::bigint[], $8::int[], $9::text[], $10::uuid[]) AS x(n, m, k, d, p, pu, u, a)
		ON CONFLICT (tenant_id, contract_id, item_no) DO UPDATE SET material_id = EXCLUDED.material_id, material_key = EXCLUDED.material_key,
			description = EXCLUDED.description, unit_price = EXCLUDED.unit_price, price_unit = EXCLUDED.price_unit, uom_code = EXCLUDED.uom_code,
			account_id = EXCLUDED.account_id, is_removed = false`,
		tenant, id, nos, materials, keys, descriptions, prices, priceUnits, units, accounts)
	if err != nil {
		return fmt.Errorf("procurement: write items of contract %s: %w", id, err)
	}
	return nil
}

func checkValidity(from, to time.Time) error {
	if to.Before(from) {
		return invalid("procurement.invalid_validity", "valid_to", "valid_to is on or after valid_from")
	}
	return nil
}

// NewContract is the input of createContract.
type NewContract struct {
	LegalEntityID, SupplierID uuid.UUID
	Currency                  string
	ValidFrom, ValidTo        time.Time
	Target, TargetCurrency    string
	Items                     []ItemInput
}

// createContract creates a draft contract.
func (e *Endpoints) createContract(ctx context.Context, in NewContract) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID)
	if err != nil {
		return Contract{}, err
	}
	if err := checkValidity(in.ValidFrom, in.ValidTo); err != nil {
		return Contract{}, err
	}
	exp, err := exponent(ctx, tx, in.Currency, "currency")
	if err != nil {
		return Contract{}, err
	}
	target, err := amount(in.Target, in.TargetCurrency, in.Currency, exp, "target_value", true)
	if err != nil {
		return Contract{}, err
	}
	if err := e.supplier(ctx, in.SupplierID, le.ID); err != nil {
		return Contract{}, err
	}
	items, err := e.contractItems(ctx, le.ID, in.Currency, exp, in.Items)
	if err != nil {
		return Contract{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return Contract{}, err
	}
	number, err := e.number(ctx, le.ID, "contract", day)
	if err != nil {
		return Contract{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from,
			valid_to, target_value, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'draft', $10, $10)`,
		p.TenantID, id, number, le.ID, in.SupplierID, in.Currency, in.ValidFrom, in.ValidTo, target, p.ID); err != nil {
		return Contract{}, fmt.Errorf("procurement: create contract %s: %w", number, err)
	}
	if err := writeContractItems(ctx, tx, p.TenantID, id, items); err != nil {
		return Contract{}, err
	}
	c, err := getContract(ctx, tx, id, false)
	if err != nil {
		return Contract{}, err
	}
	return c, audit.Record(ctx, "procurement.contract.create", "procurement.contract", id, map[string]any{"document_number": number, "items": len(items)})
}

// ContractChange is a merge patch of a draft contract.
type ContractChange struct {
	ValidFrom, ValidTo     *time.Time
	Target, TargetCurrency *string
	Items                  []ItemInput // nil: unchanged
}

func loadContract(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error, action string, allowed ...string) (Contract, error) {
	c, err := getContract(ctx, tx, id, true)
	if err != nil {
		return Contract{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Contract{}, err
	}
	if !allows(c.Status, allowed...) {
		return Contract{}, statusConflict("contract", c.Status, action, allowed...)
	}
	return c, nil
}

// updateContract changes a draft contract.
func (e *Endpoints) updateContract(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch ContractChange) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	c, err := loadContract(ctx, tx, id, ifMatch, "update", Draft)
	if err != nil {
		return Contract{}, err
	}
	from, to, target := c.ValidFrom, c.ValidTo, c.TargetValue
	if ch.ValidFrom != nil {
		from = *ch.ValidFrom
	}
	if ch.ValidTo != nil {
		to = *ch.ValidTo
	}
	if err := checkValidity(from, to); err != nil {
		return Contract{}, err
	}
	if ch.Target != nil {
		if target, err = amount(*ch.Target, *ch.TargetCurrency, c.Currency, c.Exponent, "target_value", true); err != nil {
			return Contract{}, err
		}
	}
	if ch.Items != nil {
		items, err := e.contractItems(ctx, c.LegalEntityID, c.Currency, c.Exponent, ch.Items)
		if err != nil {
			return Contract{}, err
		}
		if err := writeContractItems(ctx, tx, p.TenantID, id, items); err != nil {
			return Contract{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.contract SET valid_from = $2, valid_to = $3, target_value = $4, version = version + 1, updated_by = $5
		WHERE id = $1`, id, from, to, target, p.ID); err != nil {
		return Contract{}, fmt.Errorf("procurement: update contract %s: %w", id, err)
	}
	updated, err := getContract(ctx, tx, id, false)
	if err != nil {
		return Contract{}, err
	}
	return updated, audit.Record(ctx, "procurement.contract.update", "procurement.contract", id, map[string]any{"items": len(updated.Items)})
}

// contractTransition is a status change of a contract and its event.
type contractTransition struct {
	action, from, to, event string
}

var (
	releaseContract   = contractTransition{"release", Draft, Active, "contract_activated"}
	cancelContract    = contractTransition{"cancel", Draft, Cancelled, "contract_cancelled"}
	terminateContract = contractTransition{"terminate", Active, Terminated, "contract_terminated"}
)

// changeContract applies a status transition; reason is recorded for
// terminations.
func changeContract(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, t contractTransition, reason string) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	c, err := loadContract(ctx, tx, id, ifMatch, t.action, t.from)
	if err != nil {
		return Contract{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.contract SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, t.to, p.ID); err != nil {
		return Contract{}, fmt.Errorf("procurement: %s contract %s: %w", t.action, id, err)
	}
	updated, err := getContract(ctx, tx, id, false)
	if err != nil {
		return Contract{}, err
	}
	payload := map[string]any{"contract_id": id, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID, "supplier_id": c.SupplierID,
		"previous_status": c.Status, "status": t.to}
	switch t.to {
	case Active:
		payload["valid_from"], payload["valid_to"] = c.ValidFrom.Format(time.DateOnly), c.ValidTo.Format(time.DateOnly)
		payload["target_value"] = money.EventAmount(c.TargetValue, c.Currency, c.Exponent)
	case Terminated:
		payload["consumed_value"] = money.EventAmount(c.ConsumedValue, c.Currency, c.Exponent)
	}
	detail := map[string]any{"from_status": c.Status}
	if reason != "" {
		detail["reason"] = reason
	}
	return updated, audit.Record(ctx, "procurement.contract."+t.action, "procurement.contract", id, detail, event(t.event, "contract", id, payload))
}
