// Package autonomy names the agent types whose evaluations certify an
// autonomy level (ADR-0039) and what the agents context needs to know of
// identity's agents to keep their routes certified. Shared by agents
// (certifications, routing) and identity (profiles), which import no other
// context.
package autonomy

import (
	"slices"

	"github.com/google/uuid"
)

// Types are the agent types an agent's profile may name, each with its
// evaluation suite under evals/<type>/. An open enumeration: clients
// accept values added later.
var Types = []string{"ap_exceptions", "configuration", "migration", "test", "analytics", "reconciliation"}

// MaxLevel is the highest autonomy level a profile may hold (ADR-0036),
// which a waived certification certifies every type for.
const MaxLevel = 3

// KnownType reports whether t is one of them.
func KnownType(t string) bool { return slices.Contains(Types, t) }

// Agent is an agent of the tenant above level 0: what its type is
// certified for on the model its alias routes to bounds its level. Type is
// empty for an agent whose administrators named none (one from before
// certification, ADR-0039).
type Agent struct {
	ID    uuid.UUID
	Type  string
	Alias string
	Level int16
}
