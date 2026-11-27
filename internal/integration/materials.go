package integration

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/langtag"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The material master's migration (S2o; ADR-0082 As built S2o): SAP's
// plants (T001W) and storage locations (T001L) become tenancy's org units,
// and its materials (MARA with its descriptions of MAKT, the units of
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

// sapLanguages are the language tags (langtag) of SAP's one-character
// language keys (MAKT-SPRAS, table T002, from memory; ADR-0082 As built
// S1a2); a key of two letters is taken as its ISO 639-1 code (EN). Only
// keys known with some confidence are listed (the pilot's E and A among
// them); a text in another key warns and is left out until the specialist
// confirms its tag (K, L, V, M, B and the rest of T002).
var sapLanguages = map[string]string{"E": "en", "D": "de", "F": "fr", "S": "es", "I": "it", "P": "pt", "N": "nl", "A": "ar", "R": "ru", "T": "tr",
	"J": "ja", "1": "zh"}

// undetermined is the tag of a default description in a language key
// that has none (BCP 47's und).
const undetermined = "und"

// languageOf is the tag of SAP's language key, false when it has none.
func languageOf(spras string) (string, bool) {
	if tag, ok := sapLanguages[spras]; ok {
		return tag, true
	}
	if len(spras) != 2 {
		return "", false
	}
	return langtag.Canonical(strings.ToLower(spras))
}

// maxFactor is the largest numerator or denominator of a conversion the
// platform keeps (materials.unit_conversion, ADR-0082 §4).
const maxFactor = 1_000_000

// sapPlant is a plant of the extract (T001W) with its company code, its
// valuation area (BWKEY; the plant's own code when the extract leaves it
// out) and its storage locations (T001L), in code order.
type sapPlant struct {
	werks, name, company, area string
	locations                  []sapLocation
}

// sapLocation is a storage location of a plant (T001L).
type sapLocation struct{ lgort, name string }

// sapMaterial is a material the load creates: its SAP number, its
// platform type, group, base unit and default description with its
// language, its further descriptions by language, its base unit's
// measures, its alternative units with theirs in code order and its
// plants in plant order, each with its storage locations in code order.
type sapMaterial struct {
	number, mtype, group, baseUnit, description, language string
	descriptions                                          []materials.Description
	baseMeasures                                          materials.Measures
	units                                                 []materials.UnitInput
	plants                                                []sapMaterialPlant
}

type sapMaterialPlant struct {
	werks     string
	locations []string
}

// knownUnits reads the platform's units of measure (ref.unit_of_measure)
// with their kinds (mass, volume, length, …), which a material's base and
// alternative units, and its measures' units, must be.
func knownUnits(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	rows, _ := tx.Query(ctx, `SELECT code, dimension FROM ref.unit_of_measure`)
	out := map[string]string{}
	var code, dimension string
	if _, err := pgx.ForEachRow(rows, []any{&code, &dimension}, func() error {
		out[code] = dimension
		return nil
	}); err != nil {
		return nil, fmt.Errorf("integration: read the units of measure: %w", err)
	}
	return out, nil
}

