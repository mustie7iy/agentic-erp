# ADR-0031: Per-tenant identity providers and sessions

**Status:** accepted (2026-10-16)

## Context
ADR-0010 lets tenants bring their own identity providers, but trust is
deployment-wide: every `ERP_OIDC_ISSUERS` entry may name any tenant through
a claim, which the issuer-and-subject binding (ADR-0010, amended
2026-10-14; F7) only contains. Tenant providers are discovered at startup,
so one unreachable provider stops a cell, and go-oidc's key set refetches
on every unknown key id without a timeout. Nothing enforces multi-factor
authentication, session lifetime or idle time. Principals that 000046 bound
to the development issuer `erp-dev` are refused outside development and
cannot be re-bound (T37).

## Decision
- **Registry.** Each tenant registers its identity providers in its own
  schema (`identity.identity_provider`, row-level security), administered
  by operators on the control plane; tenants do not administer them
  through the public API. An active (issuer, audience) belongs to one
  tenant: the registration, not a token claim, decides a token's tenant,
  and a registered tenant claim is a cross-check. Issuer and audience never
  change (a change is a new registration), a retired registration stays
  retired, and a tenant keeps at least one active provider. The router
  reads a projection (`directory.issuer`) that the cell writes as a claim
  at registration and reconciles in rounds; the claim makes the pair unique
  across cells. Lookups are cached for 30 s, so a retired provider may
  authenticate for up to that long.
- **Discovery and keys** are fetched lazily per issuer and cached:
  discovery 24 h, keys 1 h or the response's `Cache-Control` max-age within
  5 min to 24 h. An unknown key id refreshes at most once a minute per
  issuer, concurrent requests sharing one fetch; every fetch has a 5 s
  timeout and a 1 MiB limit; issuers are https, and in production no
  fetch reaches a loopback, private or link-local address. An unreachable
  provider with no cached keys is 503, not 401; old keys stay until a fetch
  succeeds.
- **Multi-factor.** The tenant's policy (`identity.auth_policy`) says
  whether people must prove multi-factor; each provider says how it proves
  it, by `acr` values or `amr` values (RFC 8176). A policy requiring it
  while a provider can prove neither is refused. A person's token that
  proves neither is answered with the step-up challenge of RFC 9470.
  Services and agents are exempt.
- **Sessions.** A session is one authentication at the provider: its `sid`,
  or its subject and `auth_time`. Its absolute lifetime runs from
  `auth_time` (or its first sighting); its idle time from a server-side
  `identity.session` row, touched in a transaction of its own at most once
  a minute per replica. An ended session never resumes, so a refreshed
  token cannot revive it. A person's token with neither `auth_time` nor
  `sid` is refused. Session starts and ends are audited; sessions publish
  no events and are not exported, imported or fingerprinted. Defaults:
  12 hours absolute (15 minutes to 24 hours), 30 minutes idle (5 minutes to
  8 hours, never above the lifetime), multi-factor not required.
- **Operators** need `erp:admin` to register, change or retire providers,
  change the policy and bind principals; `erp:read` lists and reads them.
  An operator binds an unbound principal (one whose issuer is no active
  provider of its tenant, `erp-dev` included) to a provider and subject,
  one principal at a time.
- **Startup.** Production refuses `ERP_OIDC_ISSUERS` (trust for every
  tenant), requires `ERP_OPERATOR_ISSUERS`, and refuses to start while an
  active tenant has no active provider. Development keeps the shared
  issuers and `erp-dev`.

## Alternatives
- **Registry in the global directory only:** outside the tenant's export
  and audit chain; cells would depend on the directory to authenticate.
- **Global issuers with a per-tenant allow-list:** one trust list for every
  tenant still, and the tokens of enterprise providers, which carry no
  tenant id of ours, could not be routed.
- **Stateless sessions only:** cannot enforce an idle timeout. Opaque
  tokens with introspection: a call to the provider on every request.
- **Session revocation through the API:** deferred; the session table can
  end a session as revoked when it is needed.

## Consequences
- One cached lookup per request on the cell (as `erp_control`) and on the
  router; about one session write per minute per active person per replica.
- Tenants' providers must put `auth_time` or `sid` in people's tokens.
- The (issuer, audience) unique index is the one key in a tenant schema not
  led by `tenant_id`: the pair decides the tenant. The registry, like the
  rest of the identity context, has no foreign key into `tenancy`.
- Amends ADR-0010 (trust per tenant), ADR-0015 and ADR-0027 (operations
  and their roles), ADR-0021 (sessions not exported) and ADR-0022 (the
  router resolves the tenant by issuer).
- Built in steps T37a–T37h (07-transition §7.6).
