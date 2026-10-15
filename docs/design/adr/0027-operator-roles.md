# ADR-0027: Operator roles on the control planes

**Status:** proposed

## Context
The control planes (ADR-0015, ADR-0023) trusted any operator the issuer
accepted with every operation: an operator who may look at tenants may also
offboard and erase them. Operator identity providers can say more about an
operator than that they exist.

## Decision

**A roles claim, three roles, one rule.** Operator tokens may carry a
roles claim (`roles` by default, `roles_claim` per issuer; a JSON array or a
space-separated string). Each control-plane operation needs one role:
`erp:read` to list and read, `erp:provision` to provision, `erp:operate`
for everything that changes a tenant or the directory; `erp:admin` implies
all. A token with the claim does what its roles allow (403
`authz.operator_role` otherwise); a token without the claim does
everything, so issuers that do not emit roles keep working, and the
deployment decides when to require them by emitting the claim.

## Alternatives
- **Roles per tenant.** Operators act on the cell or the platform, not on
  a tenant's behalf; the public API's permission model is the tenant's.
- **A role table in the platform.** The operator identity provider already
  holds who an operator is; one more place to administer the same people.

## Consequences
- The verifier parses one more claim; the control planes check one role per
  operation; the operation's role is recorded in the contract's description.
- A lost roles claim on an issuer that emitted it before widens access
  silently; a deployment that requires roles should make the issuer always
  emit the claim, empty when needed.

## Amendment (2026-10-14)

A roles claim that is present but malformed (not a JSON array of strings or a
space-separated string: a number, an object, a boolean, `null`, or an array
holding anything but strings) holds no role, so the token is refused every
control-plane operation (403 `authz.operator_role`). Only an absent claim
means every role; a claim the issuer meant to send but got wrong must not
widen access.

(2026-10-14, F7) An absent roles claim means every role only with
`ERP_ENV=development`. With `ERP_ENV=production` an operator token without
the claim holds no role, so every control-plane operation, on the cell's and
on the global control plane, is refused with 403 `authz.operator_role`:
a production operator identity provider must emit the claim. The verifier
learns the environment from its constructor (`authn.New`), not from a
global, and treats any environment other than development as production.
This supersedes the Decision's "a token without the claim does everything"
for production, and closes the consequence noted above (a lost claim
widening access) there. `ERP_ENV` itself is now required, with no default,
so a production replica cannot fall back to development's rule.
