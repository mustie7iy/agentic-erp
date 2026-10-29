# ADR-0013: Resource-oriented REST with OpenAPI 3.1 as the source of truth

**Status:** proposed

## Context
The platform is API-first. Consumers include a web UI, agents (via tool
schemas and MCP), partner systems and the CLI. The contract must exist and be
reviewed before implementation.

## Decision
- Public API: resource-oriented JSON over HTTPS, versioned by major version in
  the path (`/v1`). Standard methods (list, get, create, update) plus custom
  methods for state transitions (`POST /v1/purchase-orders/{id}:release`),
  following Google AIP conventions.
- `api/openapi.yaml` (OpenAPI 3.1) is the source of truth. Server interfaces
  and clients are generated from it; handlers implement generated interfaces,
  so code cannot drift from the contract.
- Contract changes go through review; CI lints the spec and fails on breaking
  changes against `main` (oasdiff).
- Domain events are specified in `api/asyncapi.yaml`.
- Internal service-to-service calls (after extraction) use the same contracts.

## Alternatives
- **GraphQL:** flexible reads, but harder authorization per field, caching and
  rate limiting; may be added later as a read facade.
- **gRPC public API:** efficient, but weaker for browsers and partners;
  possible internally later.

## Consequences
- Rules in 04-api-design.md apply to every endpoint.

## As built: a migration run's report has a schema (review:batch23)

`MigrationRun.report` was an object with `additionalProperties: true`, so
the keys a run writes (the loaded counts such as `gr_ir_positions` and
`gr_ir_top_ups`, the reconciliation's checks and differences) were known only
from docs/migration.md. It is now `MigrationRunReport`, with
`MigrationExtractCounts`, `MigrationLoadedCounts`, `MigrationIssue`,
`MigrationCheck` and `MigrationDiff`, each key described. There is no ADR of
its own for migration runs, so the choices, each settled by an existing
rule, are recorded here:

- **Open or closed.** The schema leaves `additionalProperties` unset, as
  every other response schema of the contract does; `false` is used on
  request bodies, where an unknown field is refused (04 §4.6). A key a later
  step adds is a new optional property within `/v1` (04 §4.10). oasdiff
  finds no breaking change: every key is an optional property added to a
  response.
- **What is present when.** As the run writes it: `extract` and `quality`
  always (required); `load` and `loaded` once the load ran; `checks`,
  `diffs` and `passed` once the run is reconciled, with `missing_documents`
  and `issues` only when there are some. A loaded count is absent when the
  run created none of its kind, as before, so a consumer that tests for a
  key keeps working. The one change on the wire: `checks` and `diffs` of a
  reconciled run with none are `[]` where they were `null`.
- **How the code is held to it.** The report is a typed struct
  (`integration.Report`), stored as JSONB and decoded leniently with
  encoding/json, so a report written before a key existed still reads;
  `reportToAPI` converts it to the generated types, so the contract cannot
  be answered with an undeclared key. `TestReportMatchesContract` sets
  every field of a report by reflection (a field added later included),
  encodes it both ways and requires the same document with every declared
  field set; `TestEveryReportFieldIsWritten` requires the run's code to
  write every field of the report's types (a keyed literal, an assignment,
  an increment), so a declared key nothing writes fails. Generating the stored form from
  the ogen types was not chosen: their decoder refuses the `null` lists
  stored reports hold and any report missing a key a later contract
  requires.

## As built: the run's event and its report keys (review:batch24)

- **The event's summary has a schema.** `MigrationRunCompletedV1.summary`
  was a free-form object whose example showed figures the run never sent.
  It declares what the run publishes: `passed` (boolean, null until the run
  is reconciled) and `loaded` (`MigrationLoadedCounts`, null until the load
  ran), both always present, as `summaryOf` writes them. AsyncAPI shares a
  schema only within its own `components.schemas` (no `$ref` reaches
  openapi.yaml), so `MigrationLoadedCounts` is declared there with the same
  keys as the API's; `TestSummaryMatchesEventContract` holds the code's
  keys and the event contract's to each other, and the API's are held to
  the code by `TestReportMatchesContract`. The change is additive
  (eventcompat finds no breaking change: properties added, none required,
  the type unchanged), so the event stays v1. `required` was not added:
  newly required fields break the version (ADR-0041), and both keys are
  always sent anyway.
- **Published events are checked at every depth.** `apitest.Published`
  checks a payload's top-level fields; `apitest.Conforms`, added here,
  walks the payload schema at every depth (the keywords it reads: types,
  required and undeclared fields, integer bounds, patterns, `oneOf`; not
  `enum`, `const`, `format`, `minItems`, `allOf` or `anyOf`), and the
  integration tests run it on every `MigrationRunCompleted` they publish,
  a summary of each form included.
  Published itself was left as it is, so no other context's event tests
  change in this step.
- **Report keys name the reference the reader has.** A refused posting
  block is reported under `LFA1` by the vendor number, as every other
  supplier finding is, not by the partner the load created. A
  reconciliation difference of a platform supplier that is no migrated
  vendor (SAP has no number for it) is keyed `partner <id> (<name>)`, the
  name read through partner's `Suppliers` port the reconciliation already
  uses; a failed read fails the reconciliation rather than leaving the name
  out. The keys stay free-form text, as `MigrationIssue.key` and
  `MigrationDiff.key` say, so no field was added.
- **The extract's counts are named in snake_case (batch24 fix forward).**
  `MigrationExtractCounts` first named its properties by SAP's table names
  (`T001`, `AGR_DEFINE`), which the contract's rule that JSON field names
  are snake_case (04, API style; `TestRepresentations`) refuses. The API
  names each table in lower case (`t001`, `agr_define`), its description
  naming the table in SAP's spelling. The stored report keeps SAP's names,
  as every report a run has written holds them: `extractToAPI` maps between
  the two, so no data migration rewrites stored rows, and
  `TestReportMatchesContract` lowers the stored keys before comparing. On
  the wire this renames the keys of `report.extract`, which the free-form
  report returned in SAP's spelling before; neither `erp migrate` nor the
  walkthrough reads them, and the run's event carries no extract counts.
