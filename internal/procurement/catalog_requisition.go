package procurement

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// CatalogReadPermission is what searching and reading the catalog needs;
// a requisition item taken from the catalog reads a catalog item, so its
// requester needs it too (ADR-0071 §6).
const CatalogReadPermission = "procurement.catalog.read"

// catalogUse is a catalog item a requisition's item names: where the item
// is in the requisition, for a refusal to name it.
type catalogUse struct {
	field string // items[i].catalog_item_id
	item  CatalogItem
}

// fromCatalog resolves the requisition items that name a catalog item
// (ADR-0071 §6): each takes the catalog item's description, unit, price
// and price unit and may send no other (noOverride). The catalog item is an
// active one of the requisition's legal entity, priced in the
// requisition's currency. It returns the items as requisitionItems checks
// them and the catalog items used, in the items' order.
func fromCatalog(ctx context.Context, tx pgx.Tx, le uuid.UUID, currency string, in []ItemInput) ([]ItemInput, []catalogUse, error) {
	out := make([]ItemInput, len(in))
	var used []catalogUse
	for i, it := range in {
		out[i] = it
		field := fmt.Sprintf("items[%d]", i)
		if it.CatalogItemID == nil {
			if it.Price == "" && it.Currency == "" {
				return nil, nil, problem.Unprocessable("request.validation_failed", "The request is not valid: "+field+".unit_price is required for an item that names no catalog item.",
					problem.FieldError{Field: field + ".unit_price", Code: "required", Message: "a price, for an item that names no catalog item"})
			}
			continue
		}
		if used == nil {
			if err := authz.Require(ctx, CatalogReadPermission); err != nil {
				return nil, nil, err
			}
		}
		c, err := usableCatalogItem(ctx, tx, *it.CatalogItemID, le, currency, field+".catalog_item_id")
		if err != nil {
			return nil, nil, err
		}
		if err := noOverride(it, c, field); err != nil {
			return nil, nil, err
		}
		out[i].Description, out[i].MaterialKey, out[i].PriceUnit = c.Description, c.ItemKey, c.PriceUnit
		out[i].Price, out[i].Currency = c.UnitPrice.Plain(c.Exponent), c.Currency
		used = append(used, catalogUse{field: field + ".catalog_item_id", item: c})
	}
	return out, used, nil
}

// usableCatalogItem reads the catalog item a requisition item names and
// refuses, naming the item, one that is not there for the caller, of
// another legal entity, retired, or priced in another currency than the
// requisition's.
func usableCatalogItem(ctx context.Context, tx pgx.Tx, id, le uuid.UUID, currency, field string) (CatalogItem, error) {
	c, err := getCatalogItem(ctx, tx, id, false)
	switch {
	case err != nil && problem.From(err).Status == http.StatusNotFound:
		return CatalogItem{}, invalid("procurement.invalid_catalog_item", field, "no catalog item with this id that you can access")
	case err != nil:
		return CatalogItem{}, err
	case c.LegalEntityID != le:
		return CatalogItem{}, invalid("procurement.invalid_catalog_item", field, "catalog item "+c.ItemKey+" is another legal entity's")
	case c.Status != Active:
		return CatalogItem{}, invalid("procurement.invalid_catalog_item", field, "catalog item "+c.ItemKey+" is "+c.Status)
	case c.Currency != currency:
		return CatalogItem{}, invalid("procurement.invalid_catalog_item", field,
			"catalog item "+c.ItemKey+" is priced in "+c.Currency+", not the requisition currency "+currency)
	}
	return c, nil
}

