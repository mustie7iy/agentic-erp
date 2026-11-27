package integration

import (
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/gtin"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The material master's measures (S1a3; ADR-0082 As built S1a3): the
// base unit's gross and net weight, volume and EAN/UPC from MARA (BRGEW,
// NTGEW, GEWEI, VOLUM, VOLEH, EAN11) with its length, width and height
// from its own MARM row (LAENG, BREIT, HOEHE, MEABM), and each
// alternative unit's from its MARM row, which has no net weight (field
// meanings from memory). Measures are optional master data: one the
// platform cannot hold is left out with a warning, never holding the
// material back.

// unitFields are one unit's measure fields as SAP writes them.
type unitFields struct {
	gross, net, weightUnit, volume, volumeUnit, length, width, height, dimensionUnit, ean string
}

// sapDecimal is a quantity as an extract writes one: digits, with a
// decimal point and decimals or none.
var sapDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// quantity is an SAP quantity as the platform keeps a measure: written
// without leading or trailing zeros, "" for zero or empty, false when it
// is no decimal, has more than three decimals or ten whole digits.
func quantity(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	if !sapDecimal.MatchString(s) {
		return "", false
	}
	whole, frac, _ := strings.Cut(s, ".")
	whole, frac = strings.TrimLeft(whole, "0"), strings.TrimRight(frac, "0")
	switch {
	case len(frac) > 3 || len(whole) > 10:
		return "", false
	case whole == "" && frac == "":
		return "", true // zero: SAP's none
	case whole == "":
		whole = "0"
	}
	if frac != "" {
		return whole + "." + frac, true
	}
	return whole, true
}

// measureKind is one kind of a unit's measures as the load reads it.
type measureKind struct {
	name, dimension, unit string
	values                []*string
}

// measures are a unit's measures from its SAP fields: each kind with its
// values, in a unit of the platform of that kind, and the EAN a GTIN;
// what is not is left out with a warning, of the table given (MARA for
// the base unit, MARM for an alternative unit).
func (mp *materialPlan) measures(table, matnr, unit string, f unitFields) materials.Measures {
	m := materials.Measures{}
	kinds := []measureKind{
		{name: "the weights are", dimension: "mass", unit: f.weightUnit, values: []*string{&f.gross, &f.net}},
		{name: "the volume is", dimension: "volume", unit: f.volumeUnit, values: []*string{&f.volume}},
		{name: "the dimensions are", dimension: "length", unit: f.dimensionUnit, values: []*string{&f.length, &f.width, &f.height}},
	}
	key := matnr + "/" + unit
	for i := range kinds {
		if !mp.measureKind(table, key, &kinds[i]) {
			kinds[i].unit = ""
		}
	}
	if kinds[0].unit != "" {
		m.GrossWeight, m.NetWeight, m.WeightUnit = f.gross, f.net, f.weightUnit
		if m.GrossWeight != "" && m.NetWeight != "" && ratOf(m.NetWeight).Cmp(ratOf(m.GrossWeight)) > 0 {
			mp.add(SevWarning, table, key, "the net weight %s %s is more than the gross weight %s; the weights are left out", m.NetWeight, m.WeightUnit,
				m.GrossWeight)
			m.GrossWeight, m.NetWeight, m.WeightUnit = "", "", ""
		}
	}
	if kinds[1].unit != "" {
		m.Volume, m.VolumeUnit = f.volume, f.volumeUnit
	}
	if kinds[2].unit != "" {
		m.Length, m.Width, m.Height, m.DimensionUnit = f.length, f.width, f.height, f.dimensionUnit
	}
	switch ean := strings.TrimSpace(f.ean); {
	case ean == "":
	case gtin.Valid(ean):
		m.GTIN = ean
	default:
		mp.add(SevWarning, table, key, "EAN/UPC %q is no GTIN (8, 12, 13 or 14 digits ending in their GS1 check digit); it is left out", ean)
	}
	return m
}

// measureKind reads one kind's values in place and reports whether the
// unit keeps them: false, with a warning, for a value that is no
// quantity the platform keeps or a unit that is none of its kind, and
// false, silently, when every value is zero or empty (SAP keeps a weight
// unit with no weight).
func (mp *materialPlan) measureKind(table, key string, k *measureKind) bool {
	given := false
	for _, v := range k.values {
		q, ok := quantity(*v)
		if !ok {
			mp.add(SevWarning, table, key, "%s left out: %q is no quantity above zero of up to ten digits and three decimals", k.name, *v)
			return false
		}
		*v, given = q, given || q != ""
	}
	switch {
	case !given:
		return false
	case mp.units[k.unit] != k.dimension:
		mp.add(SevWarning, table, key, "%s left out: unit %q is no unit of %s of the platform", k.name, k.unit, k.dimension)
		return false
	}
	return true
}

// ratOf is a quantity the load accepted, exactly.
func ratOf(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s) // quantity wrote it as a decimal
	return r
}

// baseMeasures are the base unit's measures: MARA's weights, volume and
// EAN with the length, width and height of its own MARM row.
func (mp *materialPlan) baseMeasures(r sap.MARA) materials.Measures {
	f := unitFields{gross: r.BRGEW, net: r.NTGEW, weightUnit: r.GEWEI, volume: r.VOLUM, volumeUnit: r.VOLEH, ean: r.EAN11}
	if i := slices.IndexFunc(mp.marm[r.MATNR], func(u sap.MARM) bool { return u.MEINH == r.MEINS }); i >= 0 {
		u := mp.marm[r.MATNR][i]
		f.length, f.width, f.height, f.dimensionUnit = u.LAENG, u.BREIT, u.HOEHE, u.MEABM
	}
	return mp.measures("MARA", r.MATNR, r.MEINS, f)
}

// sharedGTINs leaves out, with a warning, a GTIN the extract gives two
// units, of one material or two, compared as GS1 compares them: which of
// them it names is SAP's to settle, so none of them loads it.
func (mp *materialPlan) sharedGTINs() {
	count := map[string]int{}
	each := func(fn func(table, matnr, unit string, m *materials.Measures)) {
		for i := range mp.materials {
			mat := &mp.materials[i]
			fn("MARA", mat.number, mat.baseUnit, &mat.baseMeasures)
			for j := range mat.units {
				fn("MARM", mat.number, mat.units[j].Code, &mat.units[j].Measures)
			}
		}
	}
	each(func(_, _, _ string, m *materials.Measures) {
		if m.GTIN != "" {
			count[gtin.Key(m.GTIN)]++
		}
	})
	each(func(table, matnr, unit string, m *materials.Measures) {
		if m.GTIN != "" && count[gtin.Key(m.GTIN)] > 1 {
			mp.add(SevWarning, table, matnr+"/"+unit, "EAN/UPC %s is another unit's too; it is left out of both", m.GTIN)
			m.GTIN = ""
		}
	})
}
