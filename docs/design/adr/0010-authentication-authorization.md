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

## As built: every permission check applies the SoD rules in force (review:batch44, 2026-11-02)
Decided by default under ADR-0053 §d, from this ADR's step (3) ("SoD rules
against the actor's full permission set") and `Authorize` as built:
- **One decision for every check.** A permission checked beyond the
  operation's own is decided as `Authorize` decides the operation's: held,
  the first missing in the order given 403 `identity.permission_missing`,
  and one a rule in force pairs with another the principal holds 403
  `identity.sod_conflict`. `authz.Require` (now variadic) does this through
  the same `holdsAll`; `authz.MayUse` answers the same question as a
  boolean for a step a missing permission skips rather than refuses, and
  fails closed when the rules cannot be read. Batch 44's review found
  `Require` checked only that a permission was held.
- **The callers, each held to it; none has a reason not to be.**
  - `approveApprovalRequest` / `rejectApprovalRequest`: the object type's
    approval permission (`approvals.ObjectPermissions`). Only people get
    there (agents are refused first), and an approval permission paired
    with the one that makes the object is the rule SoD exists for.
  - The model gateway (`createModelMessage`, `httpapi.modelRoute`): its
    x-permission and x-also-permissions, for agents, people and services
    alike, as `Authorize` holds them on every other operation.
  - A metric definition change's simulation (`analytics.query.run`): it
    runs as the change request's requester (`configuration.SimulationWorker`
    acts as `platform.operation.created_by`), a person, not as a system
    principal, so the requester's rules apply.
  - An analytics query's fact types (`endpoint.datasets`) and the
    analytical store's reads (`store.Source.permitted`): both refuse "as
    the API would refuse that read", and the database path's reads already
    went through `Authorize`, so a barred person got 403 from the database
    and rows from the store; the store and the database now answer alike.
    The store checks one permission at a time in the order the database
    path reads, as each of that path's reads is authorized before the
    next, so the same problem comes first on either path.
  - The approvals inbox (`listApprovalRequests decidable_by=me`): it offers
    the object types whose approval permission is usable, not merely held,
    so a request is listed exactly when the decision would succeed
    (ADR-0052 G1).
  - Automatic posting of matched bank statement lines
    (`payments.autoPost`): a person or integration barred from
    `payments.statement.post` imports and runs the rules, and nothing
    posts, as for one without it. A rule pairing it with
    `payments.statement.import` needs nothing more: the person cannot
    import (the import operation's `Authorize` refuses them), so no rules
    run for them; any other rule pairing it bars the posting alone
    (TestAutomaticPostingBarredBySoD). The statement fetcher, a system
    principal, still posts without the check: it acts for
    the bank's company and is no person whose duties SoD separates
    (ADR-0047).
- **Not changed.** `authz.Effective`'s other callers (getCurrentPrincipal,
  recurring entries' owner check) already use the usable permissions alone; `authz.RequireWrites` checks an agent's autonomy
  level, not a permission. `MayUse` and `barredBy` came with batch 44's
  expansions (expperm); their callers there (ledger's entry partners and
  revaluation names, `authz.Expansions` in procurement and payables) were
  already SoD-aware.

## As built: an inactive role gives its holders nothing anywhere (review:T95c, 2026-11-16)
Decided by default under ADR-0053 §d, from step (1) here ("permission
from roles") and `authz.Permissions` as built, which grants a role's
permissions only while the role is active:
- **Where a role's holders are resolved, the role's status counts.**
  `identity.Directory.HoldsRole` and `RolesHeld` read an assignment only
  of an active role, as `authz.Permissions` and the segregation-of-duties
  check (`identity.sodConflict`) already did. T95c's review found both
  ignored the status, so the approvals inbox (`decidable_by=me`) listed,
  and `approveApprovalRequest` / `rejectApprovalRequest` let decide, a
  step of a role made inactive after the request was routed to it, while
  authz granted that role nothing.
- **No operation deactivates a role yet** (`updateRole` changes its name
  and description alone), so a role becomes inactive only out of band
  today; the check is defence in depth until such an operation exists.
- **A step already routed to a role made inactive** stays on that role
  (the request records it, `workflow.approval_step.role_id`) and is
  decided only by holders of it once it is active again: until then every
  approval or rejection is 403 `workflow.role_missing`, as for a person
  without the role, and no inbox lists it. A new request is not routed
  to it: steps resolve their role codes through `RoleByCode`, which
  already read active roles alone (422 `workflow.unknown_role`). Moving
  the step to another role is not built; the requester can still
  withdraw the request (`approval-requests/{id}:cancel`).
- **The callers.** Approvals (decision and inbox), the payment run's
  release role (`payments.Endpoints`, whose `RoleByCode` already refused
  an inactive role, so nothing changes there) and the reports a role is
  shared with (`app.reportGrantees.RolesHeld`, which filtered the status
  itself and now takes `RolesHeld` as it is; ADR-0076 §7).
- **Not changed.** The agent-approval trigger
  (`identity.check_agent_approvals`, migration 000013) still counts an
  inactive role's grants: it refuses an agent an approval permission it
  would hold once the role is active again, the safer reading. The
  segregation-of-duties check's limit to one role's holders reads every
  assignment of the role, but judges only active roles' permissions.
