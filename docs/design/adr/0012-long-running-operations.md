# ADR-0012: Long-running work as Temporal workflows behind API operations

**Status:** proposed

## Context
Payment runs over 100k items, migrations, simulations, regression runs and
agent sessions exceed request timeouts and must survive restarts.

## Decision
- Such requests return `202 Accepted` with an `Operation` resource
  (`/v1/operations/{id}`: status, progress, result, error). The work runs as a
  Temporal workflow; each step calls the same application services as the API.
- Clients poll the operation or subscribe to its completion event.

## Alternatives
- **Cron + database queue tables:** simpler, but retries, timeouts and
  visibility are hand-built.

## Consequences
- Temporal is a new runtime dependency (self-hosted or cloud).
