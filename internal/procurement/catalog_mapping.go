package procurement

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The catalog's keys mapped onto materials (S1f2; ADR-0082 As built
// S1f): a company's active catalog items that name no material are given
// the material whose number their key is, once, when the material is
// active and the item is ordered in one of its units. The catalog is the
// master data a requisition's items are taken from; documents already
// made keep their items as they are.

// Outcomes of a key in a mapping.
const (
	KeyMapped      = "mapped"
	KeyNoMaterial  = "no_material"
	KeyNotMappable = "not_mappable"
)

// KeyMapping is what a mapping did with one item key: its outcome, the
// material of its number (nil for KeyNoMaterial), why its items were not
// mapped (KeyNotMappable) and how many items it concerns.
type KeyMapping struct {
	Key      string
	Outcome  string
	Material *materials.Material
	Reason   string
	Items    int32
}

// CatalogMapping is a mapping's report: the company, its keys in order,
// and the items of each outcome.
type CatalogMapping struct {
	Company tenancy.Entity
	Keys    []KeyMapping
	Items   map[string]int32 // by outcome
}

// mapCatalogMaterials maps a company's catalog keys onto materials. The
// unmapped active items are locked in the order of their keys and ids,
// so two runs of one company wait for each other and the second finds
// what the first mapped; each item mapped is set once (the database
// refuses a changed material, catalog_item_material_fixed), its version
// raised, audited and published.
func (e *Endpoints) mapCatalogMaterials(ctx context.Context, company uuid.UUID) (CatalogMapping, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CatalogMapping{}, err
	}
	le, err := e.legalEntity(ctx, company)
	if err != nil {
		return CatalogMapping{}, err
	}
	rows, _ := tx.Query(ctx, catalogSelect+` WHERE ci.legal_entity_id = $1 AND ci.status = 'active' AND ci.material_id IS NULL
		ORDER BY ci.item_key, ci.id FOR UPDATE OF ci`, le.ID)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CatalogItem, error) { return scanCatalogItem(r) })
	if err != nil {
		return CatalogMapping{}, fmt.Errorf("procurement: unmapped catalog items of %s: %w", le.Code, err)
	}
	keys := make([]string, 0, len(items))
	for i, c := range items {
		if i == 0 || items[i-1].ItemKey != c.ItemKey {
			keys = append(keys, c.ItemKey)
		}
	}
	ms, err := e.Materials.ByNumber(ctx, keys)
	if err != nil {
		return CatalogMapping{}, fmt.Errorf("procurement: materials of %d catalog keys: %w", len(keys), err)
	}
	out := CatalogMapping{Company: le, Items: map[string]int32{}}
	for start := 0; start < len(items); {
		end := start + 1
		for end < len(items) && items[end].ItemKey == items[start].ItemKey {
			end++
		}
		got, err := e.mapKey(ctx, tx, p, items[start:end], ms)
		if err != nil {
			return CatalogMapping{}, err
		}
		for _, k := range got {
			out.Items[k.Outcome] += k.Items
		}
		out.Keys, start = append(out.Keys, got...), end
	}
	return out, nil
}

// mapKey maps the items of one key onto the material of that number: an
// active material's, each item ordered in one of its units. It reports
// the key's mapped items and those left, with why, in that order.
func (e *Endpoints) mapKey(ctx context.Context, tx pgx.Tx, p reqctx.Principal, items []CatalogItem, ms map[string]materials.Material) ([]KeyMapping, error) {
	key := items[0].ItemKey
	m, ok := ms[key]
	switch {
	case !ok:
		return []KeyMapping{{Key: key, Outcome: KeyNoMaterial, Items: int32(len(items))}}, nil
	case m.Status != materials.Active:
		return []KeyMapping{{Key: key, Outcome: KeyNotMappable, Material: &m, Reason: "material " + m.Number + " is " + m.Status,
			Items: int32(len(items))}}, nil
	}
	var mapped, left int32
	var units []string
	for _, c := range items {
		if _, err := m.Factor(c.Unit); err != nil {
			left++
			if !slices.Contains(units, c.Unit) {
				units = append(units, c.Unit)
			}
			continue
		}
		if err := e.mapItem(ctx, tx, p, c, m.ID); err != nil {
			return nil, err
		}
		mapped++
	}
	var out []KeyMapping
	if mapped > 0 {
		out = append(out, KeyMapping{Key: key, Outcome: KeyMapped, Material: &m, Items: mapped})
	}
	if left > 0 {
		out = append(out, KeyMapping{Key: key, Outcome: KeyNotMappable, Material: &m, Items: left,
			Reason: "ordered in " + strings.Join(units, ", ") + ", none of material " + m.Number + "'s units (its base unit " + m.BaseUnit +
				" or an alternative unit)"})
	}
	return out, nil
}

// mapItem gives a locked catalog item its material, raising its version,
// and audits and publishes the change.
func (e *Endpoints) mapItem(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c CatalogItem, material uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE procurement.catalog_item SET material_id = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		c.ID, material, p.ID); err != nil {
		return fmt.Errorf("procurement: map catalog item %s onto material %s: %w", c.ID, material, err)
	}
	after, err := getCatalogItem(ctx, tx, c.ID, false)
	if err != nil {
		return err
	}
	return e.catalogChanged(ctx, after, "map_material", &c.Status) // its status stays, as the event says (status_before)
}

// MapCatalogItemMaterials implements POST /v1/catalog-material-mappings.
func (e *Endpoints) MapCatalogItemMaterials(ctx context.Context, req *gen.CatalogMaterialMapping, _ gen.MapCatalogItemMaterialsParams) (gen.MapCatalogItemMaterialsRes, error) {
	got, err := e.mapCatalogMaterials(ctx, req.LegalEntityID)
	if err != nil {
		return nil, err
	}
	out := gen.CatalogMaterialMappingReport{LegalEntityID: got.Company.ID, CompanyCode: got.Company.Code, CompanyName: got.Company.Name,
		MappedItems: got.Items[KeyMapped], NoMaterialItems: got.Items[KeyNoMaterial], NotMappableItems: got.Items[KeyNotMappable], Keys: make([]gen.CatalogKeyMapping, len(got.Keys))}
	for i, k := range got.Keys {
		out.Keys[i] = gen.CatalogKeyMapping{ItemKey: k.Key, Outcome: k.Outcome, Reason: apitypes.OptText(k.Reason), Items: k.Items}
		if k.Material != nil {
			out.Keys[i].MaterialID = gen.NewOptUUID(k.Material.ID)
			out.Keys[i].MaterialNumber = gen.NewOptString(k.Material.Number)
			out.Keys[i].MaterialDescription = gen.NewOptString(k.Material.Description)
		}
	}
	return &gen.CatalogMaterialMappingReportHeaders{Response: out}, nil
}
