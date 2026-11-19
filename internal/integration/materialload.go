package integration

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Materials creates and extends the materials a run loads and reads them
// back for the reconciliation (materials.Migration, ADR-0082 As built
// S2o).
type Materials interface {
	Number(ctx context.Context, legalEntity uuid.UUID) (string, error)
	CreateMaterial(ctx context.Context, in materials.MigratedMaterial) (uuid.UUID, error)
	LockMaterials(ctx context.Context, ids []uuid.UUID) error
	ExtendMaterial(ctx context.Context, id uuid.UUID, plants []materials.PlantInput, reason string) (bool, error)
	Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]materials.Material, error)
}

// OrgUnits finds and creates the plants and storage locations a run loads
// and reads them back (tenancy.OrgUnitMigration).
type OrgUnits interface {
	OrgUnitByCode(ctx context.Context, legalEntity uuid.UUID, kind, code string) (tenancy.Unit, bool, error)
	CreateActiveOrgUnit(ctx context.Context, in tenancy.NewOrgUnit) (tenancy.Unit, error)
	OrgUnits(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Unit, error)
}

// Org unit kinds of plants and storage locations (tenancy.OrgUnitKinds).
const (
	kindPlant           = "plant"
	kindStorageLocation = "storage_location"
)

// materialMaster loads the plants with their storage locations, then the
// materials, each kept by its SAP key: a plant by T001W/WERKS, a storage
// location by T001L/WERKS/LGORT, a material by MARA/MATNR. What an earlier
// run loaded stays as it is; a material it loaded is only extended to the
// plants and storage locations it is not yet at. The materials to extend
// are locked first, then the new materials' numbers drawn, before any
// material change is written (materials.Migration's lock order).
func (l *loader) materialMaster(ctx context.Context) error {
	plants, _ := plantsOf(l.ext)                     // the quality gate refused their errors
	mats, _ := materialsOf(l.ext, l.t.units, plants) // and reported the materials held back
	var loaded []uuid.UUID
	for _, m := range mats {
		if id, ok := l.refs["MARA/"+m.number]; ok {
			loaded = append(loaded, id)
		}
	}
	if len(loaded) > 0 {
		if err := l.e.Materials.LockMaterials(ctx, loaded); err != nil {
			return err
		}
	}
	company := map[string]string{}
	for _, p := range plants {
		company[p.werks] = p.company
	}
	numbers, err := l.materialNumbers(ctx, mats, company)
	if err != nil {
		return err
	}
	for _, p := range plants {
		if err := l.plant(ctx, p); err != nil {
			return err
		}
	}
	for _, m := range mats {
		if err := l.material(ctx, m, company, numbers); err != nil {
			return err
		}
	}
	return nil
}

// materialNumbers draws the number of each material the run creates from
// the material range of its first plant's company; a material whose
// number is refused is reported and created by no one.
func (l *loader) materialNumbers(ctx context.Context, mats []sapMaterial, company map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range mats {
		if _, ok := l.refs["MARA/"+m.number]; ok {
			continue
		}
		var n string
		if _, err := l.step(ctx, "MARA", m.number, func() error {
			var err error
			n, err = l.e.Materials.Number(ctx, l.t.entities[company[m.plants[0].werks]].ID)
			return err
		}); err != nil {
			return nil, err
		}
		if n != "" {
			out[m.number] = n
		}
	}
	return out, nil
}

// plant loads a plant and its storage locations: found by its code in its
// company, or created active.
func (l *loader) plant(ctx context.Context, p sapPlant) error {
	le := l.t.entities[p.company].ID
	id, ok := l.refs["T001W/"+p.werks]
	if !ok {
		var err error
		if id, err = l.orgUnit(ctx, "T001W", p.werks, tenancy.NewOrgUnit{LegalEntityID: le, Kind: kindPlant, Code: p.werks, Name: p.name}); err != nil {
			return err
		}
		if id == uuid.Nil {
			return nil // refused: its storage locations wait for it
		}
		l.counts.Plants++
	}
	for _, s := range p.locations {
		key := p.werks + "/" + s.lgort
		if _, ok := l.refs["T001L/"+key]; ok {
			continue
		}
		parent := id
		created, err := l.orgUnit(ctx, "T001L", key, tenancy.NewOrgUnit{LegalEntityID: le, Kind: kindStorageLocation, Code: s.lgort, Name: s.name, ParentID: &parent})
		if err != nil {
			return err
		}
		if created != uuid.Nil {
			l.counts.StorageLocations++
		}
	}
	return nil
}

