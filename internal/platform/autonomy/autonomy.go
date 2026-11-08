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
var Types = []string{"ap_exceptions", "configuration", "migration", "test", "analytics", "reconciliation", "close"}

// MaxLevel is the highest autonomy level a profile may hold (ADR-0036),
// which a waived certification certifies every type for.
const MaxLevel = 3

// KnownType reports whether t is one of them.
func KnownType(t string) bool { return slices.Contains(Types, t) }

// Agent kinds beyond the types, so that a metric's agent_kind is a bounded
// enumeration, never a principal (ADR-0043).
const (
	Untyped  = "untyped" // an agent whose profile names no type it knows
	NotAgent = "none"    // a person or service: not an agent
)

// Kinds are every agent kind: the types, Untyped and NotAgent.
var Kinds = append(slices.Clone(Types), Untyped, NotAgent)

// Kind is the agent kind of an agent of type t: t if it is a type, Untyped
// otherwise (none named, or one since retired).
func Kind(t string) string {
	if KnownType(t) {
		return t
	}
	return Untyped
}

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
