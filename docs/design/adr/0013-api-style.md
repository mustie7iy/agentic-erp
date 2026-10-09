# ADR-0013: Resource-oriented REST with OpenAPI 3.1 as the source of truth

**Status:** proposed

## Context
The platform is API-first. Consumers include a web UI, agents (via tool
schemas and MCP), partner systems and the CLI. The contract must exist and be
reviewed before implementation.

## Decision
- Public API: resource-oriented JSON over HTTPS, versioned by major version in
  the path (`/v1`). Standard methods (list, get, create, update) plus custom
  methods for state transitions (`POST /v1/purchase-orders/{id}:release`),
  following Google AIP conventions.
- `api/openapi.yaml` (OpenAPI 3.1) is the source of truth. Server interfaces
  and clients are generated from it; handlers implement generated interfaces,
  so code cannot drift from the contract.
- Contract changes go through review; CI lints the spec and fails on breaking
  changes against `main` (oasdiff).
- Domain events are specified in `api/asyncapi.yaml`.
- Internal service-to-service calls (after extraction) use the same contracts.

## Alternatives
- **GraphQL:** flexible reads, but harder authorization per field, caching and
  rate limiting; may be added later as a read facade.
- **gRPC public API:** efficient, but weaker for browsers and partners;
  possible internally later.

## Consequences
- Rules in 04-api-design.md apply to every endpoint.
