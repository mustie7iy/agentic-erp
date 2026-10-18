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

## Amendment (2026-10-14)

A principal is the subject of one identity provider: the unit of work
resolves a token to the principal whose issuer and subject are the token's
`iss` and `sub` in the tenant it names (`identity.principal.external_issuer`
next to `external_subject`), never by the subject alone. Tenants bring
their own identity providers and every trusted issuer may name any tenant,
so two issuers can issue the same subject; matching the subject alone let
one tenant's identity provider sign in as another tenant's principal. A
token whose issuer and subject match no principal of its tenant is refused
(403 `identity.principal_unknown`). Until the contract can name an issuer,
a principal created through the API is bound to the issuer its creating
administrator signed in with, and a new tenant's first administrator to
the cell's only tenant issuer; a cell that trusts several refuses to
provision. (03-data-model §3.4 has the schema and the backfill.)

## Amendment (2026-10-16, ADR-0031)

Trust in an identity provider is per tenant: a tenant's providers are
registered in its identity schema, and a token's tenant is the tenant whose
active registration holds the token's issuer and audience, not whichever
tenant a claim names. Multi-factor authentication is required where the
tenant's policy demands it, proven by the provider's `acr`/`amr` values, and
people's sessions have a lifetime and an idle timeout. Outside development
`ERP_OIDC_ISSUERS`, which trusts an issuer for every tenant, is refused. An
operator binds a principal whose issuer is not an active provider of its
tenant (such as `erp-dev` after 000046) to a registered provider. ADR-0031
has the design.

**Amended 2026-10-16 (T38d, ADR-0033).** Agents' tokens are bound to the
agent's client certificate (RFC 8705, `cnf.x5t#S256`). The router asks
every public client for a certificate and forwards its thumbprint to the
cell, which trusts it only from the router's SPIFFE identity. A bound
token, anyone's, is accepted only with its certificate, checked before
its session is touched; a confirmation other than a certificate's
(`cnf.jkt` of DPoP, a malformed `cnf`) is refused, and so is a bound
operator token, whose control planes see no certificate. In production an
agent's token is accepted only bound, from a provider registered with
`certificate_bound_tokens`; otherwise it is 401, logged. People and
services are not asked. The token remains the principal; the certificate
only proves the token's holder.

**Amended 2026-10-16 (T42, ADR-0036).** Agents reach models only through
the cell's model gateway, with their own tokens; they hold no provider
key. A tenant's AI kill switch (the tenant's administrators, or an
operator, whose suspension wins) refuses agents' model calls and agent
principals' API requests alike; people and services are not affected.
Autonomy levels: 0 reads, 1 also recommends, 2 also performs the
agent-permitted writes within their configured limits, 3 reserved.
