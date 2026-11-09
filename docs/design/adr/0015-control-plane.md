# ADR-0015: Control plane for tenants on the admin listener

**Status:** proposed

## Context
Tenants are provisioned, suspended and offboarded by platform operators,
not by the tenants' own principals. The public API cannot carry these
operations: the tenant comes from the access token and is never in a URL
(ADR-0003, 04-api-design §4.2), so no request can address another tenant,
and operators hold no principal in any tenant. T8 (07-transition) left
provisioning as a server command; suspension and offboarding did not exist.
ADR-0003 foresaw "platform operations" on "a separate, audited maintenance
role" and 05-scalability a global tenant directory.

## Decision

**A separate contract on the admin listener.** The operators' API for
tenants is `api/control.yaml`, served by `erp-server` under `/control/v1`
on the admin listener (`ERP_ADMIN_ADDR`), which is internal to the platform
like readiness and metrics. It follows the public API's rules (problems,
cursor pagination, `Idempotency-Key`, `If-Match`), is linted with the same
ruleset and is implemented by hand-written handlers in
`internal/controlplane`, checked against the contract by a test.

**Operators authenticate with operator tokens.** The verifier trusts a
separate list of operator identity providers (`ERP_OPERATOR_ISSUERS`); an
operator token must name no tenant, and a tenant's token is refused by the
control plane as an operator's is by the public API. There is no permission
model: whoever the operator identity provider issues a token to may do
anything the contract offers.

**Changes are recorded in the tenant they change.** A status change runs in
the tenant's own scope as the application role; the tenant's hash chain
gets an audit event with actor kind `system` and an id derived from the
operator's issuer and subject, and the tenant's outbox publishes
`TenantStatusChanged`. Reads across tenants run as `erp_control`, a
`BYPASSRLS` role that may only `SELECT` the tenant table and the
idempotency keys.

**Suspension is enforced at the door.** The unit of work refuses requests
of a tenant that is not active (403 `tenancy.tenant_inactive`); the control
plane changes only the status. Offboarding is a final status; the control
plane deletes nothing.

## Alternatives
- **Tenant operations on `/v1` for a "platform tenant".** Would need a
  tenant that can see every tenant, contradicting ADR-0003's isolation by
  construction, and would make operators principals of a tenant that is not
  one.
- **Direct SQL or a server command only.** No audit of who did what in the
  tenant's chain, no idempotency, no contract; the T8 command stays for
  bootstrapping only.
- **Generating the control handlers with ogen.** A second generated server
  with its own security handler and error mapping for six operations; the
  hand-written handlers are smaller and the contract test keeps them
  honest.

## Consequences
- Two contracts to lint and check for breaking changes; the public API's
  operation count is unchanged.
- One more database role; `erp_control` has no write and cannot see
  business data.
- The tenant directory and cell routing of 05 §5.4 remain to be built; this
  control plane manages one cell.
- Operator roles (read-only, provisioning) would need a claim the verifier
  checks; today the operator identity provider decides.

## Amendment (2026-10-16, ADR-0031)

The control plane also administers each tenant's identity providers and
authentication policy, and binds principals to a registered provider. These
operations run in the tenant's scope and are recorded in its audit trail and
outbox like a status change; they need the `erp:admin` role (ADR-0027),
listing and reading them `erp:read`. Provisioning registers the tenant's
first provider with its first administrator; in production it is required.

## As built: workers leave an inactive tenant's work where it is (po:2026-11-09)

The decision enforced suspension at the door only: the unit of work
refused a request of a tenant that is not active, but the background
workers read queued work across tenants as `erp_worker` with no status
filter, so a suspended tenant's queued payment release was still posted
and its bank file sent. A worker must not do what the tenant could not
ask for, so every worker round that reads a tenant's work across tenants
now takes only the active tenants' (`tenant_id IN database.ActiveTenants`,
one subquery in `internal/platform/database`, which the worker role may
read since 000023), and leaves the rest untouched, neither attempted nor
marked as started, until the tenant is active again, when the next round
(or the next period, for the work started once a period) takes it. The
same holds for a tenant still provisioning or offboarded, whose work
stays for the offboarding lifecycle; erasure is not such work.