// orgUnit records the org unit of the company with the kind and code as
// the SAP key's: an active one the company has, a storage location under
// the plant given, or else one created active. One the company has that
// is not active, or a storage location under another plant, refuses the
// key, as does a creation tenancy refuses; the id is uuid.Nil then.
func (l *loader) orgUnit(ctx context.Context, table, key string, in tenancy.NewOrgUnit) (uuid.UUID, error) {
	u, found, err := l.e.OrgUnits.OrgUnitByCode(ctx, in.LegalEntityID, in.Kind, in.Code)
	switch {
	case err != nil:
		return uuid.Nil, err
	case found && u.Status != tenancy.Active:
		l.issue(table, key, "the company's "+kindName(in.Kind)+" "+in.Code+" is "+u.Status+"; activate it, or give SAP's another code")
		return uuid.Nil, nil
	case found && in.ParentID != nil && (u.ParentID == nil || *u.ParentID != *in.ParentID):
		l.issue(table, key, "the company's storage location "+in.Code+" is under another plant")
		return uuid.Nil, nil
	}
	id := u.ID
	done, err := l.step(ctx, table, key, func() error {
		if !found {
			created, err := l.e.OrgUnits.CreateActiveOrgUnit(ctx, in)
			if err != nil {
				return err
			}
			id = created.ID
		}
		return l.addRef(ctx, "tenancy.org_unit", id, table, key)
	})
	if !done {
		return uuid.Nil, err
	}
	return id, nil
}

func kindName(kind string) string {
	if kind == kindPlant {
		return "plant"
	}
	return "storage location"
}

// material creates a material, numbered from the material range of its
// first plant's company, or extends the one an earlier run created.
func (l *loader) material(ctx context.Context, m sapMaterial, company, numbers map[string]string) error {
	plants := make([]materials.PlantInput, 0, len(m.plants))
	for _, p := range m.plants {
		in := materials.PlantInput{PlantID: l.refs["T001W/"+p.werks]}
		if in.PlantID == uuid.Nil {
			l.issue("MARC", m.number+"/"+p.werks, "plant "+p.werks+" was not loaded")
			return nil
		}
		for _, s := range p.locations {
			id, ok := l.refs["T001L/"+p.werks+"/"+s]
			if !ok {
				l.issue("MARD", m.number+"/"+p.werks+"/"+s, "storage location "+s+" of plant "+p.werks+" was not loaded")
				return nil
			}
			in.StorageLocations = append(in.StorageLocations, id)
		}
		plants = append(plants, in)
	}
	reason := "Migration run " + l.run.ID.String() + ": SAP material " + m.number
	if id, ok := l.refs["MARA/"+m.number]; ok {
		var extended bool
		done, err := l.step(ctx, "MARC", m.number, func() error {
			var err error
			extended, err = l.e.Materials.ExtendMaterial(ctx, id, plants, reason)
			return err
		})
		if done && extended {
			l.counts.MaterialsExtended++
		}
		return err
	}
	number, ok := numbers[m.number]
	if !ok {
		return nil // its number was refused (materialNumbers)
	}
	in := materials.MigratedMaterial{LegalEntityID: l.t.entities[company[m.plants[0].werks]].ID, Number: number, Description: m.description, BaseUnit: m.baseUnit,
		Type: m.mtype, Group: m.group, Plants: plants, Units: m.units, Reason: reason}
	done, err := l.step(ctx, "MARA", m.number, func() error {
		id, err := l.e.Materials.CreateMaterial(ctx, in)
		if err != nil {
			return err
		}
		return l.addRef(ctx, "materials.material", id, "MARA", m.number)
	})
	if done {
		l.counts.Materials++
	}
	return err
}
