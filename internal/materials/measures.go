package materials

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/gtin"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Dimensions, weights and GTINs (S1a3; ADR-0082 As built S1a3): each unit
// of measure a material is kept in, its base unit and each alternative
// unit, may carry its gross and net weight in one unit of mass, its
// volume in a unit of volume, its length, width and height in one unit
// of length and its GTIN, as SAP keeps them per unit in MARM (from
// memory). Values are decimal strings, never floats, of up to ten digits
// and three decimals; a GTIN names one unit of one material in the tenant
// (materials.unit_measure_gtin_once).

func init() {
	problem.RegisterConstraint("unit_measure_gtin_once", func() *problem.Problem {
		return problem.Conflict("materials.gtin_taken", "Another material's unit of measure carries this GTIN; a GTIN names one unit of one material.")
	})
}

// Measures are a unit of measure's dimensions, weights and GTIN, each ""
// when unknown; values are decimal strings without trailing zeros
// (canonical), each in the unit of its kind beside it.
type Measures struct {
	GTIN          string `json:"gtin,omitempty"`
	GrossWeight   string `json:"gross_weight,omitempty"`
	NetWeight     string `json:"net_weight,omitempty"`
	WeightUnit    string `json:"weight_unit,omitempty"`
	Volume        string `json:"volume,omitempty"`
	VolumeUnit    string `json:"volume_unit,omitempty"`
	Length        string `json:"length,omitempty"`
	Width         string `json:"width,omitempty"`
	Height        string `json:"height,omitempty"`
	DimensionUnit string `json:"dimension_unit,omitempty"`
}

// IsZero reports whether the unit has no measures.
func (m Measures) IsZero() bool { return m == Measures{} }

// canonical is m with each value written as the database answers it
// (trim_scale): no trailing zeros after the point, nor the point when
// nothing follows it, so "2.50" and "2.5" compare equal. A value that is
// no decimal is left for check to refuse.
func (m Measures) canonical() Measures {
	for _, v := range []*string{&m.GrossWeight, &m.NetWeight, &m.Volume, &m.Length, &m.Width, &m.Height} {
		if decimal.MatchString(*v) && strings.Contains(*v, ".") {
			*v = strings.TrimSuffix(strings.TrimRight(*v, "0"), ".")
		}
	}
	return m
}

