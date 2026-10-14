# ADR-0025: Scheduled audit verification with an alarm

**Status:** proposed

## Context
The audit trail is hash-chained per tenant (ADR-0011) and anchored to
write-once object storage (ADR-0016), and `POST /v1/audit-events:verify`
recomputes a range on request. Nobody asks on a schedule, so a chain that
stops verifying is noticed when someone happens to look.

## Decision

**A worker verifies every tenant's whole chain in rounds**
(`ERP_AUDIT_VERIFY_INTERVAL`, default one hour): the chain from its first
event to its head, and every anchor against the events it fixes. Each
round's result per tenant goes to `audit.verification` (head, events
checked, anchors checked, the first broken sequence, insert-only).

**The alarm sounds once.** When a chain that verified stops doing so, the
round logs an error, publishes `AuditChainTampered` through the tenant's
outbox, and the gauge `erp_audit_chains_tampered` counts the tenant until a
round verifies again. Operators then verify on request to see where.

**Verification is full, not incremental.** Anchors fix the chain at points;
tampering can happen anywhere behind them, so each round recomputes from
the first event. The cost is a sequential read of a tenant's events once
an hour, affordable at Phase 1 volumes.

## Alternatives
- **Verify from the last anchor only.** Cheaper, blind to changes before
  it; the anchors are the reference, not the boundary of suspicion.
- **Alert from the verification endpoint.** It answers the caller; an alarm
  needs a subject that does not depend on one.
- **An external job calling the API.** One more thing to deploy and give a
  token; the worker uses the roles the platform has.

## Consequences
- One table, one worker, one gauge, one event, one setting; `0` leaves
  verification to requests.
- Incremental verification with checkpoints is the path when a tenant's
  chain outgrows an hourly read.
- The alarm is a log line, an event and a gauge; routing it to a pager is
  the deployment's.

## Amendment (2026-10-14)

`ERP_AUDIT_VERIFY_INTERVAL=0` is refused with `ERP_ENV=production`: leaving
verification to requests means a broken or tampered chain can go unnoticed
until someone asks, and a production cell must notice it on its own. In
development `0` still leaves verification to requests.
