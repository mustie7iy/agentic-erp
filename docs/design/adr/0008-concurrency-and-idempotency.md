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

## As built: a retryable answer leaves the key free (review:T57f)

The unit of work stores a failed answer with the key only when it is a
business outcome: 404, 409, 410 or 422, the result of running the request
against the current state, which a retry would meet again. An answer that
carries `Retry-After` is not one, whatever its status: it says the same
request may succeed later, so storing it would replay it, forever, to the
very retry it invites. The 409 `request.in_progress` of a lock timeout
(55P03) was such an answer and was stored; it now leaves the key unstored
and rolls back like a rejection of the request itself (400, 401, 403, 412,
413, 428, 429) and a 5xx, so the retry with the same key runs. The same
holds for every problem that sets `RetryAfter`: the 503s of a
serialization failure or deadlock (40001, 40P01), a statement timeout
(57014), a failover (25006), a lost connection or the request's deadline,
the 429s of the rate limits and of the agents' model budget, and any
other answer a handler gives `RetryAfter`. One property decides, in one
place (`uow.businessOutcome`): the answer's `Retry-After` header, which
`problem.Write` sets from `RetryAfter`. A retryable answer records no
refusal (ADR-0046): it refused nothing. `TestRetryableAnswersLeaveTheKey`
covers each class and `TestLockTimeoutRetryRuns` a real lock timeout; a
409 without `Retry-After` is stored and replayed as before
(`TestErrorsKeepTheAnswerNotTheChanges`).