Filtered: the workflow dispatcher's queued operations (payment proposals
and releases, simulations, migration runs, depreciation and consolidation
runs), its approval deadlines, statement fetches, recurring entries and
cash snapshots; the in-process proposer, releaser, depreciator,
consolidator, recurrer, snapshotter and simulation worker; the bank file
transmitter, the cash exporter, the webhook dispatcher and the contract
expirer; the analytics scheduler's claim already filtered and now uses
the same subquery.

Not filtered, because they do no work on the tenant's behalf: the outbox
relay and the bus consumers (they move facts already committed, the
`tenant_status_changed` event of the suspension itself among them; a
delivery they fan out waits in the webhook dispatcher), the balance
projector (derived data of committed postings), session sweeps, the audit
anchor and verifier, key rotation, the directory sync, the webhook
delivery prune (retention), the replica record and the offboarding worker.
Three limits of "left where it is": a suspended tenant's pending webhook
deliveries older than the retention are still pruned, as held ones are; a
daily cash snapshot is of the day's position, so a day missed while
suspended is not taken afterwards; and without a workflow engine an
in-process run already running (a release or a depreciation or
consolidation run) pauses at its next step and goes on once the tenant is
active again.

## As built: a running workflow waits while its tenant is suspended (po:2026-11-09)

A workflow started before the suspension used to run on: its activities'
transactions did not check the tenant's status, so a payment release
started just before the suspension still posted. The product owner
decided (po:2026-11-09) that the run waits: while the tenant is not
active, its next step that does work on the tenant's behalf fails, to be
tried again, and commits nothing; once the tenant is active again the run
carries on where it stopped, and it never fails because of the
suspension.

The check is in one place, `database.BeginActive`: it begins as
`database.BeginOr` does and, in a tenant's scope, reads the tenant's
status in the transaction (the application role reads its own tenant's
row under RLS), and for a tenant that is not active rolls back and returns
a `*database.TenantInactiveError` naming the tenant and its status, an
ordinary error, not a non-retryable application error. Every activity type of
`internal/workflows` (payments, approvals, statements, recurring entries,
cash snapshots, migrations, depreciation and consolidation runs) and the
configuration simulation worker begin their transactions with it, so the
check is not repeated in any activity; a scope without a tenant (a
worker's read across tenants) is not checked. The status is read, not
locked, as the unit of work reads it for a request: a step whose
transaction began before the suspension committed may still commit, as a
request in flight may; the next step waits.

Retries: most steps' policies allow five attempts with backoff to a
minute, which a suspension would spend in about fifteen seconds. Every
workflow step therefore runs through `workflows.perform`, which adds
`TenantInactiveError` (the type name the SDK gives the Go error) to the
step's non-retryable types, so a step refused for its tenant goes to the
workflow at once without spending its attempts. The workflow then runs
`AwaitActive`, an activity that changes nothing, retried with no limit
at most a minute apart, and once it succeeds runs the step again with its
attempts whole. A step that wrapped the error is still recognised (the
failure's chain is searched), after its policy's attempts. A suspension of
any length therefore cannot exhaust the policy and fail the run; the
engine records an activity's retries as one event, so a long suspension
adds a few events to the history, not one per attempt. Other failures keep
their policy's limit. The new commands follow only a
failure that no activity returned before, so histories recorded earlier
replay alike and no `workflow.GetVersion` is needed (the retry policy is
not part of the command a replay matches). A release, migration,
simulation, statement fetch, approval deadline, recurring entry or cash
snapshot cancelled while it waits ends cancelled. Depreciation and
consolidation runs, and a payment proposal's run after its cancellation,
run their steps on a disconnected context so the run always ends
(`DepreciationRun`, `ConsolidationRun`, `ProposePaymentRun`): they wait
through a cancellation, and only the tenant's reactivation or terminating
the workflow in the engine ends the wait.

Not blocked: offboarding (export and erasure) begins its transactions
with `database.BeginOr`, not `BeginActive`, and so do the outbox relay,
bus consumers and the other workers listed above as not filtered. The
in-process runners (releaser, depreciator, consolidator) call the same
activity methods, so a run they carry stops at its next step when its
tenant is suspended and is taken again by a later round once the tenant
is active. Limits: a step of an offboarded tenant waits like a suspended
one's, until the tenant is active again or the workflow is cancelled
(where cancelling ends the wait, above) or terminated in the engine; a
cash snapshot whose workflow waited is taken after the reactivation, as
one delayed by an outage of the database is.