// noOverride refuses what an item from the catalog may not give: a price,
// a price unit or a description other than the catalog item's, or
// another unit (plan default 18: the catalog's price cannot be
// overridden). One equal to the catalog item's is accepted (product
// owner, 2026-11-12), so a requisition read back, whose items carry what
// they took, can be sent again as it was read. The price is compared as
// the API shows it, the plain amount with the currency's decimals.
func noOverride(it ItemInput, c CatalogItem, field string) error {
	const code = "procurement.catalog_item_override"
	price := c.UnitPrice.Plain(c.Exponent)
	switch {
	case (it.Price != "" || it.Currency != "") && (it.Price != price || it.Currency != c.Currency):
		return invalid(code, field+".unit_price", fmt.Sprintf("the price is catalog item %s's, %s %s; send that or none", c.ItemKey, price, c.Currency))
	case it.PriceUnit != 0 && it.PriceUnit != c.PriceUnit:
		return invalid(code, field+".price_unit", fmt.Sprintf("the price unit is catalog item %s's, %d; send that or none", c.ItemKey, c.PriceUnit))
	case it.Description != "" && it.Description != c.Description:
		return invalid(code, field+".description", "the description is catalog item "+c.ItemKey+"'s; send that or none")
	case it.Unit != c.Unit:
		return invalid(code, field+".quantity.unit", "catalog item "+c.ItemKey+" is ordered in "+c.Unit)
	}
	return nil
}

// catalogParties is the supplier and the contract of a requisition whose
// items use catalog items (ADR-0071 §6): the catalog items' supplier, and
// the contract a catalog item names. What the requisition does not name
// it takes from them; another supplier or contract is refused, naming
// the item.
func catalogParties(supplier, contract *uuid.UUID, used []catalogUse) (*uuid.UUID, *uuid.UUID, error) {
	for _, u := range used {
		c := u.item
		if supplier == nil {
			supplier = &c.SupplierID
		}
		if *supplier != c.SupplierID {
			return nil, nil, invalid("procurement.catalog_item_supplier", u.field,
				"catalog item "+c.ItemKey+" is another supplier's than the requisition's")
		}
		if c.ContractID == nil {
			continue
		}
		if contract == nil {
			contract = c.ContractID
		}
		if *contract != *c.ContractID {
			return nil, nil, invalid("procurement.catalog_item_contract", u.field,
				"catalog item "+c.ItemKey+" is priced by contract "+*c.ContractNumber+", another than the requisition's")
		}
	}
	return supplier, contract, nil
}

// storedCatalogUse is the catalog items a stored requisition's items
// name, for a change of its supplier or contract that leaves the items
// as they are: retired or not, they still say whose the requisition is.
func storedCatalogUse(ctx context.Context, tx pgx.Tx, items []Item) ([]catalogUse, error) {
	var used []catalogUse
	for i, it := range items {
		if it.CatalogItemID == nil {
			continue
		}
		c, err := getCatalogItem(ctx, tx, *it.CatalogItemID, false)
		if err != nil {
			return nil, err
		}
		used = append(used, catalogUse{field: fmt.Sprintf("items[%d].catalog_item_id", i), item: c})
	}
	return used, nil
}

// requisitionParties checks who a requisition is with and resolves its
// items from the catalog: the items sent (in, nil for a change that
// leaves the stored items), the supplier and contract the catalog items
// give or require, the contract as checkRequisitionContract requires it,
// and a supplier that can be ordered from in the legal entity.
func (e *Endpoints) requisitionParties(ctx context.Context, tx pgx.Tx, le uuid.UUID, currency string, supplier, contract *uuid.UUID,
	in []ItemInput, stored []Item) (*uuid.UUID, *uuid.UUID, []ItemInput, error) {
	var used []catalogUse
	var err error
	if in != nil {
		in, used, err = fromCatalog(ctx, tx, le, currency, in)
	} else {
		used, err = storedCatalogUse(ctx, tx, stored)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if supplier, contract, err = catalogParties(supplier, contract, used); err != nil {
		return nil, nil, nil, err
	}
	if supplier, err = checkRequisitionContract(ctx, tx, contract, le, currency, supplier); err != nil {
		return nil, nil, nil, err
	}
	if supplier != nil {
		if err := e.supplier(ctx, *supplier, le); err != nil {
			return nil, nil, nil, err
		}
	}
	return supplier, contract, in, nil
}
