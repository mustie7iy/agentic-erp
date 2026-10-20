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
  object, whose existence is checked on write. No floats anywhere. Until
  cost objects exist (T56) the schema compiler refuses a cost object field
  (coordinator, 2026-10-20); T56 lifts the refusal.
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
  version with the row (`extensions_schema`) and with the row's events.
  Every create and update of the row checks its extensions as the write
  leaves them; a change request is checked when it is requested, and the
  version it was checked against is applied with it. A tenant without a
  schema for the entity has no custom fields: every key is unknown. A
  captured supplier invoice carries no custom fields, so a field the schema
  requires holds it (naming the field) instead of refusing the document;
  its next update, and its posting, check its extensions against the
  current schema, so they require the field. A migration load is not a
  capture: it refuses an invoice missing a required field, row by row
  (coordinator, 2026-10-20).
- **API.** Resources keep `extensions` free-form in the contract. The
  tenant's schema for an entity is at `GET /v1/schemas/{entity}`, with the
  configuration version as its ETag: `If-None-Match` answers 304 until a
  new version is activated, and an entity without a schema is 404. Whoever
  may read the entity may read its schema, so the operation names a
  permission per entity (`x-entity-permissions`, in place of
  `x-permission`) and the authorizer picks the one of the requested entity;
  an entity it does not list is 404. A GET, it is open to every autonomy
  level. A write that does not match the
  schema is answered 422 `request.invalid_extension` with one field error
  per offending key (`extensions.<key>`).
- **Events.** Events of an extensible entity carry its `extensions` and
  the `extensions_schema` version they were written under, with personal
  and sensitive values masked. They are BusinessPartnerCreated and
  BusinessPartnerUpdated, PurchaseOrderReleased and PurchaseOrderChanged,
  SupplierInvoiceCaptured and SupplierInvoicePosted: the events that carry
  the entity's facts; the others name it by id. Masking reads the active
  schema and shows only fields it declares `x-pii: none`; every other
  value, a key the schema does not declare included (written before it, or
  under an older version), is the string `[MASKED]`, so a value is never
  shown unless the schema classifies it as public. Key names are not
  masked: they are schema identifiers, never personal data. Masking follows
  the active classification, so after an approved reclassification to
  `none` earlier values of the field appear in later events; that is
  intended, and is why any `x-pii` change is tier 2. The fields are optional
  in the contract, an additive change. Webhooks (T50) deliver these events
  and so inherit the masking; they must never read the row's extensions
  in its place. Logs and traces carry no event payloads or extensions, and
  a refusal's messages repeat no value.
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
- **Engine (T58f).** `internal/platform/rules` on cel-go (`cel.dev/cel-go`,
  pinned). Rules sit in the configuration document under
  `rules.<entity>.<name>` with an `expression`, a `message` and unit
  `cases` (an input and `valid: true|false`; at least one of each, at most
  20), run whenever the rule is compiled. The inputs are `row.<field>`, the
  header fields of the entity's create request, and `ext.<key>`, the custom
  fields of the schema in the same document; a field that may be absent is
  a CEL optional (`ext.x.orValue("")`), dates are `YYYY-MM-DD` strings and
  money is `{amount, currency}` of strings. The expression must be boolean;
  any other type (a derived value, a route) is refused. The standard
  library is used without its time functions (`timestamp`, `duration` and
  their accessors), with optional types and nothing else. Limits: 1000
  code points and nesting 32 per expression, a cost of 10 000 per
  evaluation, 10 ms through the context, checked on every comprehension
  step. An input of the wrong type, a failed evaluation or an exceeded
  limit is an error, never a pass.
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

## Release note
Rows written before T58c may hold free-form `extensions` keys. A tenant
without a schema for the entity has no custom fields, so every such key is
unknown: the row's next update is refused (422 `request.invalid_extension`)
until the update removes the keys, or the tenant declares them in a schema
first (coordinator, 2026-10-20).

## Consequences
- Writes of the first entities validate extensions against the active
  schema; a schema change never needs a migration.
- Existing rows are never rewritten by a schema change; a narrowed or
  required field applies from the next write of a row, which is why
  narrowing needs tier 2 approval.
- Masking depends on the schema's classification, so a field declared
  `none` that holds personal data is the tenant's misclassification; the
  tier 2 approval of any `x-pii` change is the control.
