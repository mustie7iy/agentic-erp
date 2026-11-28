package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Materials is the tenant's material master configuration (ADR-0082 As
// built S1a4): the material types and the material groups its materials
// may have (SAP T134 and T023, from memory). A list left empty checks
// nothing, so a tenant that has configured none keeps every one of the
// platform's types and any well-formed group as before; once a list has an
// entry, a material created or changed in it takes only what it lists.
type Materials struct {
	MaterialTypes  []MaterialType  `yaml:"material_types,omitempty" json:"material_types,omitempty"`
	MaterialGroups []MaterialGroup `yaml:"material_groups,omitempty" json:"material_groups,omitempty"`
}

// MaterialType is a material type the tenant uses, one of the platform's
// (MaterialTypeCodes), with the tenant's name for it.
type MaterialType struct {
	Code string `yaml:"code" json:"code"`
	Name string `yaml:"name" json:"name"`
}

// MaterialGroup is a material group, a code grouping like materials, with
// its name.
type MaterialGroup struct {
	Code string `yaml:"code" json:"code"`
	Name string `yaml:"name" json:"name"`
}

// materialTypeCodes are the platform's material types (ADR-0082 §4: SAP's
// standard types ROH, HALB, FERT, HAWA, NLAG and DIEN, from memory), whose
// behaviour the platform knows; a configured type is one of them.
var materialTypeCodes = []string{"raw_material", "semi_finished", "finished_good", "trading_good", "non_stock", "service"}

// MaterialTypeCodes are the platform's material types, in the contract's
// order. The answer is a copy.
func MaterialTypeCodes() []string { return slices.Clone(materialTypeCodes) }

// IsMaterialType reports whether code is one of the platform's material
// types.
func IsMaterialType(code string) bool { return slices.Contains(materialTypeCodes, code) }

// materialGroupRe is a material group's form, materials.material's
// (ADR-0082 §4: SAP MATKL's length, from memory).
var materialGroupRe = regexp.MustCompile(`^[A-Z0-9_-]{1,9}$`)

// IsMaterialGroupCode reports whether code has a material group's form.
func IsMaterialGroupCode(code string) bool { return materialGroupRe.MatchString(code) }

// MaxMaterialGroups bounds the material groups: the document is read
// whole on each material request.
const MaxMaterialGroups = 1000

// MaterialType returns the configured material type of the code, if any.
func (m Materials) MaterialType(code string) (MaterialType, bool) {
	i := slices.IndexFunc(m.MaterialTypes, func(t MaterialType) bool { return t.Code == code })
	if i < 0 {
		return MaterialType{}, false
	}
	return m.MaterialTypes[i], true
}

// MaterialGroup returns the configured material group of the code, if
// any.
func (m Materials) MaterialGroup(code string) (MaterialGroup, bool) {
	i := slices.IndexFunc(m.MaterialGroups, func(g MaterialGroup) bool { return g.Code == code })
	if i < 0 {
		return MaterialGroup{}, false
	}
	return m.MaterialGroups[i], true
}

// AllowsType reports whether a material may take the type: any of the
// platform's while no type is configured, else a configured one.
func (m Materials) AllowsType(code string) bool {
	if len(m.MaterialTypes) == 0 {
		return IsMaterialType(code)
	}
	_, ok := m.MaterialType(code)
	return ok
}

// AllowsGroup reports whether a material may take the group: any while no
// group is configured (its form is the contract's and the database's),
// else a configured one.
func (m Materials) AllowsGroup(code string) bool {
	if len(m.MaterialGroups) == 0 {
		return true
	}
	_, ok := m.MaterialGroup(code)
	return ok
}

// TypeCodes are the codes of the types a material may take, in the
// platform's order when none is configured, else in the configuration's.
func (m Materials) TypeCodes() []string {
	if len(m.MaterialTypes) == 0 {
		return MaterialTypeCodes()
	}
	out := make([]string, len(m.MaterialTypes))
	for i, t := range m.MaterialTypes {
		out[i] = t.Code
	}
	return out
}

// materials checks the material types (each one of the platform's, once,
// with a name) and the material groups (each a code of materials'
// form, once, with a name; at most MaxMaterialGroups).
func (v *validator) materials(c *Config) {
	seen := map[string]bool{}
	for i, t := range c.Materials.MaterialTypes {
		p := fmt.Sprintf("materials.material_types[%s]", t.Code)
		switch {
		case !IsMaterialType(t.Code):
			p = fmt.Sprintf("materials.material_types[%d]", i)
			v.err(p+".code", "must be one of %s", strings.Join(materialTypeCodes, ", "))
		case seen[t.Code]:
			v.err(p, "duplicate material type code")
		}
		v.name(p, t.Name)
		seen[t.Code] = true
	}
	if len(c.Materials.MaterialGroups) > MaxMaterialGroups {
		v.err("materials.material_groups", "at most %d material groups", MaxMaterialGroups)
	}
	seen = map[string]bool{}
	for i, g := range c.Materials.MaterialGroups {
		p := fmt.Sprintf("materials.material_groups[%s]", g.Code)
		switch {
		case !IsMaterialGroupCode(g.Code):
			p = fmt.Sprintf("materials.material_groups[%d]", i)
			v.err(p+".code", "must be 1-9 capital letters, digits, - or _")
		case seen[g.Code]:
			v.err(p, "duplicate material group code")
		}
		v.name(p, g.Name)
		seen[g.Code] = true
	}
}

// MaterialsTransitions checks the material types and groups against the
// base document (nil for the first version): neither is removed once
// activated, since materials name them and a change of a material keeps
// what it does not change, as a price group is never removed
// (PricingTransitions). Their names may change. So a list, once it has an
// entry, never becomes empty again, which would stop the check.
func MaterialsTransitions(base, proposed *Config) []Finding {
	if base == nil {
		return nil
	}
	var out []Finding
	for _, was := range base.Materials.MaterialTypes {
		if _, ok := proposed.Materials.MaterialType(was.Code); !ok {
			out = append(out, Finding{Severity: SevError, Path: "materials.material_types[" + was.Code + "]",
				Message: "a material type cannot be removed once activated"})
		}
	}
	for _, was := range base.Materials.MaterialGroups {
		if _, ok := proposed.Materials.MaterialGroup(was.Code); !ok {
			out = append(out, Finding{Severity: SevError, Path: "materials.material_groups[" + was.Code + "]",
				Message: "a material group cannot be removed once activated"})
		}
	}
	return out
}