// decimal is a measure's value as the contract and the database take it:
// up to ten digits and three decimals (numeric(13,3); SAP's QUAN 13,3,
// from memory), so nothing is rounded on write.
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})(\.[0-9]{1,3})?$`)

// dimensions are the units of measure's kinds by code
// (ref.unit_of_measure.dimension), as unitDimensions reads them.
type dimensions map[string]string

// unitDimensions reads the kind of each unit of measure the measures name.
func unitDimensions(ctx context.Context, tx pgx.Tx, ms []Measures) (dimensions, error) {
	codes := []string{}
	for _, m := range ms {
		codes = append(codes, m.WeightUnit, m.VolumeUnit, m.DimensionUnit)
	}
	rows, _ := tx.Query(ctx, `SELECT code, dimension FROM ref.unit_of_measure WHERE code = ANY($1)`, codes)
	out := dimensions{}
	var code, dimension string
	_, err := pgx.ForEachRow(rows, []any{&code, &dimension}, func() error {
		out[code] = dimension
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("materials: read the kinds of %d units of measure: %w", len(codes), err)
	}
	return out, nil
}

// measureGroup is one kind of a unit's measures: its values with their
// fields, and its unit, which is of the dimension named.
type measureGroup struct {
	values    map[string]string // field: value
	order     []string
	unitField string
	unit      string
	dimension string
}

func (m Measures) groups() []measureGroup {
	return []measureGroup{
		{values: map[string]string{"gross_weight": m.GrossWeight, "net_weight": m.NetWeight}, order: []string{"gross_weight", "net_weight"},
			unitField: "weight_unit", unit: m.WeightUnit, dimension: "mass"},
		{values: map[string]string{"volume": m.Volume}, order: []string{"volume"}, unitField: "volume_unit", unit: m.VolumeUnit, dimension: "volume"},
		{values: map[string]string{"length": m.Length, "width": m.Width, "height": m.Height}, order: []string{"length", "width", "height"},
			unitField: "dimension_unit", unit: m.DimensionUnit, dimension: "length"},
	}
}

// check refuses measures a unit cannot carry (field names them, as
// "base_unit_measures"): a value that is no decimal above zero, a value
// without its unit or a unit without a value, a unit unknown (422
// materials.unknown_unit) or of another kind, a net weight above the
// gross (422 materials.invalid_measures), and a GTIN that is none (422
// materials.invalid_gtin).
func (m Measures) check(field string, dims dimensions) error {
	for _, g := range m.groups() {
		given := false
		for _, f := range g.order {
			v := g.values[f]
			if v == "" {
				continue
			}
			given = true
			if !decimal.MatchString(v) || strings.Trim(v, "0.") == "" {
				return problem.Invalid("materials.invalid_measures", field+"."+f, fmt.Sprintf("%q is no decimal above zero of up to ten digits "+
					"and three decimals.", v))
			}
		}
		if err := g.checkUnit(field, given, dims); err != nil {
			return err
		}
	}
	if m.NetWeight != "" && m.GrossWeight != "" && rat(m.NetWeight).Cmp(rat(m.GrossWeight)) > 0 {
		return problem.Invalid("materials.invalid_measures", field+".net_weight", "The net weight "+m.NetWeight+" is more than the gross weight "+
			m.GrossWeight+"; the gross weight includes the packaging.")
	}
	if m.GTIN != "" && !gtin.Valid(m.GTIN) {
		return problem.Invalid("materials.invalid_gtin", field+".gtin", fmt.Sprintf("%q is no GTIN: 8, 12, 13 or 14 digits ending in their "+
			"GS1 check digit.", m.GTIN))
	}
	return nil
}

// checkUnit refuses a group's unit: missing beside a value, given without
// one, unknown, or of another kind.
func (g measureGroup) checkUnit(field string, given bool, dims dimensions) error {
	dimension, known := dims[g.unit]
	switch {
	case given && g.unit == "":
		return problem.Invalid("materials.invalid_measures", field+"."+g.unitField, "A unit of "+g.dimension+" is needed for "+
			strings.Join(g.order, ", ")+".")
	case !given && g.unit != "":
		return problem.Invalid("materials.invalid_measures", field+"."+g.unitField, g.unit+" is given without "+strings.Join(g.order, " or ")+
			"; leave it out.")
	case g.unit == "":
		return nil
	case !known:
		return problem.Invalid("materials.unknown_unit", field+"."+g.unitField, g.unit+" is not a unit of measure.")
	case dimension != g.dimension:
		return problem.Invalid("materials.invalid_measures", field+"."+g.unitField, g.unit+" is a unit of "+dimension+", not of "+g.dimension+".")
	}
	return nil
}

// rat is a decimal check has accepted, exactly.
func rat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s) // decimal matched it, so it parses
	return r
}

// unitMeasures are the measures a request gives a material, by unit:
// its base unit's and each alternative unit's, with the field each is
// checked under.
type unitMeasures struct {
	units  []string
	fields []string
	ms     []Measures
}

func measuresOf(base string, baseM *Measures, units []UnitInput) unitMeasures {
	var out unitMeasures
	if baseM != nil {
		out.units, out.fields, out.ms = []string{base}, []string{"base_unit_measures"}, []Measures{*baseM}
	}
	for i, u := range units {
		out.units, out.fields = append(out.units, u.Code), append(out.fields, fmt.Sprintf("alternative_units[%d].measures", i))
		out.ms = append(out.ms, u.Measures)
	}
	return out
}

// checkMeasures checks a request's measures (Measures.check), each GTIN
// on one unit only (422 materials.duplicate_gtin) and on no unit of
// another material than self, whatever its status (409
// materials.gtin_taken, naming it); the database's unique index holds the
// GTIN when it is written, so two requests at once cannot both take one.
func checkMeasures(ctx context.Context, tx pgx.Tx, self uuid.UUID, um unitMeasures) error {
	dims, err := unitDimensions(ctx, tx, um.ms)
	if err != nil {
		return err
	}
	keys, fields := []string{}, map[string]string{}
	for i, m := range um.ms {
		if err := m.check(um.fields[i], dims); err != nil {
			return err
		}
		if m.GTIN == "" {
			continue
		}
		key := gtin.Key(m.GTIN)
		if _, twice := fields[key]; twice {
			return problem.Invalid("materials.duplicate_gtin", um.fields[i]+".gtin", "GTIN "+m.GTIN+" is given to "+fields[key]+" too; a GTIN names one unit.")
		}
		keys, fields[key] = append(keys, key), um.fields[i]
	}
	var number, unit, given string
	err = tx.QueryRow(ctx, `SELECT m.number, x.uom_code, x.gtin FROM materials.unit_measure x JOIN materials.material m ON m.tenant_id = x.tenant_id AND m.id = x.material_id
		WHERE lpad(x.gtin, 14, '0') = ANY($1) AND x.gtin IS NOT NULL AND x.removed_at IS NULL AND x.material_id <> $2 ORDER BY m.number, x.uom_code LIMIT 1`,
		keys, self).Scan(&number, &unit, &given)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("materials: find the materials of %d GTINs: %w", len(keys), err)
	}
	return problem.Conflict("materials.gtin_taken", "GTIN "+given+" is material "+number+"'s, unit "+unit+"; a GTIN names one unit of one material.")
}

// measureColumns are a unit_measure row's measures as materialSelect
// reads them, x the row: a JSON object of the values given, each written
// without trailing zeros.
const measureColumns = `json_strip_nulls(json_build_object('gtin', x.gtin, 'gross_weight', trim_scale(x.gross_weight)::text,
	'net_weight', trim_scale(x.net_weight)::text, 'weight_unit', x.weight_uom, 'volume', trim_scale(x.volume)::text, 'volume_unit', x.volume_uom,
	'length', trim_scale(x.length)::text, 'width', trim_scale(x.width)::text, 'height', trim_scale(x.height)::text, 'dimension_unit', x.dimension_uom))`

// lockGTINs takes the GTINs' advisory locks, by key in key order, held
// to the end of the transaction (ADR-0008). Every writer of a GTIN takes
// them before it writes one, so two writers waiting on unique index
// entries each holds (one material's change taking X then Y, another's Y
// then X) wait in one order instead, the second then refused 409
// materials.gtin_taken; the SAP migration run takes all its GTINs first
// (Migration.LockGTINs), after its material numbers, as a creation draws
// its number before it writes its measures.
func lockGTINs(ctx context.Context, tx pgx.Tx, gtins []string) error {
	keys := make([]string, 0, len(gtins))
	for _, g := range gtins {
		keys = append(keys, gtin.Key(g))
	}
	slices.Sort(keys)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('materials.gtin:' || platform.require_tenant() || ':' || x.k, 0))
		FROM unnest($1::text[]) WITH ORDINALITY AS x (k, n) ORDER BY x.n`, slices.Compact(keys)); err != nil {
		return fmt.Errorf("materials: lock %d GTINs: %w", len(keys), err)
	}
	return nil
}

