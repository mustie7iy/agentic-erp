# 6. Engineering process

How work moves from an idea to production. The aim is that design, contract
and data model are agreed **before** code is written, and that every rule in
this design is checked by a machine, not by memory.

## 6.1 Gates

```mermaid
flowchart LR
  A[Requirement] --> B[Design gate<br/>ADR + data model + contract]
  B --> C[Build<br/>generated interfaces, domain, repositories]
  C --> D[Verification gate<br/>tests, coverage, NFR suites]
  D --> E[Release<br/>expand migrations, canary, contract]
```

| Gate | Required to pass | Who signs off |
|---|---|---|
| **Design** | Problem statement and acceptance criteria; ADR for any significant or hard-to-reverse decision; data model change in `03-data-model.md` and a migration; API change in `openapi.yaml` / `asyncapi.yaml`, linted, no breaking changes | Tech lead + one domain (finance/SAP) reviewer + one API consumer for contract changes |
| **Verification** | CI green (§6.5); acceptance criteria demonstrated by tests; NFR budgets met for touched paths | Code reviewer |
| **Release** | Migration is backward compatible with the running version; feature flag or dark launch for risky changes; runbook updated | On-call owner of the context |

A change that touches only the inside of one context (no data model, contract
or ADR impact) skips the design gate and goes straight to review.

## 6.2 Decision records

- One ADR per significant decision in `docs/design/adr/`, numbered, with
  context, decision, alternatives and consequences.
- Statuses: proposed → accepted → (superseded by ADR-NNNN). Accepted ADRs are
  never edited except to mark them superseded; a change of mind is a new ADR.
- Write an ADR when a decision is expensive to reverse, crosses contexts, adds
  a runtime dependency, or relaxes a rule in this design.

## 6.3 Contract-first API workflow

1. Change `api/openapi.yaml` (and `api/asyncapi.yaml` for events) on a branch.
2. CI lints the contracts, runs `api/spec_test.go` (permissions, idempotency,
   `If-Match`, no deletes, problem responses) and diffs against `main` for
   breaking changes.
3. Review by a consumer (UI or integration) and the context owner.
4. Merge; server interfaces and clients are regenerated (`make generate`)
   and checked in. Handlers that do not implement the new interface fail to
   compile.
5. Contract tests replay the spec's examples against the running service.

## 6.4 Schema change rules

The database is the foundation, so changes to it are the most controlled.

**Every migration**

- Is a numbered pair `NNNNNN_name.up.sql` / `.down.sql` in `db/migrations/`,
  immutable once merged (fix forward with a new migration).
- Keeps the conventions of 03-data-model §3.1, checked by `TestDesignRules`:
  `tenant_id` leads every key, RLS on every table, no cross-context foreign
  keys, no floating-point money, no DELETE grant to the application.
- Comes with a schema test for every new database-enforced rule
  (`db/schema_test.go`): the valid case passes, the invalid case fails with
  the expected error.

**Zero-downtime (expand / migrate / contract)**

Releases are rolling, so the previous and the new application version run
against the same schema at the same time.

| Change | How |
|---|---|
| Add a column | Nullable or with a constant default (no table rewrite); backfill in batches; add `NOT NULL` later via a `NOT VALID` check, then `VALIDATE` |
| Rename a column or table | Never in place. Add the new one, write both, backfill, switch reads, stop writing the old one, drop it in a later release |
| Add an index | `CREATE INDEX CONCURRENTLY` in its own migration file (the runner applies such files outside a transaction) |
| Add a foreign key or check | `NOT VALID` first, `VALIDATE CONSTRAINT` in a later migration |
| Change a type | New column + backfill + switch, as for a rename |
| Drop anything | Only after no running version reads it, at least one release later |
| Add a partition | Ahead of need, by the maintenance job; never on the request path |

Long backfills run as operations in batches with progress, not inside a
migration.

## 6.5 Continuous integration

Every pull request runs, and must pass:

| Step | Tool |
|---|---|
| Format, vet, static analysis | `gofmt`, `go vet`, `staticcheck` |
| Security | `govulncheck`, secret scanning (`gitleaks`), static analysis (`gosec`, with G104, G115, G304 and G107 excluded by decision: errors ignored on purpose carry a comment, integer conversions are of bounded domain values, paths and URLs come from settings and flags; the generated server excluded; the rest annotated where a finding is a false positive, each with its reason) |
| Contract lint and breaking-change check | `vacuum` (Go), `oasdiff` (Go) |
| Schema tests on PostgreSQL 17 | `go test ./db` against a CI service container |
| Unit, functional and non-functional tests with the race detector | `go test -race ./...` |
| Coverage gate | `scripts/coverage.sh`, 100% of production statements |
| Fuzz targets | `make fuzz` (30 s per target on PRs, longer nightly) |
| Build and container image | `go build`, image scan |

