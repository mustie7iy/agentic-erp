# ADR-0008: Optimistic locking and idempotency keys

**Status:** proposed

## Context
Many users and agents act on the same documents; networks retry requests.
Double-posting an invoice or payment is unacceptable.

## Decision
- Every mutable aggregate root has `version integer`. Updates use
  `WHERE id = $1 AND version = $2`; the API exposes it as an ETag and requires
  `If-Match` on updates and state transitions (412 on mismatch).
- Every non-safe request requires an `Idempotency-Key` header. The key,
  request hash and stored response are kept per tenant for 7 days in
  `platform.idempotency_key`; a repeat returns the stored response, a key
  reused with a different body returns 422.

## Alternatives
- **Pessimistic locks held across user think time:** blocks others, leaks locks.
- **Idempotency only for payments:** inconsistent client experience.

## Consequences
- Clients must handle 412 by re-reading; agents get it as a tool error.
- Retries are always safe.
