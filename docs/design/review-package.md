# Design review package (T0)

**Status: ready for review.** The design (documents 01 to 07 and the 30
decision records) has been implemented through the transition steps T1 to
T25, so the review is of a design that runs: every claim below can be
checked against the code, the contracts and the test suites. Reviewers sign
off the checklist in [README.md](README.md); this package collects what
they need to decide, and what the implementation found out on the way.

## 1. What is on the table

| Artifact | Size | Where |
|---|---|---|
| Design documents | 7 (requirements, architecture, data model, API design, scalability, engineering process, transition) | [01](01-requirements.md) to [07](07-transition.md) |
| Decision records | 30, all `proposed` | [adr/](adr/README.md) |
| API contract | 165 operations (OpenAPI 3.1), 82 event types (AsyncAPI 3.0) | [`api/openapi.yaml`](../../api/openapi.yaml), [`api/asyncapi.yaml`](../../api/asyncapi.yaml) |
| Operator contracts | 9 cell operations, 8 global operations | [`api/control.yaml`](../../api/control.yaml), [`api/directory.yaml`](../../api/directory.yaml) |
| Physical model | 27 migrations, PostgreSQL 17, row-level security on every table | [`db/migrations/`](../../db/migrations/) |
| Tests | 443 test functions across 68 packages, 100% statement coverage, race detector in CI; load, soak and resilience suites | [docs/testing.md](../testing.md) |
| Transition | 25 steps done, each with its decisions and open points | [07-transition.md](07-transition.md) |

## 2. Assumptions to confirm

