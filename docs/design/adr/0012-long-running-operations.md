# ADR-0012: Long-running work as Temporal workflows behind API operations

**Status:** accepted (2026-10-12, design review decision 2)

## Context
Payment runs over 100k items, migrations, simulations, regression runs and
agent sessions exceed request timeouts and must survive restarts. Through
the transition (T1 to T25) this work ran in in-process workers polling
database queues, one transaction per job, behind the `Operation` resource
the API already exposes; the review decided how it runs from here.

## Decision

**Scope.** Temporal runs the ERP processes that are multi-step, cross-module
or external, that wait on human approval or timers, or that are
long-running batches: procure-to-pay, order-to-cash, approvals, payroll,
period close, MRP and costing runs, bank and e-invoicing integration, bulk
imports. Their endpoints return `202 Accepted` with an `Operation` resource
(`GET /v1/operations/{id}`) exposing status, progress, result and errors.

**Ledger and inventory integrity.** Every financial or stock posting is a
single ACID database transaction executed inside one idempotent activity.
Workflows never split a posting across steps. Compensation for a posted
document is a reversal entry, never a deletion. Period status is
re-validated immediately before posting.

**Idempotency.** Workflow ids derive from business keys
(`{tenant}-{entity}-{docType}-{docNo}`); all activities are idempotent;
payments and external calls use idempotency keys.

**Out of scope.** Master-data CRUD and fast reads stay synchronous.

**Engineering rules.** Explicit retry policies and timeouts on every
activity; failed workflows raise exception-queue items for human
resolution; workflow versioning is mandatory; sensitive payloads are
encrypted through a payload codec; Temporal is self-hosted in-country, with
namespaces per environment and tenant isolation by task queue or namespace.

## Alternatives
- **Cron + database queue tables** (what the transition built): simpler,
  but retries, timeouts, resumption and visibility are hand-built, and a job
  is one transaction, so a proposal over 100,000 items cannot report
  progress or resume.
- **A message broker with consumers** (JetStream work queues): durable
  delivery, but no durable *state* between steps: timers, human waits and
  compensation would be hand-built on top.

## Consequences
- Temporal is a runtime dependency of every cell: a self-hosted cluster per
  region (data stays in-country), a namespace per environment, task queues
  per tenant tier where isolation requires it.
- `erp-server` gains a worker mode (02-architecture §2.5): the same binary
  runs workflows and activities against the same application services the
  API uses, as the application role in the tenant's scope.
- The `Operation` resource is the API's view of a workflow: its id is the
  workflow id, its status, progress, result and error are written by the
  workflow's activities; `:cancel` is a workflow cancellation. Nothing in
  the contract changes.
- Each posting activity reuses the unit of work: one transaction, the
  outbox and the audit chain written with it. Activities are retried on
  failure, so each one checks whether its effect already exists before
  acting (idempotency by business key).
- Failed workflows become exception-queue items an operator or a user
  resolves (retry, cancel, compensate), surfaced through the API.
- The transition plan gains the steps that move each worker onto Temporal
  (07-transition T26 onward): the runtime and the operations bridge first,
  then payment proposals (chunked, resumable) and release as an operation,
  simulations and migration runs, then approvals and period close as
  workflows with human waits and timers.
