package procurement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Retired is the status of a catalog item no longer offered (ADR-0071 §3).
const Retired = "retired"

func init() {
	problem.RegisterConstraint("catalog_item_active_key", func() *problem.Problem {
		return problem.Conflict("procurement.catalog_item_exists", "The supplier already has an active catalog item with this item key in the company.")
	})
	problem.RegisterConstraint("catalog_item_uom_code_fkey", func() *problem.Problem {
		return problem.Unprocessable("procurement.unknown_unit", "A unit of measure is not known.",
			problem.FieldError{Field: "unit", Code: "unknown_unit", Message: "a unit of measure code from the reference data"})
	})
}

// CatalogItem is a row of procurement.catalog_item (ADR-0071): a
// supplier's item in a company's catalog. UnitPrice is minor units of
// Currency per PriceUnit. ContractNumber is the document number of the
// contract whose item ContractItemNo prices an order of it, if it names
// one.
type CatalogItem struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	SupplierID     uuid.UUID
	ItemKey        string
	Description    string
	Unit           string
	Currency       string
	Exponent       int
	UnitPrice      money.Minor
	PriceUnit      int32
	ContractID     *uuid.UUID
	ContractNumber *string
	ContractItemNo *int32
	Status         string
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

// catalogSelect reads catalog items under the alias ci; its columns are
// unqualified in the result, so a list wraps it for the keyset's id.
const catalogSelect = `SELECT ci.id, ci.legal_entity_id, ci.supplier_id, ci.item_key, ci.description, ci.uom_code, ci.currency, cu.exponent,
		ci.unit_price, ci.price_unit, ci.contract_id,
		(SELECT c.document_number FROM procurement.contract c WHERE c.tenant_id = ci.tenant_id AND c.id = ci.contract_id) AS contract_number,
		ci.contract_item_no, ci.status, ci.version, ci.created_at, ci.created_by, ci.updated_at, ci.updated_by
	FROM procurement.catalog_item ci JOIN ref.currency cu ON cu.code = ci.currency`

func scanCatalogItem(row pgx.Row) (CatalogItem, error) {
	var c CatalogItem
	if err := row.Scan(&c.ID, &c.LegalEntityID, &c.SupplierID, &c.ItemKey, &c.Description, &c.Unit, &c.Currency, &c.Exponent, &c.UnitPrice,
		&c.PriceUnit, &c.ContractID, &c.ContractNumber, &c.ContractItemNo, &c.Status, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy); err != nil {
		return CatalogItem{}, fmt.Errorf("scan catalog item: %w", err)
	}
	return c, nil
}

// getCatalogItem loads a catalog item of a legal entity the caller may
// act in (404 otherwise); lock takes the row lock for a change.
func getCatalogItem(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (CatalogItem, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return CatalogItem{}, err
	}
	q := catalogSelect + ` WHERE ci.id = $1`
	if lock {
		q += ` FOR UPDATE OF ci`
	}
	c, err := scanCatalogItem(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("catalog item"), "procurement: get catalog item %s", id); err != nil {
		return CatalogItem{}, err
	}
	return c, nil
}

// GetCatalogItem returns a catalog item.
func GetCatalogItem(ctx context.Context, id uuid.UUID) (CatalogItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CatalogItem{}, err
	}
	return getCatalogItem(ctx, tx, id, false)
}

// NewCatalogItem is the input of createCatalogItem. Price is a decimal
// amount of Currency per PriceUnit; ContractID and ContractItemNo name the
// contract item that prices an order of it, both or neither.
type NewCatalogItem struct {
	LegalEntityID  uuid.UUID
	SupplierID     uuid.UUID
	ItemKey        string
	Description    string
	Unit           string
	Price          string
	Currency       string
	PriceUnit      int32
	ContractID     *uuid.UUID
	ContractItemNo *int32
}