Six assumptions shaped the data model ([01 § Assumptions](01-requirements.md#assumptions-to-confirm)). Each needs a yes or a correction; a correction changes the schema, so these come first.

| # | Assumption | What depends on it |
|---|---|---|
| 1 | Multi-tenant SaaS: many tenants per platform, many legal entities per tenant, dedicated cells possible | `tenant_id` on every row, RLS, cells, the directory and router (ADR-0003, 0022) |
| 2 | Gapless numbering per legal entity and fiscal year for some document types | Number ranges and their locking (ADR-0004) |
| 3 | Three amounts per journal line: document, company and group currency | Ledger line shape (ADR-0005) |
| 4 | Up to 16 posting periods, non-calendar fiscal years | Period tables, partitioning by fiscal year (ADR-0006) |
| 5 | 10 years online, then archive; legal holds override | Partition maintenance, offboarding retention (ADR-0021) |
| 6 | Data residency by region; a tenant's data never leaves its home region | Cells per region, moves refused across regions (ADR-0023) |

## 3. Decision records

All 30 are `proposed`. The review accepts, amends or rejects each. They fall into three groups.

**Foundation (0001 to 0014).** Taken before T1 and exercised by every step since: modular monolith, PostgreSQL as system of record, pooled tenancy with RLS, UUIDv7 plus number ranges, integer minor units, append-only ledger with derived balances, transactional outbox with CloudEvents, optimistic locking and idempotency keys, governed custom fields, OIDC with permission, company and SoD authorization, hash-chained audit, long-running work as operations, resource-oriented REST from an OpenAPI source of truth, and the request pipeline with generated server code. One of them is not yet implemented as written: **ADR-0012 names Temporal** for long-running work, but proposals, migration runs and simulations still run inside one worker transaction (see the open points on chunked work below). The review decides whether Temporal stays the plan or the in-process workers with operations are the design.

**Platform operations (0015 to 0025).** Taken step by step as the platform grew: the control plane on the admin listener, write-once object storage, secrets by reference, per-tenant encryption keys with a KMS master, direct uploads, the offboarding lifecycle, the global directory and router, the global control plane, ambient cloud credentials and scheduled audit verification. Each has a question worth a reviewer's minute, listed in its record under Alternatives.

**Refinements (0026 to 0030).** Exports in parts and the audit-detail rule (0026 also closes a checklist item by decision: details carry identifiers, statuses and amounts, never personal data, and are never rewritten), operator roles, key rotation policy, re-onboarding from an export, and cross-cell verification. Together 0021, 0029 and 0030 define a tenant move as five operator steps; the review decides whether that is the operating model or whether a single long-running `:migrate` should drive it.

## 4. Open points from the transition

Every step recorded its open points (07-transition, "decisions and open points"). Of the 81 recorded, those closed by later steps come first; the rest are grouped by what kind of decision they need.

### Closed by later steps

| Raised in | Point | Closed by |
|---|---|---|
| T2, T8 | No API to create a tenant or its first administrator | T8 provisioning, T11 control plane |
| T3, T13, T14 | One deployment key for field encryption; rotation; KMS | T14 per-tenant keys, T15 KMS master, T23 rotation policy |
| T4, T5, T12 | Captured documents and bank files not stored; documents above 5 MB | T12 object storage, T16 direct uploads |
| T4, T6 | Tier 3 and 4 changes wait for simulation evidence; no simulation or regression API | T9 simulation, T10 regression suites |
| T6 | No tenant bootstrap or development issuer; `EnvSecrets` stand-in | T8 local login, T13 secrets |
| T8, T11 | Global tenant directory; control plane manages one cell; offboarding is a status only | T17 lifecycle, T18 directory and router, T19 global control plane |
| T11, T19 | Operator authorization all-or-nothing | T23 operator roles |
| T12 | Anchors checked on request only | T21 scheduled verification |
| T13 | `gosec` not in CI | T23 |
| T15 | Access keys instead of ambient credentials | T20 |
| T16 | Upload URL is a path | T23 `ERP_PUBLIC_URL` |
| T17 | Exports built in memory; audit details about erased objects | T22 chunked exports, ADR-0026 by decision |
| T17, T22, T23 | Restoring an exported tenant | T24 re-onboarding |
| T18 | Directory operations not on a contract; placement | T19 |
| T19, T23, T24 | Cross-cell verification of a moved tenant | T25 |
| T3, T7 | Projector lag not a metric; the outbox gauge counts whole outboxes on each scrape | Fixed after the package was written: `erp_outbox_lag_seconds` and `erp_projection_lag_seconds` (age of the oldest waiting row, one index lookup each) beside the counts |
| T15, T21 | The control plane could show a tenant's latest audit verification and the master wrapping its key | Fixed after the package was written: `encryption` and `audit_verification` on the tenant (migration 000027 lets the control role read them) |
| T22 | A lint of audit details for personal data | Fixed after the package was written: `audit.Write` refuses a detail whose key names personal data or whose value looks like an IBAN or an email address (ADR-0026 at write time) |
| T5 | No operation for the bank's status reports; `acknowledged` and `rejected` files never produced | Fixed after the package was written: `reportBankFileStatus` applies the bank's report (accepted or rejected, with its reason) to a transmitted file, publishing `BankFileAcknowledged` or `BankFileRejected`; failing the released run stays with the reversal decision |
| T4 | Purchase orders never closed; contracts never expired | Fixed after the package was written: `closePurchaseOrder` (`:close` on a released order; receipts and invoices refused after) and a contract-expiry job emitting `ContractExpired` |
| T2, T3, T4 | Event contract drift: `LegalEntityUnblocked` documented on `:activate`; no event for leaving `inactive`; `BankAccountChanged` naming the ended account as the new one; `PeriodClosed` described as an operation; `SupplierInvoiceCaptured` saying `parked` only | Fixed after the package was written: `LegalEntityActivated` and `OrgUnitActivated` added, descriptions corrected, `bank_account_id` null on an end-only change |

### Still open: contract gaps (the API review decides)

- Events: `PartnerChangeCancelled` has no operation; no event for recorded evidence (`ChangeRequestValidated` repeats) (T3, T9).
- Payment reversal: resetting a clearing, and with it failing a released run after the bank rejects its file (`PaymentRunFailed` for a released run), changes the ledger model of ADR-0006, so it is a decision rather than a gap; tax code rate changes with end validity (T3, T5).
- Custom fields validated for snake_case keys only; schema validation waits for published custom field schemas (ADR-0009) (T2).
- The per-tenant identity provider registry ADR-0014 expected is not built; trusted issuers come from deployment configuration (T2, T8).
- The configuration document still carries roles, users and SoD pairs the identity context owns; one house bank per company, no payment methods, cash discount or partial payments (T2, T5).
- The administrator holds every permission including approvals until real roles are provisioned; a tenant's first SoD rules could ship with the configuration (T8).
- Operator roles are coarse; per-cell or per-region scoping would be a further claim (T23).

### Still open: work that outgrows one transaction (the scalability review decides, with ADR-0012)

- Payment release answers synchronously and takes about 26 s for 2,000 payments over 100,000 items locally; proposals report progress only on completion; migration runs and simulations run within the request or one worker transaction (T5, T6, T9).
- A connector should confirm a sent bank file before commit once integration messages exist (T5).
- Incremental audit verification with checkpoints for chains too long to read hourly (T21).
- A `:migrate` operation driving the five steps of a tenant move, once the import can be called on a cell (T25).
- Regression and simulation replays cover supplier invoices only; suites of other transaction kinds, suite carry-forward across versions, and an explicit "accepted changes" step instead of the touched-companies heuristic (T9, T10).

### Still open: deployment and operations (the platform owner decides)

- Budgets are met at a laptop's rate (about 500 requests/s), not the largest tenant's 2,000/s sustained and 10,000/s peak; the first scalability gate of 05 §5.1 needs a cell-sized environment; quarterly resilience tests against real clusters (T7).
- Rate limits are per replica; per-agent-profile quotas belong at the gateway; read replicas and PgBouncer are deployment concerns (T7).
- Further providers behind existing ports: GCP KMS and Azure Key Vault masters; AWS Secrets Manager, GCP and Azure secret stores; ECS task role and process credential providers; a credentials check at start (T13, T15, T20).
- KMS key policy restricting `Decrypt` to the replicas' role is the deployment's (T15).
- Bucket lifecycle rule for uploads never consumed; exports to object storage and SAP extracts are still files on disk (T12, T16).
- Partitions for old years must exist in a destination cell before an import (T24).
- Cell capacity as a registered attribute for placement (T19).
- `erp-server provision` stays for bootstrapping a cell without an operator identity provider (T11).

### Still open: product and agents

- Agents are tested against a scripted model only; evals against the live API come before any agent gets autonomy above L0 (T6).
- Uploads serve capture only; attachments on other documents would reuse the resource (T16).

### Accepted as designed (no decision needed unless a reviewer objects)

- The router's cache period is the shortest read-only window a move can have (T18).
- The page-token key is not rotated; tokens are short-lived cursors (T14).
- The CLI's own credentials stay in the user's environment (T13).
- Fingerprints are snapshots per cell; a tenant still taking writes differs until frozen (T25).

## 5. Review checklist, with where to look

| Checklist item | Evidence |
|---|---|
| Assumptions confirmed or corrected | Section 2; [01 § Assumptions](01-requirements.md#assumptions-to-confirm) |
| Bounded contexts and data ownership agreed | [02-architecture.md](02-architecture.md), [03-data-model.md](03-data-model.md); `db/schema_test.go` proves no cross-context foreign keys |
| Every ADR accepted, amended or rejected | Section 3; [adr/README.md](adr/README.md) |
| Audit details carry identifiers, never personal data (ADR-0026) | [ADR-0026](adr/0026-export-chunking-and-audit-detail.md); the write-time rule in `internal/audit`; the lint is an open point |
| Data model reviewed by a finance/SAP functional expert | [03-data-model.md](03-data-model.md); the SAP mapping in `docs/tracker.json` (every item names its SAP counterpart) |
| API guidelines accepted; contract reviewed by a consumer | [04-api-design.md](04-api-design.md); `api/spec_test.go` enforces the rules; the CLI (`cmd/erp`) and the agents' API tools are the consumers so far |
| Scalability targets and evolution path accepted | [05-scalability.md](05-scalability.md); the load, soak and resilience suites in `internal/nfr`; the cells, directory, router and move built in T18 to T25 |

## 6. Decisions requested, in order

1. The six assumptions (section 2): any correction changes the schema.
2. ADR-0012: Temporal, or in-process workers behind operations, for long-running work. This decides how the "outgrows one transaction" group is closed.
3. The tenant move as five operator steps (ADR-0021, 0029, 0030) or one long-running operation.
4. The contract gaps (section 4): which events and operations are added before a consumer builds on v1.
5. The platform owner's list: the cell-sized environment for the first scalability gate, the further providers, the deployment concerns.
6. Acceptance of the remaining 28 ADRs as written, or amendments.