// plantsOf are the extract's plants in code order, each with its storage
// locations, and the errors that refuse them: a key that is no org unit
// code, no name, a row twice and a storage location of no plant. A
// storage location's code may be at two plants of a company code, as
// tenancy keys it by its plant as SAP does.
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
		plants = append(plants, sapPlant{werks: r.WERKS, name: r.NAME1, company: r.BUKRS, area: cmp.Or(r.BWKEY, r.WERKS)})
		byCode[r.WERKS] = nil
	}
	for i := range plants {
		byCode[plants[i].werks] = &plants[i]
	}
	locations := slices.SortedFunc(slices.Values(e.T001L), func(a, b sap.T001L) int { return cmp.Compare(a.WERKS+"/"+a.LGORT, b.WERKS+"/"+b.LGORT) })
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
		}
		orgUnitIssues("T001L", key, r.LGORT, r.LGOBE, "storage location", add)
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
	units     map[string]string // the platform's units of measure, with their kinds
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
func materialsOf(e *sap.Extract, units map[string]string, plants []sapPlant) ([]sapMaterial, []Issue) {
	mp := &materialPlan{units: units, plants: map[string]sapPlant{}, texts: map[string][]sapText{}, marm: map[string][]sap.MARM{},
		marc: map[string][]sap.MARC{}, mard: map[string][]sap.MARD{}}
	for _, p := range plants {
		mp.plants[p.werks] = p
	}
	for _, t := range e.MAKT {
		if slices.ContainsFunc(mp.texts[t.MATNR], func(x sapText) bool { return x.lang == t.SPRAS }) {
			mp.add(SevError, "MAKT", t.MATNR+"/"+t.SPRAS, "the material's description in language %s is listed twice", t.SPRAS)
			continue
		}
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
	mp.sharedGTINs()
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
	if ok {
		m.description = strings.TrimSpace(t.name)
		m.language, m.descriptions = mp.materialDescriptions(r.MATNR, t)
	}
	var unitReasons []string
	m.units, unitReasons = mp.materialUnits(r)
	if reasons = append(reasons, unitReasons...); len(reasons) > 0 {
		mp.add(SevWarning, "MARA", r.MATNR, "held back: %s", strings.Join(reasons, "; "))
		return
	}
	m.baseMeasures = mp.baseMeasures(r)
	mp.materials = append(mp.materials, m)
}

// materialDescriptions are a material's default description's language,
// that of the text it loads with (und, with a warning, for a key with no
// tag), and its other MAKT texts as its further descriptions by language;
// a text in a key with no tag, or in a language a text already has, is
// left out with a warning, and a blank one is left out.
func (mp *materialPlan) materialDescriptions(matnr string, picked sapText) (string, []materials.Description) {
	language, ok := languageOf(picked.lang)
	if !ok {
		mp.add(SevWarning, "MAKT", matnr+"/"+picked.lang, "language key %s has no language tag; the default description loads in %s (undetermined)",
			picked.lang, undetermined)
		language = undetermined
	}
	taken := map[string]bool{language: true}
	var out []materials.Description
	for _, t := range slices.SortedFunc(slices.Values(mp.texts[matnr]), func(a, b sapText) int { return cmp.Compare(a.lang, b.lang) }) {
		text := strings.TrimSpace(t.name)
		if t.lang == picked.lang || text == "" {
			continue
		}
		tag, ok := languageOf(t.lang)
		switch {
		case !ok:
			mp.add(SevWarning, "MAKT", matnr+"/"+t.lang, "language key %s has no language tag; the description %q is left out", t.lang, text)
		case taken[tag]:
			mp.add(SevWarning, "MAKT", matnr+"/"+t.lang, "language key %s is %s, which another description has; the description %q is left out", t.lang,
				tag, text)
		default:
			taken[tag], out = true, append(out, materials.Description{Language: tag, Text: text})
		}
	}
	slices.SortFunc(out, func(a, b materials.Description) int { return cmp.Compare(a.Language, b.Language) })
	return language, out
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
	if mp.units[r.MEINS] == "" {
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
		case mp.units[u.MEINH] == "":
			reasons = append(reasons, fmt.Sprintf("alternative unit %q is no unit of measure of the platform", u.MEINH))
		case !numOK || !denOK:
			reasons = append(reasons, fmt.Sprintf("the conversion of %s, %s/%s base units, is not whole numbers from 1 to 1,000,000", u.MEINH, u.UMREZ, u.UMREN))
		default:
			out = append(out, materials.UnitInput{Code: u.MEINH, Numerator: num, Denominator: den, Measures: mp.measures("MARM", r.MATNR, u.MEINH, unitFields{
				gross: u.BRGEW, weightUnit: u.GEWEI, volume: u.VOLUM, volumeUnit: u.VOLEH, length: u.LAENG, width: u.BREIT, height: u.HOEHE,
				dimensionUnit: u.MEABM, ean: u.EAN11})})
		}
	}
	return out, reasons
}

// wholeNumber is a numerator or denominator as an extract writes one:
// digits, with decimals of zero or none ("20", "20.000").
var wholeNumber = regexp.MustCompile(`^[0-9]+(\.0+)?$`)

// factor is a numerator or denominator of a conversion (MARM UMREZ,
// UMREN): a whole number from 1 to maxFactor (wholeNumber); ok is false
// for any other value, a fraction or an exponent among them.
func factor(s string) (int32, bool) {
	s = strings.TrimSpace(s)
	if !wholeNumber.MatchString(s) {
		return 0, false
	}
	whole, _, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(whole, 10, 32)
	if err != nil || n < 1 || n > maxFactor {
		return 0, false
	}
	return int32(n), true
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
