# ADR-0007: Domain events via transactional outbox, in CloudEvents format

**Status:** proposed

## Context
Contexts react to each other (approval completed → requisition approved),
read models and analytics need change feeds, and dual writes to a database and
a broker lose events on failure.

## Decision
- Each context writes events to its own `outbox` table in the same transaction
  as the state change. A relay publishes them to NATS JetStream and marks them
  sent. Delivery is at-least-once; every consumer is idempotent (it records
  processed event ids in its own `inbox` table).
- Event envelope: CloudEvents 1.0 (`id`, `source`, `type`, `subject`, `time`,
  `dataschema`, and the extensions `tenantid`, `actorid`, `traceid`). Event
  types are versioned (`com.example.erp.payables.supplier_invoice_posted.v1`)
  and documented in `api/asyncapi.yaml`. The outbox enforces the format and
  that a context publishes only its own types (CHECK on `event_type`), and
  stores the acting principal (`actor_id`) with every event.
- NATS subjects are `erp.<cell>.<context>.<tenant>.<event>`, so streams can be
  partitioned and ordered per tenant.
- Events carry the facts needed by consumers (ids, amounts, status), not full
  aggregates.
- When a context is extracted to its own service, cross-context transactions
  become sagas driven by these events, with compensating commands.

## Alternatives
- **CDC from tables (Debezium) as the event source:** no outbox writes, but
  leaks table structure as the public contract.
- **Direct publish after commit:** loses events on crashes.

## Consequences
- Small write amplification (one outbox row per event).
- A replayable, ordered (per aggregate) history of business facts.
