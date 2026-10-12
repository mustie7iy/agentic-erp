# ADR-0028: Key rotation by policy

**Status:** proposed

## Context
Tenant keys rotate on an operator's request (ADR-0018). Nothing makes sure
a key is rotated before it is a year old, or whatever age a deployment's
policy sets.

## Decision
A worker in the server rotates, once per round, the key of every active
tenant whose current key is older than `ERP_KEY_ROTATION_AGE` (default 0:
on request only), exactly as an operator's `:rotateKey` does, as a system
actor, recorded in the tenant's audit trail with the policy that caused it.
Tenants still on the legacy key count as due.

## Alternatives
- **Rotation inside the request path** when a stale key is used. Rotation
  re-seals every sealed row; a request is the wrong place for it.
- **An external scheduler calling the control plane.** One more thing to
  deploy and give an operator token; the worker uses the roles the server has.

## Consequences
- One more worker, one setting, one grant for the worker role to see key ages.
- A rotation storm on first enabling the policy in an old deployment is
  bounded by the hourly round over tenants in id order; the age can be
  lowered in steps.
