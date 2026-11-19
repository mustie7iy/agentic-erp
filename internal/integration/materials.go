package integration

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The material master's migration (S2o; ADR-0082 As built S2o): SAP's
// plants (T001W) and storage locations (T001L) become tenancy's org units,
// and its materials (MARA with one description of MAKT, the units of
// MARM, the plants of MARC and the storage locations of MARD) the
// materials context's, each kept by its SAP key as an external reference.
// What the platform cannot hold of a material is no error of the run: the
// material is held back with its reason, a warning, and the rest loads;
// the reconciliation leaves held-back materials out on SAP's side, as it
// leaves out the invoices payables refuses as duplicates.

// orgUnitCode is tenancy.org_unit's code: what a plant's or storage
// location's SAP key must be to be one.
var orgUnitCode = regexp.MustCompile(`^[A-Z0-9_]{1,10}$`)

// materialGroup is materials.material's group (ADR-0082 §4).
var materialGroup = regexp.MustCompile(`^[A-Z0-9_-]{1,9}$`)

// materialTypes are the platform's material types by SAP's standard
// material type (MARA-MTART; ADR-0082 §4, from memory). A material of
// another type is held back.
var materialTypes = map[string]string{sap.MaterialTypeRaw: "raw_material", sap.MaterialTypeSemiFinished: "semi_finished",
	sap.MaterialTypeFinished: "finished_good", sap.MaterialTypeTrading: "trading_good", sap.MaterialTypeNonStock: "non_stock",
	sap.MaterialTypeService: "service"}

// maxFactor is the largest numerator or denominator of a conversion the
// platform keeps (materials.unit_conversion, ADR-0082 §4).
const maxFactor = 1_000_000

// sapPlant is a plant of the extract (T001W) with its company code and
// its storage locations (T001L), in code order.
type sapPlant struct {
	werks, name, company string
	locations            []sapLocation
}

// sapLocation is a storage location of a plant (T001L).
type sapLocation struct{ lgort, name string }

// sapMaterial is a material the load creates: its SAP number, its
// platform type, group, base unit and description, its alternative units
// in code order and its plants in plant order, each with its storage
// locations in code order.
type sapMaterial struct {
	number, mtype, group, baseUnit, description string
	units                                       []materials.UnitInput
	plants                                      []sapMaterialPlant
}

type sapMaterialPlant struct {
	werks     string
	locations []string
}