Nightly: load and soak suites (05 §5.8), longer fuzzing, dependency updates.
Latency budgets on the shared runner are scaled by `ERP_BUDGET_SCALE`
(docs/testing.md); the budgets at scale 1 are checked in a cell-sized
environment.

## 6.6 Testing strategy

| Level | Scope | Runs against |
|---|---|---|
| Domain unit tests | Pure Go rules: matching, tolerances, tax, payment proposals | Nothing external |
| Schema tests | Database-enforced invariants and design rules | Real PostgreSQL |
| Repository tests | Mapping between domain and tables; error translation to API codes | Real PostgreSQL |
| Application tests | Use cases with transactions, authorization, idempotency, outbox | Real PostgreSQL, fake clock |
| Contract tests | Handlers against `openapi.yaml` examples and schemas | Running service |
| End-to-end | Procure-to-pay through the public API, as UI and agents use it | Full stack in containers |
| Non-functional | Performance budgets, load, soak, resilience, security (authz matrix, tenant isolation) | Full stack |
| Agent evaluations | Agent behaviour on recorded scenarios, with the LLM stubbed in CI and live in nightly evals | `llmtest` stubs / Claude API |

Tests never use mocks for PostgreSQL. A rule enforced by the database is
tested against the database.

## 6.7 Code layout for contexts

Each bounded context is one flat Go package, `internal/<context>`, holding the
layers of 02-architecture §2.1 as files rather than sub-packages: its types and
invariants, its use cases (each in the unit of work: transaction, authorization,
idempotency, outbox, audit), the SQL against its own schema, and `api.go`, the
handlers of its part of the generated OpenAPI interface. What other contexts may
call is the package's exported Go API, usually a small interface the caller declares
and the context satisfies (for example `payables` declares the `Payments` it
needs and `payments.Reservations` satisfies it), wired together in one place.

```
internal/<context>/      one context: types, use cases, SQL, api.go handlers
internal/delivery/<agent>/  each delivery agent stands alone
internal/money/          shared kernel every context may import
internal/platform/<pkg>/ technical services the contexts use (database, uow,
                         outbox, audit, authn, problem, ...) and test support
internal/platform/httpapi/  the composition root: builds the API from every
                         context's handlers and wires their ports together
```

Import rules, checked by `internal/layout`'s test, which parses the imports of
every production file under `internal/`:

- A platform package imports no context. The two exceptions are named in the
  test: `platform/httpapi`, the composition root, and `platform/fixture`, test
  support that builds a configured tenant.
- Any package may import the platform and the shared kernel (`money`).
- A context imports another context only where the test's allowlist says so.
  The list is exact: a new dependency fails the test until it is added there,
  in the same change as the interface it uses (and an ADR if it reverses a
  dependency direction), and an entry no longer used fails it too.
- Tests are not checked: a test may import whatever it needs.

The test checks Go imports only. SQL can still read another context's schema;
where it does today (offboarding and the control plane over tenancy and audit,
payments and configuration reading identity, the directory over tenancy) it is
listed in docs/refactoring-progress.md as a gap to close through the owning
context's Go API.

## 6.8 Definition of done

A change is done when:

- [ ] Acceptance criteria are covered by tests at the right level (§6.6).
- [ ] Contract, data model and ADRs match what was built.
- [ ] CI is green, including 100% coverage and schema tests.
- [ ] New permissions are seeded and mapped to roles; new rules have audit
      records and events.
- [ ] Metrics, traces and structured logs exist for new operations, with
      `tenant_id`, `principal_id` and `trace_id`.
- [ ] Migration follows §6.4 and was applied to a copy of production-sized data
      when it touches large tables.
- [ ] The tracker (`docs/tracker.json`) is updated.

## 6.9 Branching, review and release

- Trunk-based: short-lived branches, pull requests into `main`, at least one
  reviewer; two for migrations, contracts and security-sensitive code.
- Conventional commit messages; the pull request links its tracker item.
- `main` is always releasable. Releases are container images promoted
  through dev → staging → canary cell → all cells.
- Feature flags (per tenant) hide incomplete features; flags are removed once
  a feature is fully rolled out.
