// Package modelalias names the model aliases callers ask for (ADR-0036):
// the model gateway routes each to a concrete model under the tenant's
// policy, and an agent's profile names the one it runs on. Shared by
// agents (routing) and identity (profiles), which import no other context.
package modelalias

import "slices"

// Aliases are the platform's model aliases.
var Aliases = []string{"erp-default", "erp-extraction", "erp-analytics", "erp-close"}

// Known reports whether alias is one of them.
func Known(alias string) bool { return slices.Contains(Aliases, alias) }
