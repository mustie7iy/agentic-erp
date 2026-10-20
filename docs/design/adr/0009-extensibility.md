# ADR-0009: Governed custom fields in jsonb

**Status:** accepted (product owner, 2026-10-19), amended by ADR-0046,
which decides the entities, the schema subset, types, limits, privacy,
tiers, events and CEL rules.

## Context
Every ERP customer adds fields (SAP append structures, CI_ includes). Schema
changes per tenant are impossible in a pooled database.

## Decision
- Extensible entities have `extensions jsonb NOT NULL DEFAULT '{}'`.
- Allowed fields are declared per tenant in configuration as a JSON Schema per
  entity type (type, length, required, allowed values), versioned and approved
  like any configuration change. The application validates writes against it.
- Fields that need indexing get an expression index created by a governed
  platform operation; fields promoted to the core model become real columns in
  a regular migration.

## Alternatives
- **Entity-attribute-value tables:** flexible, slow to query, no types.
- **Per-tenant columns:** impossible with pooled tenancy.

## Consequences
- Custom fields appear in the API under `extensions` with documented schema
  per tenant (exposed at `/v1/schemas/{entity}`).
