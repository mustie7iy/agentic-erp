# ADR-0010: OIDC authentication; permission, company and SoD authorization

**Status:** proposed

## Context
Tenants bring their own identity providers. Authorization must enforce
permissions, legal-entity access and segregation of duties on every call,
for people and agents alike.

## Decision
- Authentication: OIDC bearer tokens from the tenant's IdP (validated at the
  gateway and again in the service); machine clients and agents use OAuth2
  client credentials with their own identities. Users are provisioned via
  SCIM or just-in-time.
- Authorization in the service, per request: (1) permission from roles,
  (2) access to the legal entity in the resource, (3) SoD rules against the
  actor's full permission set, (4) workflow rules (requester cannot approve,
  agents cannot approve). Decisions are logged.
- Permissions are fine-grained verbs (`payables.invoice.post`); OAuth scopes
  map to coarse groups of them.

## Alternatives
- **Authorization only at the gateway:** cannot see resource data (company,
  amount) needed for decisions.
- **External policy engine (OPA/Cedar) now:** kept as an option; the decision
  function has a single interface so it can be swapped.

## Consequences
- Every handler calls one authorization function with the action and the
  resource; tests cover the full matrix (as in Phase 1).