// createCatalogItem loads one item into a company's catalog (ADR-0071
// §2): of a supplier that can be ordered from in the company, as an
// order's must be, at a positive price, its key the supplier's once among
// the company's active items (the unique index refuses a second,
// procurement.catalog_item_exists).
func (e *Endpoints) createCatalogItem(ctx context.Context, in NewCatalogItem) (CatalogItem, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CatalogItem{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID)
	if err != nil {
		return CatalogItem{}, err
	}
	if err := catalogTexts(in); err != nil {
		return CatalogItem{}, err
	}
	exp, err := exponent(ctx, tx, in.Currency, "unit_price.currency")
	if err != nil {
		return CatalogItem{}, err
	}
	price, err := amount(in.Price, in.Currency, in.Currency, exp, "unit_price", true)
	if err != nil {
		return CatalogItem{}, err
	}
	if err := e.supplier(ctx, in.SupplierID, le.ID); err != nil {
		return CatalogItem{}, err
	}
	if err := catalogContractItem(ctx, tx, in, le.ID); err != nil {
		return CatalogItem{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.catalog_item (tenant_id, id, legal_entity_id, supplier_id, item_key, description, uom_code,
			currency, unit_price, price_unit, contract_id, contract_item_no, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'active', $13, $13)`,
		p.TenantID, id, le.ID, in.SupplierID, in.ItemKey, in.Description, in.Unit, in.Currency, price, max(in.PriceUnit, 1), in.ContractID,
		in.ContractItemNo, p.ID); err != nil {
		return CatalogItem{}, fmt.Errorf("procurement: create catalog item %s of supplier %s: %w", in.ItemKey, in.SupplierID, err)
	}
	c, err := getCatalogItem(ctx, tx, id, false)
	if err != nil {
		return CatalogItem{}, err
	}
	return c, e.catalogChanged(ctx, c, "create", nil)
}

// catalogTexts checks an item's key and description: the key as it is
// stored and searched, without surrounding white space.
func catalogTexts(in NewCatalogItem) error {
	if in.ItemKey == "" || in.ItemKey != strings.TrimSpace(in.ItemKey) {
		return invalid("procurement.invalid_item_key", "item_key", "an item key without leading or trailing white space")
	}
	return problem.RequireText("description", in.Description)
}

// catalogContractItem checks the contract item a catalog item names, if
// any: an item of an active contract of the company and the supplier, in
// the catalog item's unit and currency.
func catalogContractItem(ctx context.Context, tx pgx.Tx, in NewCatalogItem, le uuid.UUID) error {
	if in.ContractID == nil && in.ContractItemNo == nil {
		return nil
	}
	if in.ContractID == nil {
		return invalid("procurement.invalid_contract_item", "contract_item_no", "a contract item needs its contract_id")
	}
	if in.ContractItemNo == nil {
		return invalid("procurement.invalid_contract_item", "contract_item_no", "a contract is named with the item of it that prices the catalog item")
	}
	c, err := referencedContract(ctx, tx, *in.ContractID)
	if err != nil {
		return err
	}
	switch {
	case c.Status != Active:
		return invalid("procurement.invalid_contract", "contract_id", "the contract is "+c.Status)
	case c.LegalEntityID != le || c.SupplierID != in.SupplierID:
		return invalid("procurement.invalid_contract", "contract_id", "a contract of the legal entity with the catalog item's supplier")
	case c.Currency != in.Currency:
		return invalid("procurement.invalid_contract", "contract_id", "a contract in the catalog item's currency "+in.Currency)
	}
	for _, it := range c.Items {
		if it.No == *in.ContractItemNo && it.Unit == in.Unit {
			return nil
		}
	}
	return invalid("procurement.invalid_contract_item", "contract_item_no", "an item of the contract in the same unit")
}

// retireCatalogItem takes an active item out of the catalog, once: it is
// found by id and by a search that asks for retired items, and its key is
// free for a new item.
func (e *Endpoints) retireCatalogItem(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (CatalogItem, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CatalogItem{}, err
	}
	c, err := getCatalogItem(ctx, tx, id, true)
	if err != nil {
		return CatalogItem{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return CatalogItem{}, err
	}
	if c.Status != Active {
		return CatalogItem{}, statusConflict("catalog item", c.Status, "retire", Active)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.catalog_item SET status = 'retired', version = version + 1, updated_by = $2
		WHERE id = $1 AND status = 'active'`, id, p.ID); err != nil {
		return CatalogItem{}, fmt.Errorf("procurement: retire catalog item %s: %w", id, err)
	}
	retired, err := getCatalogItem(ctx, tx, id, false)
	if err != nil {
		return CatalogItem{}, err
	}
	return retired, e.catalogChanged(ctx, retired, "retire", &c.Status)
}

// catalogChanged records a catalog item's change with its event
// (ADR-0071 §4): CatalogItemChanged names the supplier by id and number,
// never by name (product owner, 2026-11-05).
func (e *Endpoints) catalogChanged(ctx context.Context, c CatalogItem, action string, before *string) error {
	names, err := e.SupplierNames.Names(ctx, []uuid.UUID{c.SupplierID})
	if err != nil {
		return err
	}
	payload := map[string]any{"catalog_item_id": c.ID, "legal_entity_id": c.LegalEntityID, "supplier_id": c.SupplierID,
		"supplier_number": names[c.SupplierID].Number, "item_key": c.ItemKey, "unit": c.Unit,
		"unit_price": money.EventAmount(c.UnitPrice, c.Currency, c.Exponent), "price_unit": c.PriceUnit, "contract_id": c.ContractID,
		"contract_item_no": c.ContractItemNo, "status": c.Status, "status_before": before, "version": c.Version}
	return audit.Record(ctx, "procurement.catalog_item."+action, "procurement.catalog_item", c.ID,
		map[string]any{"item_key": c.ItemKey, "supplier_id": c.SupplierID, "status": c.Status},
		event("catalog_item_changed", "catalog_item", c.ID, payload))
}
