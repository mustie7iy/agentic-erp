# ADR-0015: Control plane for tenants on the admin listener

**Status:** proposed

## Context
Tenants are provisioned, suspended and offboarded by platform operators,
not by the tenants' own principals. The public API cannot carry these
operations: the tenant comes from the access token and is never in a URL
(ADR-0003, 04-api-design §4.2), so no request can address another tenant,
and operators hold no principal in any tenant. T8 (07-transition) left
provisioning as a server command; suspension and offboarding did not exist.
ADR-0003 foresaw "platform operations" on "a separate, audited maintenance
role" and 05-scalability a global tenant directory.

## Decision

**A separate contract on the admin listener.** The operators' API for
tenants is `api/control.yaml`, served by `erp-server` under `/control/v1`
on the admin listener (`ERP_ADMIN_ADDR`), which is internal to the platform
like readiness and metrics. It follows the public API's rules (problems,
cursor pagination, `Idempotency-Key`, `If-Match`), is linted with the same
ruleset and is implemented by hand-written handlers in
`internal/controlplane`, checked against the contract by a test.

**Operators authenticate with operator tokens.** The verifier trusts a
separate list of operator identity providers (`ERP_OPERATOR_ISSUERS`); an
operator token must name no tenant, and a tenant's token is refused by the
control plane as an operator's is by the public API. There is no permission
model: whoever the operator identity provider issues a token to may do
anything the contract offers.

**Changes are recorded in the tenant they change.** A status change runs in
the tenant's own scope as the application role; the tenant's hash chain
gets an audit event with actor kind `system` and an id derived from the
operator's issuer and subject, and the tenant's outbox publishes
`TenantStatusChanged`. Reads across tenants run as `erp_control`, a
`BYPASSRLS` role that may only `SELECT` the tenant table and the
idempotency keys.

**Suspension is enforced at the door.** The unit of work refuses requests
of a tenant that is not active (403 `tenancy.tenant_inactive`); the control
plane changes only the status. Offboarding is a final status; the control
plane deletes nothing.

## Alternatives
- **Tenant operations on `/v1` for a "platform tenant".** Would need a
  tenant that can see every tenant, contradicting ADR-0003's isolation by
  construction, and would make operators principals of a tenant that is not
  one.
- **Direct SQL or a server command only.** No audit of who did what in the
  tenant's chain, no idempotency, no contract; the T8 command stays for
  bootstrapping only.
- **Generating the control handlers with ogen.** A second generated server
  with its own security handler and error mapping for six operations; the
  hand-written handlers are smaller and the contract test keeps them
  honest.

## Consequences
- Two contracts to lint and check for breaking changes; the public API's
  operation count is unchanged.
- One more database role; `erp_control` has no write and cannot see
  business data.
- The tenant directory and cell routing of 05 §5.4 remain to be built; this
  control plane manages one cell.
- Operator roles (read-only, provisioning) would need a claim the verifier
  checks; today the operator identity provider decides.

## Amendment (2026-10-16, ADR-0031)

The control plane also administers each tenant's identity providers and
authentication policy, and binds principals to a registered provider. These
operations run in the tenant's scope and are recorded in its audit trail and
outbox like a status change; they need the `erp:admin` role (ADR-0027),
listing and reading them `erp:read`. Provisioning registers the tenant's
first provider with its first administrator; in production it is required.
