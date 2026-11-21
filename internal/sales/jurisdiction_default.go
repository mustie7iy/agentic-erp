package sales

import (
	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// The emirate an item defaults to (product owner 2026-11-21; ADR-0063 As
// built, the emirate on the item; ADR-0067 As built, the emirate on a
// sales item). A UAE item's tax code (S5) levies the emirate its lines
// name, and the VAT 201 reports a sale by the emirate of the supplier's
// establishment most closely connected with it (ADR-0049 §11): the
// seller's side, never the customer's or the ship-to address. An item
// that names none therefore takes, when it is entered, its plant's region
// for an item of a material kept in stock of an order with a plant, else
// the company's establishment, the region of its postal address in the
// configuration. An item may name another, which is kept; the audit
// record of the document says which items defaulted and which differ
// from their default. A default the item's code does not levy is no
// default: the item is refused as before (sales.tax_jurisdiction_required).

// defaultedCountry is the one country whose items default their
// jurisdiction: the product owner's decision is the UAE's (VAT 201 boxes
// 1a-1g by emirate); another country's codes that levy their lines'
// subdivision keep the jurisdiction entered (ADR-0063 As built).
const defaultedCountry = "AE"

// defaultJurisdiction is the jurisdiction item it of the code defaults
// to, nil for none: only an item entered now (not one an order change
// keeps) whose code levies the UAE's emirates, from its plant for an item
// of a material kept in stock, else from the company's establishment, when
// the code levies that one.
func (c itemCheck) defaultJurisdiction(it ItemInput, code ledger.TaxCode) *string {
	fault, component := code.CheckJurisdiction(nil)
	if it.Kept || fault != ledger.JurisdictionRequired || component.Jurisdiction != defaultedCountry {
		return nil
	}
	candidate := c.doc.establishment
	if it.MaterialID != nil && c.materials[*it.MaterialID].Stocked() && c.doc.plant != nil && c.doc.plant.Region != nil {
		candidate = c.doc.plant.Region
	}
	if candidate == nil {
		return nil
	}
	if fault, _ := code.CheckJurisdiction(candidate); fault != ledger.JurisdictionOK {
		return nil
	}
	return candidate
}

// jurisdictionDefaults is the audit detail of the items that had a
// default jurisdiction, nil for none: each item's number, its default and
// the jurisdiction it was entered with, and whether that differs from the
// default (a person named another).
func jurisdictionDefaults(items []Item) []map[string]any {
	var out []map[string]any
	for _, it := range items {
		if it.jurisdictionDefault == nil {
			continue
		}
		out = append(out, map[string]any{"item_no": it.ItemNo, "default": *it.jurisdictionDefault, "tax_jurisdiction": it.TaxJurisdiction,
			"overridden": *it.TaxJurisdiction != *it.jurisdictionDefault}) // an item with a default names a jurisdiction (itemCheck.tax)
	}
	return out
}
