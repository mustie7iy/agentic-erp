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

## As built

**Deferred work and a requester no longer active (review:T57f, PO
2026-11-03).** The work an operation defers acts as the person who asked
for it, read again in each step's transaction (`identity.Actors.Actor`):
a migration run's activity (T28), a payment proposal and a release
(T27, the proposer and the releaser), a simulation (its requester) and a
depreciation run's steps (ADR-0057 §7), with the workflow engine or the
in-process runners. The product owner decided on 2026-11-03 that a
requester blocked or deprovisioned since the start is refused. Each step
that does the work first calls `identity.Actors.MayAct`, the one place the
rule and its problem live: a requester no longer active is refused with
403 `identity.principal_blocked`, the code a request of theirs gets (the
unit of work's `resolve`), so no new code. The refusal is a business
problem, not retried, and fails the work through the path each job
already had for a problem, which records it as the requester whatever
their status (`Actor` stays a plain read for those steps):
- a migration run's operation fails and the run goes back to `queued` (or
  `loaded` for a reconciliation) with `integration.migration_run.fail`
  audited, nothing loaded (`integration.Endpoints.prepareAs`);
- a proposal fails its run and operation (`payments.payment_run.fail`),
  nothing proposed;
- a release refuses the next payment or the finish: the release
  operation fails (`payments.payment_run.fail`), the payments posted
  before stay posted, the run stays `releasing` without a bank file (or
  goes back to `proposed` with nothing posted), and a person active
  resumes it with `:release`;
- a simulation fails its operation, audited on the change request
  (`config.change_request.simulate_failed`, below), no evidence recorded,
  the change request left awaiting evidence;
- a depreciation run fails (ADR-0057 As built).
Decided by default: the check lives in one method each step calls rather
than in `Actor` itself, because the steps that record a failure must still
act as the requester. A step of a release, a proposal, a simulation or a
migration already done (a payment posted, a run released) stays a no-op on
a retry before the check, so a lost completion still succeeds; a
depreciation run's step checks first, so such a retry with the requester
blocked in between is refused, which leaves what was posted posted (a
posted run stays posted, `FailDepreciationRun` leaving it alone). The
ledger's recurrer keeps its own owner check (ADR-0056), which also needs
the owner's permission and company access.

**Every stopped simulation is audited (review:T57f, ADR-0011).** Every
problem that fails a simulation's operation, in the in-process worker and
in the `SimulateChange` activity alike (one code path,
`configuration.SimulationWorker.process`), records one
`config.change_request.simulate_failed` entry on the change request, in
the transaction that fails the operation, with the problem's `code` and
`detail` and the `operation_id`: an operation naming no change request
(`config.invalid_request`, recorded on the operation itself, object type
`platform.operation`, the action keeping its name since there is no change
request to record it on), a change request gone or no longer awaiting
evidence, a requester no longer active, a base or proposal this replica
cannot read whole, and a problem the simulator or the regression replay
reports. Decided by default: one action for every cause, the code telling
them apart, so the requester refusal's own `simulate_refused`, never
released, was folded into it. A transient error records nothing and leaves
the operation queued; the job runs only while its operation is queued, so
a retry after the commit records nothing twice. The other operation-backed
jobs already audit their failures on their own object
(`payments.payment_run.fail`, `payments.bank_file.fail`,
`integration.migration_run.fail`, `assets.depreciation_run.fail`).

**A run's failure is retried until it is recorded (review:T68d).** The
workflows that end a run through a failure step of their own,
`ConsolidationRun`, `DepreciationRun` and `ReleasePaymentRun`, ran that
step (`FailConsolidationRun`, `FailDepreciationRun`, `FailRelease`) under
the five-attempt policy of their other steps. A failure of the machinery
outlasting it left the run `queued`, `running` or `releasing` for good:
a consolidation run held its group (`consolidation_run_one_at_a_time`),
a depreciation run its company and ledger
(`depreciation_run_one_at_a_time`), a release its Operation running.
The failure step now runs under the same policy without an attempt
limit (`workflows.recording`): the backoff from a second to a minute
stays, a business problem stays non-retryable. Decided by default, the
smallest mechanism that always reaches a terminal state: the step is
idempotent, guarded by the run's expected status (a run no longer queued
or running, a release no longer under the workflow's operation, is left
alone), so retrying it is safe, and the workflows have no execution
timeout, so it is retried for as long as the cause lasts. A reaper that
fails runs whose workflow is gone was not built: it would need the
engine's visibility API and a second writer of the run's status. A
workflow still ends without its failure recorded when an operator
terminates it, when the failure step itself returns a business problem
(not retried), or, for `ReleasePaymentRun`, whose steps run on the
workflow's own context, when the Operation's `:cancel` cancels the step;
the release run then stays `releasing` with its Operation cancelled,
which `:release` resumes (payments.FailRelease). The retry policy is not
part of the command a replay matches (the SDK compares the activity's id
and type), so no `workflow.GetVersion` is needed and the recorded
histories replay. The one-activity workflows (`ProposePaymentRun`,
`MigrateRun`, `SimulateChange`) have no failure step of this shape: their
activity records its own failures and its exhausted retries leave the
Operation for the operator, as before.
