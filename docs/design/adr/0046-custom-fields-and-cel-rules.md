# ADR-0046: Custom fields and CEL rules

**Status:** accepted (product owner, 2026-10-19). Amends ADR-0009, which it
moves to accepted.

## Context
ADR-0009 decided that custom fields live in one `extensions jsonb` column
per extensible entity, declared per tenant in configuration. It left open
which entities come first, what a schema may say, how custom fields are
classified for privacy, what changing them costs in approval, how they
reach events and agents, and how tenants express rules on them without
writing Go (07 T58: no tenant Go code in Phase 2). Every ERP customer
needs both (SAP key-user extensibility: custom fields and logic); doing
either loosely lets one tenant's settings slow every tenant's writes, leak
personal data, or change postings without the approval a posting change
needs.

## Decision

### Custom fields
- **Entities.** First the supplier invoice, the purchase order and the
  business partner; the purchase requisition and the legal entity follow
  with the same code. A schema for any other entity is refused until its
  write path exists.
- **Storage.** One `extensions jsonb NOT NULL DEFAULT '{}'` column per
  entity (ADR-0009). No entity-attribute-value tables.
- **Schema.** Per entity, in the configuration document's `extensions`
  section, a restricted subset of JSON Schema that we compile ourselves: a
  flat `type: object` with `additionalProperties: false`, `required` and
  `properties`. No `$ref`, no remote fetch, no `patternProperties`, no
  nesting or arrays; an unknown keyword is refused, and so is an unknown
  key in a row (`additionalProperties: false` is mandatory).
- **Types.** string (at most 255 characters), text (at most 2000), integer,
  decimal (a decimal string, never a JSON number), money (`money.Minor`
  with a currency, the API's Money shape, ADR-0005), date, boolean, enum
  (at most 100 values), and a reference to a business partner or a cost
  object, whose existence is checked on write. No floats anywhere.
- **Limits.** At most 50 fields per entity and 16 KiB of extensions per row.
- **Privacy.** Every field declares `x-pii: none | personal | sensitive`.
  Personal and sensitive values are masked in logs, traces and events,
  and in an agent's context unless the tenant's AI policy allows them
  there; they are never extracted to analytics. Declaring a field `sensitive` is a tier 2 change.
- **Versions.** Each entity's schema carries a version number. A version
  is immutable: a changed schema must carry the base version plus one, an
  unchanged one keeps its number, and an entity's schema is never removed
  (its fields are), so a version number always means the same schema.
- **Tiers.** Adding an optional field, widening a field (a longer maximum,
  more enum values, a wider integer range) and changing titles and
  descriptions are tier 1. Adding a required field, making a field
  required, narrowing or retyping a field, removing a field, and any change
  of `x-pii` are tier 2.
- **Indexes.** Only by a governed platform operation: an expression index,
  built concurrently per partition. Never from a configuration change.
- **Which schema.** A write validates against the schema of the
  configuration version its transaction reads, and records that schema's
  version with the row's events.
- **API.** Resources keep `extensions` free-form in the contract. The
  tenant's schema for an entity is at `GET /v1/schemas/{entity}`, with the
  configuration version as its ETag. A write that does not match the
  schema is answered 422 `request.invalid_extension` with one field error
  per offending key (`extensions.<key>`).
- **Events.** Events of an extensible entity carry its `extensions` and
  the `extensions_schema` version they were written under, with personal
  and sensitive values masked.
- **Not yet.** Custom fields do not join the analytics extraction (T55)
  now; that is a later step, and personal and sensitive fields never will.

### Rules
- **Language.** CEL (cel-go): side-effect free, terminating, with cost
  estimation. Not Go, not JavaScript.
- **Scope in Phase 2.** Validation on write only: a rule that evaluates to
  false refuses the write with its message. No derived defaults, no
  routing.
- **Limits.** A cost limit and a compiled-size limit per rule, a 10 ms
  evaluation timeout through the context, at most 20 rules per entity. No
  `now()`, no randomness, no I/O; the inputs are the row and its
  extensions only.
- **Governance.** Rules are versioned in configuration, compiled when
  proposed (a rule that does not compile is a validation finding), and are
  tier 3 changes, so `:simulate` evidence is mandatory before approval.
- **Audit.** Rule outcomes are audited with `source: rule`, naming the rule
  and its version. A refused write rolls its transaction back, so the
  audit record of a refusal is committed in a transaction of its own.

### Agents
Agents may propose custom fields and rules as change requests. They never
approve or activate them.

### Delivery
In steps: a this ADR; b field schemas in configuration (compiler, tiers,
immutable versions); c the write path for the first entities; d
`GET /v1/schemas/{entity}`; e events; f the CEL engine; g validation rules
on write; h simulation of rules. No new per-pull-request CI job: the schema
compiler's fuzz target joins `make fuzz`.

## Alternatives
- **Full JSON Schema with a library validator:** `$ref`, remote fetches
  and regular-expression keywords make cost and safety per tenant hard to
  bound; a small subset we compile is enough for flat fields.
- **Entity-attribute-value tables, per-tenant columns:** rejected in
  ADR-0009.
- **Rules in Go or a scripting language:** tenant code in the process
  (07 T58 excludes it in Phase 2); CEL is bounded and analyzable.
- **Rules that derive values or route approvals now:** more power than
  Phase 2 needs; validation alone cannot change what is posted.

## Consequences
- Writes of the first entities validate extensions against the active
  schema; a schema change never needs a migration.
- Existing rows are never rewritten by a schema change; a narrowed or
  required field applies from the next write of a row, which is why
  narrowing needs tier 2 approval.
- Masking depends on the schema's classification, so a field declared
  `none` that holds personal data is the tenant's misclassification; the
  tier 2 approval of any `x-pii` change is the control.
