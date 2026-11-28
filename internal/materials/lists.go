package materials

import (
	"context"
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The material types and groups a material may take are the active
// configuration's (ADR-0082 As built S1a4; SAP T134 and T023, from
// memory). A list the configuration leaves empty checks nothing, so a
// tenant that has configured none keeps every platform type and any
// well-formed group. A material is checked when it is created and when a
// change sets its group; a material created before a list, and a change
// requested before it, keep what they name, as SAP keeps a material's
// group when its table changes (from memory).

// lists is the active configuration's material types and groups.
func (e *Endpoints) lists(ctx context.Context) (config.Materials, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return config.Materials{}, fmt.Errorf("materials: the configured material types and groups: %w", err)
	}
	return doc.Materials, nil
}

// checkType refuses a type the configuration does not allow (422
// materials.unknown_material_type naming material_type), saying which
// types it allows.
func checkType(lists config.Materials, code string) error {
	if lists.AllowsType(code) {
		return nil
	}
	allowed := strings.Join(lists.TypeCodes(), ", ")
	if len(lists.MaterialTypes) == 0 {
		return problem.Invalid("materials.unknown_material_type", "material_type", fmt.Sprintf("%q is not a material type; the types are %s.", code, allowed))
	}
	return problem.Invalid("materials.unknown_material_type", "material_type",
		fmt.Sprintf("%q is not one of the configuration's material types (materials.material_types): %s.", code, allowed))
}

// checkGroup refuses a group the configuration does not list (422
// materials.unknown_material_group naming material_group).
func checkGroup(lists config.Materials, code string) error {
	if lists.AllowsGroup(code) {
		return nil
	}
	return problem.Invalid("materials.unknown_material_group", "material_group",
		"Material group "+code+" is not one of the configuration's (materials.material_groups).")
}