// writeMeasures writes the measures of the units given that have any,
// setting those of a unit that has, or had and lost, some rather than
// writing them twice; a unit without measures writes nothing. Their GTINs
// are locked first (lockGTINs).
func writeMeasures(ctx context.Context, tx pgx.Tx, p reqctx.Principal, material uuid.UUID, um unitMeasures) error {
	cols := make([][]string, 11)
	var gtins []string
	for i, m := range um.ms {
		if m.IsZero() {
			continue
		}
		if m.GTIN != "" {
			gtins = append(gtins, m.GTIN)
		}
		for j, v := range []string{um.units[i], m.GTIN, m.GrossWeight, m.NetWeight, m.WeightUnit, m.Volume, m.VolumeUnit, m.Length, m.Width, m.Height,
			m.DimensionUnit} {
			cols[j] = append(cols[j], v)
		}
	}
	if len(cols[0]) == 0 {
		return nil
	}
	if err := lockGTINs(ctx, tx, gtins); err != nil {
		return err
	}
	args := []any{p.TenantID, material, p.ID}
	for _, c := range cols {
		args = append(args, c)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO materials.unit_measure (tenant_id, material_id, uom_code, created_by, gtin, gross_weight, net_weight, weight_uom,
			volume, volume_uom, length, width, height, dimension_uom)
		SELECT $1, $2, x.unit, $3, nullif(x.gtin, ''), nullif(x.gross, '')::numeric, nullif(x.net, '')::numeric, nullif(x.wu, ''),
			nullif(x.vol, '')::numeric, nullif(x.vu, ''), nullif(x.l, '')::numeric, nullif(x.w, '')::numeric, nullif(x.h, '')::numeric, nullif(x.du, '')
		FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::text[])
			AS x (unit, gtin, gross, net, wu, vol, vu, l, w, h, du)
		ON CONFLICT (tenant_id, material_id, uom_code) DO UPDATE SET gtin = EXCLUDED.gtin, gross_weight = EXCLUDED.gross_weight,
			net_weight = EXCLUDED.net_weight, weight_uom = EXCLUDED.weight_uom, volume = EXCLUDED.volume, volume_uom = EXCLUDED.volume_uom,
			length = EXCLUDED.length, width = EXCLUDED.width, height = EXCLUDED.height, dimension_uom = EXCLUDED.dimension_uom,
			removed_at = NULL, removed_by = NULL`, args...); err != nil {
		return fmt.Errorf("materials: measures of %d units of material %s: %w", len(cols[0]), material, err)
	}
	return nil
}

// removeMeasures marks removed the measures a change replaces: the base
// unit's when base, every alternative unit's when alternatives. They are
// marked before any is written again, so a GTIN moved from one unit of
// the material to another is never on two at once.
func removeMeasures(ctx context.Context, tx pgx.Tx, p reqctx.Principal, m Material, base, alternatives bool) error {
	if _, err := tx.Exec(ctx, `UPDATE materials.unit_measure SET removed_at = now(), removed_by = $5
		WHERE material_id = $1 AND removed_at IS NULL AND (($2 AND uom_code = $4) OR ($3 AND uom_code <> $4))`,
		m.ID, base, alternatives, m.BaseUnit, p.ID); err != nil {
		return fmt.Errorf("materials: remove the measures of material %s: %w", m.Number, err)
	}
	return nil
}

// measureDetail is the measures of a material's units as the audit
// names them, by unit; units without any are left out.
func measureDetail(base string, baseM Measures, units []Unit) map[string]Measures {
	out := map[string]Measures{}
	if !baseM.IsZero() {
		out[base] = baseM
	}
	for _, u := range units {
		if !u.Measures.IsZero() {
			out[u.Code] = u.Measures
		}
	}
	return out
}
