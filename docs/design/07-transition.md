# 7. Transition from the Phase 1 prototype

## 7.1 What Phase 1 is

The code in `internal/` (about 11,000 lines, 100% statement coverage) is a
working prototype of procure-to-pay, configuration as code, the delivery
agents and the SAP migration path. It proved the business rules and the agent
workflows. Its foundation does not meet this design:

| Aspect | Phase 1 prototype | This design |
|---|---|---|
| Storage | `core.Store`: in-memory maps saved as one JSON snapshot | PostgreSQL, schema per context, migrations in `db/` |
| Concurrency | One process-wide mutex (`Store.Mu`) | Transactions, optimistic locking, row-level constraints |
| Tenancy | Single tenant | `tenant_id` on every row, RLS, cells |
| Identifiers | Prefixed strings from a counter (`PO-000012`) | UUIDv7 ids plus document numbers from number ranges |
| Interface | CLI and agent tools calling Go functions | Public API from `openapi.yaml`; CLI and agents are API clients |
| Ledger | Mutable journal entries in a map; balances computed by scanning | Insert-only, partitioned journal; clearing table; derived balances |
| Events | None | Outbox per context, CloudEvents |
| Audit | In-process log | Hash-chained `audit.audit_event` in the same transaction |
| Packages | Organized by feature (`core`, `p2p`, `workflow`) | Organized by bounded context with domain/app/postgres/api layers |

## 7.2 Principle: keep the rules, replace the foundation

The prototype's valuable part is its **business logic and its tests**, not its
storage. The transition moves rules into context `domain` packages
unchanged where possible, puts repositories and the API around them, and uses
the prototype's tests as the first acceptance tests of the new code.

| Prototype package | Moves to | How |
|---|---|---|
| `money` | shared kernel `internal/money` | Kept; add decimal-string API encoding (04 §4.4) |
| `core` (matching, tolerances, index) | `payables/domain` | Rules kept; `Store` lookups become repository ports |
| `core` (ledger, posting) | `ledger/domain` | Becomes the posting service that builds balanced lines; persistence is insert-only |
| `core` (procurement) | `procurement/domain` | Kept; goods-receipt limits are also enforced by the database |
| `p2p` | `procurement/app`, `payables/app`, `payments/app` | Split by context; each use case becomes one transaction with outbox and audit |
| `workflow` | `workflow/domain` + `workflow/app` | Rules kept (no self-approval, human-only decisions); state in `workflow.*` tables |
| `governance` | `identity/app` (SoD, permissions) and `audit/app` | Runtime SoD checks move to the authorization layer |
| `config` | `configuration/domain` | Configuration documents become `config.config_version` rows; validation kept |
| `bankfile` | `payments/adapters/bankfile` | Kept as an outbound adapter |
| `capture` | `payables/adapters/capture` | Kept; captured documents go to object storage, hash in `capture_sha256` |
| `sap`, `migration` | `integration` | Extract readers kept; loaders write through the application services, never into tables directly |
| `delivery/*` agents | `agents` | Tools regenerated from OpenAPI operations instead of calling Go functions |
| `analytics` | `analytics` | Semantic layer kept; reads read models instead of the snapshot |
| `llm`, `llmtest` | `internal/llm` | Kept |
| `nfr`, `testkit` | Kept | Fixtures rebuilt on the database; NFR budgets re-baselined |

## 7.3 Sequence

Each step ends with CI green and nothing in production depending on the
prototype store for that context.

| Step | Work | Exit criterion |
|---|---|---|
| T0 | **Design approval**: this package reviewed, assumptions confirmed, ADRs accepted | Review checklist in README signed off |
| T1 | **Platform skeleton**: config, pgx pool with `SET LOCAL` tenant/actor per transaction, error translation, idempotency middleware, ETag handling, outbox writer and relay, audit writer, OpenAPI code generation, migration runner, health checks, telemetry | A generated "operations" endpoint works end to end with authn, idempotency and audit |
| T2 | **Tenancy, identity, configuration** contexts behind the API | Tenants, legal entities, principals, roles, number ranges managed only through the API; authz and SoD enforced |
| T3 | **Partner and ledger** | Journal entries posted and reversed through the API; projector maintains balances; trial balance meets its budget |
| T4 | **Procurement, payables, workflow** | Requisition → PO → receipt → invoice → match → post through the API, with approvals |
| T5 | **Payments** | Proposal as an operation, release with SoD, bank file, clearing |
| T6 | **Agents and integration** onto the API | Delivery agents and SAP migration use generated API tools only; prototype store deleted |
| T7 | **Hardening** | Load, soak and resilience suites pass at the targets in 01 §1.3 |

The prototype stays runnable as a demo until T6, then `core.Store` is
removed. The CLI becomes a thin API client.

## 7.4 Data from the prototype

Prototype snapshots are demo data, not production data, so there is no data
migration from the JSON store. SAP is the production source: real client data
arrives through the migration path in `integration` (reconciled loads through
the API), exactly as it will for every client.

## 7.5 Tracker

`docs/tracker.json` tracks the transition: module 14 (non-functional and
infrastructure) holds the platform items for T1, and each functional module
gets "on foundation" items for its context's step. A Phase 1 feature counts as
done again only when it runs on the foundation through the API.
