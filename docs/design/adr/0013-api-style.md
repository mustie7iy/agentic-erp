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
