# ADR-0001: Modular monolith with bounded contexts built to split

**Status:** proposed

## Context
The platform will eventually span ~20 business domains and must scale to the
targets in 01-requirements without a re-architecture. The team is small now.
Microservices from day one add network failure modes, distributed
transactions and operational load before there is a need; a classic monolith
becomes impossible to split once modules share tables.

## Decision
One deployable Go service composed of bounded contexts with enforced
boundaries: schema per context, no cross-schema foreign keys or joins,
communication only through published Go interfaces and outbox events, and
ports-and-adapters layering inside each context. Boundaries are enforced in
CI by an import linter (a context may import only another context's `api`
package) and by a database check that no foreign key crosses schemas.

## Alternatives
- **Microservices now:** independent scaling and deploys, but distributed
  transactions for every posting, more infrastructure, slower delivery.
- **Layered monolith (no context boundaries):** fastest initially, but shared
  tables make later extraction a rewrite.

## Consequences
- Cross-context writes can share one transaction while contexts share a
  database; when a context is extracted, those flows become sagas. Domain
  events are designed from the start so the change is local.
- Extraction is a deployment decision, not a code rewrite.
- Discipline is required; the CI checks make violations fail the build.
