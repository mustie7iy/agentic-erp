# Platform design

**Status: proposed, awaiting review.** No application code is written against
this design until it is approved. The Phase 1 code in `internal/` is a working
prototype that proved the business logic; it will be moved onto this
foundation, as described in [07-transition.md](07-transition.md).

## Documents

| # | Document | Contents |
|---|---|---|
| 1 | [Requirements](01-requirements.md) | Scope, measurable non-functional targets, constraints, assumptions to confirm |
| 2 | [Architecture](02-architecture.md) | Architectural style, bounded contexts, components, data flow, deployment |
| 3 | [Data model](03-data-model.md) | Conventions, every bounded context's entities and relationships, physical design |
| 4 | [API design](04-api-design.md) | API-first rules: resources, methods, errors, pagination, idempotency, concurrency, events |
| 5 | [Scalability](05-scalability.md) | Load model, partitioning, tenant cells, evolution path without rewrites |
| 6 | [Engineering process](06-engineering-process.md) | Design gates, contract-first workflow, schema change rules, definition of done |
| 7 | [Transition](07-transition.md) | How the Phase 1 prototype moves onto this foundation |
| — | [Decision records](adr/) | One record per significant decision, with alternatives and consequences |

## Artifacts that implement the design

| Artifact | What it is | How it is checked |
|---|---|---|
| [`api/openapi.yaml`](../../api/openapi.yaml) | The API contract (OpenAPI 3.1): 165 operations over the Phase 1 resources, written before any handler | `vacuum` lint (0 errors, 0 warnings); `api/spec_test.go` enforces 04's rules (permissions exist, idempotency, `If-Match`, no deletes, human-only approvals); `oasdiff` breaking-change check against `main` |
| [`api/asyncapi.yaml`](../../api/asyncapi.yaml) | Domain events (AsyncAPI 3.0, CloudEvents 1.0): 77 event types | `api/asyncapi_test.go` (type format, context matches channel, examples match schemas) |
| [`api/control.yaml`](../../api/control.yaml) | The control-plane contract (OpenAPI 3.1): the operators' eight operations on tenants, served on the admin listener (ADR-0015) | the same `vacuum` lint and `oasdiff` check; `internal/controlplane/controlplane_test.go` checks every operation is served |
| [`api/directory.yaml`](../../api/directory.yaml) | The global control-plane contract (OpenAPI 3.1): the operators' seven operations across cells, served by `erp-router` on its admin listener (ADR-0023) | the same `vacuum` lint and `oasdiff` check; `internal/globalplane/globalplane_test.go` checks every operation is served |
| [`db/migrations/`](../../db/migrations/) | The physical data model: 23 versioned PostgreSQL 17 migrations, every Phase 1 context and the runtime roles | `db/schema_test.go`: 21 tests on a real PostgreSQL in CI, covering the design rules (RLS everywhere, keys lead with `tenant_id`, no cross-context foreign keys) and every database-enforced business rule |

## Review checklist

Reviewers sign off each item before implementation starts:

- [ ] Assumptions in [01-requirements.md § Assumptions](01-requirements.md#assumptions-to-confirm) confirmed or corrected
- [ ] Bounded contexts and their ownership of data agreed
- [ ] Every ADR accepted, amended or rejected
- [ ] Data model reviewed by a finance/SAP functional expert (ledger, documents, master data)
- [ ] API guidelines accepted; contract reviewed by a consumer (UI or integration)
- [ ] Scalability targets and evolution path accepted