// knownUnits reads the platform's units of measure (ref.unit_of_measure),
// which a material's base and alternative units must be.
func knownUnits(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT code FROM ref.unit_of_measure`)
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("integration: read the units of measure: %w", err)
	}
	out := make(map[string]bool, len(codes))
	for _, c := range codes {
		out[c] = true
	}
	return out, nil
}

// plantsOf are the extract's plants in code order, each with its storage
// locations, and the errors that refuse them: a key that is no org unit
// code, no name, a row twice, a storage location of no plant, and a
// storage location's code at two plants of one company code, which
// tenancy keeps unique in a company where SAP keys it by its plant.
func plantsOf(e *sap.Extract) ([]sapPlant, []Issue) {
	var issues []Issue
	add := func(table, key, format string, args ...any) {
		issues = append(issues, Issue{Severity: SevError, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
	}
	byCode := map[string]*sapPlant{}
	rows := slices.SortedFunc(slices.Values(e.T001W), func(a, b sap.T001W) int { return cmp.Compare(a.WERKS, b.WERKS) })
	var plants []sapPlant
	for _, r := range rows {
		if _, twice := byCode[r.WERKS]; twice {
			add("T001W", r.WERKS, "the plant is listed twice")
			continue
		}
		orgUnitIssues("T001W", r.WERKS, r.WERKS, r.NAME1, "plant", add)
		plants = append(plants, sapPlant{werks: r.WERKS, name: r.NAME1, company: r.BUKRS})
		byCode[r.WERKS] = nil
	}
	for i := range plants {
		byCode[plants[i].werks] = &plants[i]
	}
	locations := slices.SortedFunc(slices.Values(e.T001L), func(a, b sap.T001L) int { return cmp.Compare(a.WERKS+"/"+a.LGORT, b.WERKS+"/"+b.LGORT) })
	at := map[string]string{} // company/LGORT -> the first plant with it
	for _, r := range locations {
		key := r.WERKS + "/" + r.LGORT
		p := byCode[r.WERKS]
		switch {
		case p == nil:
			add("T001L", key, "plant %s is not in T001W", r.WERKS)
			continue
		case slices.ContainsFunc(p.locations, func(l sapLocation) bool { return l.lgort == r.LGORT }):
			add("T001L", key, "the storage location is listed twice")
			continue
		case at[p.company+"/"+r.LGORT] != "":
			add("T001L", key, "storage location %s is also at plant %s of company code %s; the platform keeps a storage location's code unique "+
				"in its company, where SAP keys it by its plant", r.LGORT, at[p.company+"/"+r.LGORT], p.company)
		}
		orgUnitIssues("T001L", key, r.LGORT, r.LGOBE, "storage location", add)
		at[p.company+"/"+r.LGORT] = r.WERKS
		p.locations = append(p.locations, sapLocation{lgort: r.LGORT, name: r.LGOBE})
	}
	return plants, issues
}

// orgUnitIssues refuses a plant or storage location whose code is no org
// unit code or that has no name.
func orgUnitIssues(table, key, code, name, what string, add func(table, key, format string, args ...any)) {
	if !orgUnitCode.MatchString(code) {
		add(table, key, "the %s's code %q is not 1 to 10 capitals, digits or _, as an org unit's is", what, code)
	}
	if strings.TrimSpace(name) == "" {
		add(table, key, "the %s has no name", what)
	}
}

// materialPlan reads the extract's materials against its plants: the
// materials the load creates, in number order, and what it finds.
type materialPlan struct {
	units     map[string]bool // the platform's units of measure
	plants    map[string]sapPlant
	texts     map[string][]sapText
	marm      map[string][]sap.MARM
	marc      map[string][]sap.MARC
	mard      map[string][]sap.MARD // by MATNR/WERKS
	materials []sapMaterial
	issues    []Issue
}

func (mp *materialPlan) add(sev, table, key, format string, args ...any) {
	mp.issues = append(mp.issues, Issue{Severity: sev, Table: table, Key: key, Message: fmt.Sprintf(format, args...)})
}

// materialsOf are the extract's materials the load creates and the issues
// it finds: errors for rows that name a material, plant or storage
// location the extract does not have, and warnings for a material flagged
// for deletion, which is not migrated, and for one held back with its
// reasons (heldBack).
func materialsOf(e *sap.Extract, units map[string]bool, plants []sapPlant) ([]sapMaterial, []Issue) {
	mp := &materialPlan{units: units, plants: map[string]sapPlant{}, texts: map[string][]sapText{}, marm: map[string][]sap.MARM{},
		marc: map[string][]sap.MARC{}, mard: map[string][]sap.MARD{}}
	for _, p := range plants {
		mp.plants[p.werks] = p
	}
	for _, t := range e.MAKT {
		mp.texts[t.MATNR] = append(mp.texts[t.MATNR], sapText{lang: t.SPRAS, name: t.MAKTX})
	}
	for _, u := range e.MARM {
		mp.marm[u.MATNR] = append(mp.marm[u.MATNR], u)
	}
	mara := map[string]bool{}
	rows := slices.SortedFunc(slices.Values(e.MARA), func(a, b sap.MARA) int { return cmp.Compare(a.MATNR, b.MATNR) })
	for _, r := range rows {
		if mara[r.MATNR] {
			mp.add(SevError, "MARA", r.MATNR, "the material is listed twice")
		}
		mara[r.MATNR] = true
	}
	mp.plantData(e, mara)
	for i, r := range rows {
		if i == 0 || rows[i-1].MATNR != r.MATNR {
			mp.material(r)
		}
	}
	return mp.materials, mp.issues
}

// plantData indexes the materials' plant data (MARC) and storage location
// data (MARD), refusing a row of a material, plant or storage location
// the extract does not have.
func (mp *materialPlan) plantData(e *sap.Extract, mara map[string]bool) {
	marc := map[string]bool{}
	for _, r := range e.MARC {
		key := r.MATNR + "/" + r.WERKS
		switch _, plant := mp.plants[r.WERKS]; {
		case !mara[r.MATNR]:
			mp.add(SevError, "MARC", key, "material %s is not in MARA", r.MATNR)
		case !plant:
			mp.add(SevError, "MARC", key, "plant %s is not in T001W", r.WERKS)
		case marc[key]:
			mp.add(SevError, "MARC", key, "the material's plant data is listed twice")
		default:
			marc[key] = true
			mp.marc[r.MATNR] = append(mp.marc[r.MATNR], r)
		}
	}
	for _, r := range e.MARD {
		key := r.MATNR + "/" + r.WERKS
		switch {
		case !marc[key]:
			mp.add(SevError, "MARD", key+"/"+r.LGORT, "material %s has no plant data (MARC) at plant %s", r.MATNR, r.WERKS)
		case !slices.ContainsFunc(mp.plants[r.WERKS].locations, func(l sapLocation) bool { return l.lgort == r.LGORT }):
			mp.add(SevError, "MARD", key+"/"+r.LGORT, "storage location %s of plant %s is not in T001L", r.LGORT, r.WERKS)
		default:
			mp.mard[key] = append(mp.mard[key], r)
		}
	}
}

// material plans one material: left out when SAP flags it for deletion
// or it has no plant left, held back when the platform cannot hold it,
// created otherwise.
func (mp *materialPlan) material(r sap.MARA) {
	if r.LVORM == sap.Deleted {
		mp.add(SevWarning, "MARA", r.MATNR, "flagged for deletion in SAP (LVORM); not migrated")
		return
	}
	m := sapMaterial{number: r.MATNR, mtype: materialTypes[r.MTART], group: r.MATKL, baseUnit: r.MEINS, plants: mp.materialPlants(r.MATNR)}
	if len(m.plants) == 0 {
		mp.add(SevWarning, "MARA", r.MATNR, "every plant of the scope flags it for deletion (MARC LVORM); not migrated")
		return
	}
	var reasons []string
	if m.mtype == "" {
		reasons = append(reasons, fmt.Sprintf("material type %q is none of ROH, HALB, FERT, HAWA, NLAG and DIEN, which the platform's types are", r.MTART))
	}
	if !materialGroup.MatchString(r.MATKL) {
		reasons = append(reasons, fmt.Sprintf("material group %q is not 1 to 9 capitals, digits, - or _", r.MATKL))
	}
	t, english, ok := pickText(mp.texts[r.MATNR])
	switch {
	case !ok:
		reasons = append(reasons, "it has no description (MAKT)")
	case !english:
		mp.add(SevWarning, "MAKT", r.MATNR, "no description in English; it loads described %q, in language %s", t.name, t.lang)
	}
	m.description = strings.TrimSpace(t.name)
	var unitReasons []string
	m.units, unitReasons = mp.materialUnits(r)
	if reasons = append(reasons, unitReasons...); len(reasons) > 0 {
		mp.add(SevWarning, "MARA", r.MATNR, "held back: %s", strings.Join(reasons, "; "))
		return
	}
	mp.materials = append(mp.materials, m)
}

// materialPlants are the material's plants and their storage locations in
// order, leaving out, with a warning, those SAP flags for deletion.
func (mp *materialPlan) materialPlants(matnr string) []sapMaterialPlant {
	var out []sapMaterialPlant
	plants := slices.SortedFunc(slices.Values(mp.marc[matnr]), func(a, b sap.MARC) int { return cmp.Compare(a.WERKS, b.WERKS) })
	for _, c := range plants {
		if c.LVORM == sap.Deleted {
			mp.add(SevWarning, "MARC", matnr+"/"+c.WERKS, "flagged for deletion at the plant (LVORM); the material is not extended to it")
			continue
		}
		p := sapMaterialPlant{werks: c.WERKS}
		for _, d := range mp.mard[matnr+"/"+c.WERKS] {
			if d.LVORM == sap.Deleted {
				mp.add(SevWarning, "MARD", matnr+"/"+c.WERKS+"/"+d.LGORT, "flagged for deletion at the storage location (LVORM); the material is not kept there")
				continue
			}
			if !slices.Contains(p.locations, d.LGORT) {
				p.locations = append(p.locations, d.LGORT)
			}
		}
		slices.Sort(p.locations)
		out = append(out, p)
	}
	return out
}

// materialUnits are the material's alternative units in code order, and
// the reasons it is held back for its units: a base or alternative unit
// that is no unit of measure of the platform, a unit listed twice, the
// base unit's own row other than 1/1, or a conversion that is not whole
// numbers from 1 to 1,000,000 (ADR-0082 §5: exact, never rounded).
func (mp *materialPlan) materialUnits(r sap.MARA) ([]materials.UnitInput, []string) {
	var reasons []string
	if !mp.units[r.MEINS] {
		reasons = append(reasons, fmt.Sprintf("base unit %q is no unit of measure of the platform", r.MEINS))
	}
	rows := slices.SortedFunc(slices.Values(mp.marm[r.MATNR]), func(a, b sap.MARM) int { return cmp.Compare(a.MEINH, b.MEINH) })
	var out []materials.UnitInput
	for i, u := range rows {
		num, numOK := factor(u.UMREZ)
		den, denOK := factor(u.UMREN)
		switch {
		case i > 0 && rows[i-1].MEINH == u.MEINH:
			reasons = append(reasons, fmt.Sprintf("unit %q is listed twice (MARM)", u.MEINH))
		case u.MEINH == r.MEINS:
			if num != 1 || den != 1 {
				reasons = append(reasons, fmt.Sprintf("the base unit's own conversion is %s/%s, not 1/1", u.UMREZ, u.UMREN))
			}
		case !mp.units[u.MEINH]:
			reasons = append(reasons, fmt.Sprintf("alternative unit %q is no unit of measure of the platform", u.MEINH))
		case !numOK || !denOK:
			reasons = append(reasons, fmt.Sprintf("the conversion of %s, %s/%s base units, is not whole numbers from 1 to 1,000,000", u.MEINH, u.UMREZ, u.UMREN))
		default:
			out = append(out, materials.UnitInput{Code: u.MEINH, Numerator: num, Denominator: den})
		}
	}
	return out, reasons
}

// factor is a numerator or denominator of a conversion (MARM UMREZ,
// UMREN): a whole number from 1 to maxFactor, which an extract may write
// with decimals of zero ("20.000"); ok is false for any other value.
func factor(s string) (int32, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || !r.IsInt() || r.Sign() <= 0 || r.Num().Cmp(big.NewInt(maxFactor)) > 0 {
		return 0, false
	}
	return int32(r.Num().Int64()), true // at most maxFactor
}

// materialMaster checks the plants, storage locations and materials
// (S2o): what refuses the run is an error, what holds a material back a
// warning.
func (q *qualityCheck) materialMaster() {
	plants, issues := plantsOf(q.e)
	q.out = append(q.out, issues...)
	_, issues = materialsOf(q.e, q.t.units, plants)
	q.out = append(q.out, issues...)
}
