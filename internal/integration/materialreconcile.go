package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// materialStates are the SAP side of the material master, planned once
// from the extract as the load plans it (plantsOf, materialsOf), and the
// platform side runs loaded, read back once: the org units and materials
// by their SAP keys, and every org unit those name by id.
type materialStates struct {
	plants    []sapPlant
	materials []sapMaterial
	unitRefs  map[string]uuid.UUID // T001W WERKS, T001L WERKS/LGORT -> org unit
	loaded    map[string]materials.Material
	units     map[uuid.UUID]tenancy.Unit
}

// readMaterials plans the SAP side, refusing what the quality gate
// refuses (a reconciliation run extracts again without it), and reads
// what runs of the system loaded, once per reconciliation (readStock,
// before any company's checks).
func (rc *reconciler) readMaterials(ctx context.Context) (*materialStates, error) {
	st := &materialStates{unitRefs: map[string]uuid.UUID{}, loaded: map[string]materials.Material{}}
	var issues, more []Issue
	st.plants, issues = plantsOf(rc.ext)
	st.materials, more = materialsOf(rc.ext, rc.t.units, st.plants)
	for _, i := range append(issues, more...) {
		if i.Severity == SevError {
			rc.rec.Issues = append(rc.rec.Issues, i)
		}
	}
	var unitIDs, materialIDs []uuid.UUID
	numberOf := map[uuid.UUID]string{}
	for _, ref := range sortedKeys(rc.r) {
		object, key, _ := strings.Cut(ref, "/")
		switch object {
		case "T001W", "T001L":
			st.unitRefs[key], unitIDs = rc.r[ref], append(unitIDs, rc.r[ref])
		case "MARA":
			numberOf[rc.r[ref]], materialIDs = key, append(materialIDs, rc.r[ref])
		}
	}
	ms, err := rc.e.Materials.Materials(ctx, materialIDs)
	if err != nil {
		return nil, err
	}
	for id, m := range ms {
		st.loaded[numberOf[id]] = m
		for _, p := range m.Plants {
			unitIDs = append(append(unitIDs, p.PlantID), p.StorageLocations...)
		}
	}
	if st.units, err = rc.e.OrgUnits.OrgUnits(ctx, unitIDs); err != nil {
		return nil, err
	}
	rc.materials = st
	return st, nil
}

// materialMaster compares the company's plants, storage locations and
// materials (S2o) where either side has any: SAP's of the company code
// against those runs loaded in the legal entity, by SAP key. A material
// compares by its type, group, base unit, status, descriptions, plants
// and storage locations in the company, and alternative units; SAP's
// held back or flagged for deletion are left out (materialsOf).
func (rc *reconciler) materialMaster(s companyScope) {
	st := rc.materials
	sapPlants, sapLocations := map[string]string{}, map[string]string{}
	company := map[string]bool{}
	for _, p := range st.plants {
		if p.company != s.cc {
			continue
		}
		sapPlants[p.werks], company[p.werks] = p.werks, true
		for _, l := range p.locations {
			sapLocations[p.werks+"/"+l.lgort] = p.werks + "/" + l.lgort
		}
	}
	platformPlants, platformLocations := map[string]string{}, map[string]string{}
	for key, id := range st.unitRefs {
		u, ok := st.units[id]
		if !ok || u.LegalEntityID != s.le {
			continue
		}
		if u.Kind == kindPlant {
			platformPlants[key] = st.unitText(u)
		} else {
			platformLocations[key] = st.unitText(u)
		}
	}
	rc.compareKeyed(s.cc, "Plants", "plants", "plant ", sapPlants, platformPlants)
	rc.compareKeyed(s.cc, "Storage locations", "storage locations", "storage location ", sapLocations, platformLocations)
	sapMaterials, platformMaterials := map[string]string{}, map[string]string{}
	for _, m := range st.materials {
		if text, ok := sapMaterialText(m, company); ok {
			sapMaterials[m.number] = text
		}
	}
	for number, m := range st.loaded {
		if text, ok := st.materialText(m, s.le); ok {
			platformMaterials[number] = text
		}
	}
	rc.compareKeyed(s.cc, "Materials", "materials", "material ", sapMaterials, platformMaterials)
}

// unitText is an org unit as a difference shows it: its code after its
// parent's (a storage location's plant), and its status when it is not
// active.
func (st *materialStates) unitText(u tenancy.Unit) string {
	text := u.Code
	if u.ParentID != nil {
		text = st.units[*u.ParentID].Code + "/" + text
	}
	if u.Status != tenancy.Active {
		text += " (" + u.Status + ")"
	}
	return text
}

// materialPlantText is a material's plant with its storage locations.
type materialPlantText struct {
	code      string
	locations []string
}

// materialText is a material as a difference shows it: its type, group,
// base unit, status and descriptions (the default first, each with its
// language), its plants, each with its storage locations, and its
// alternative units.
func materialText(mtype, group, base, status string, descriptions []materials.Description, plants []materialPlantText, units []materials.UnitInput) string {
	texts := make([]string, len(descriptions))
	for i, d := range descriptions {
		texts[i] = fmt.Sprintf("%s %q", d.Language, d.Text)
	}
	parts := []string{fmt.Sprintf("%s, group %s, base unit %s, %s: %s", mtype, group, base, status, strings.Join(texts, ", "))}
	slices.SortFunc(plants, func(a, b materialPlantText) int { return strings.Compare(a.code, b.code) })
	for _, p := range plants {
		slices.Sort(p.locations)
		parts = append(parts, "plant "+p.code+" ("+strings.Join(p.locations, ", ")+")")
	}
	for _, u := range units {
		parts = append(parts, fmt.Sprintf("unit %s %d/%d", u.Code, u.Numerator, u.Denominator))
	}
	return strings.Join(parts, "; ")
}

// sapMaterialText is SAP's material with its plants of the company, ok
// false when it has none there.
func sapMaterialText(m sapMaterial, company map[string]bool) (string, bool) {
	var plants []materialPlantText
	for _, p := range m.plants {
		if company[p.werks] {
			plants = append(plants, materialPlantText{code: p.werks, locations: slices.Clone(p.locations)})
		}
	}
	descriptions := append([]materials.Description{{Language: m.language, Text: m.description}}, m.descriptions...)
	return materialText(m.mtype, m.group, m.baseUnit, materials.Active, descriptions, plants, m.units), len(plants) > 0
}

// materialText is the platform's material with its plants of the legal
// entity, ok false when it has none there.
func (st *materialStates) materialText(m materials.Material, le uuid.UUID) (string, bool) {
	var plants []materialPlantText
	for _, p := range m.Plants {
		if p.LegalEntityID != le {
			continue
		}
		pt := materialPlantText{code: st.units[p.PlantID].Code}
		for _, s := range p.StorageLocations {
			pt.locations = append(pt.locations, st.units[s].Code)
		}
		plants = append(plants, pt)
	}
	units := make([]materials.UnitInput, len(m.Units))
	for i, u := range m.Units {
		units[i] = materials.UnitInput{Code: u.Code, Numerator: u.Numerator, Denominator: u.Denominator}
	}
	return materialText(m.Type, m.Group, m.BaseUnit, m.Status, m.AllDescriptions(), plants, units), len(plants) > 0
}
