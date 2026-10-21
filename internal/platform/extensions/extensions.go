// Package extensions checks the custom fields a request carries under
// `extensions` (ADR-0009, docs/design/04-api-design.md §4.4): one rule for
// every context that stores them, so a key one resource accepts is not
// refused by another. It also compiles a tenant's custom field schema, the
// restricted JSON Schema subset of ADR-0046, which the configuration
// document carries per entity, and checks the rows of the entities that
// have one against the active schema (Writes); Check remains the rule for
// the entities that have none yet.
package extensions

import (
	"maps"
	"regexp"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Check refuses, on an entity without a custom field schema, custom field keys that are not snake_case starting with a
// letter: 422 request.invalid_extension on the field extensions.<key>. The
// keys are checked in order, so the same input always names the same key.
func Check(ext map[string]any) error {
	for _, k := range slices.Sorted(maps.Keys(ext)) {
		if !keyPattern.MatchString(k) {
			return problem.Unprocessable("request.invalid_extension", "Custom field keys are snake_case.",
				problem.FieldError{Field: "extensions." + k, Code: "invalid_key", Message: "snake_case, starting with a letter"})
		}
	}
	return nil
}
