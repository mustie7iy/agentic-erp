# ADR-0018: Per-tenant field-encryption keys wrapped by a master key ring

**Status:** proposed

## Context
Bank accounts, tax identifiers and house bank IBANs are sealed before they
reach the database (03-data-model §3.6) and fingerprinted for equality
search. Until T14 one deployment key did both for every tenant, could not
be rotated without rewriting every row at once, and gave tenants no key of
their own to rotate or destroy. ADR-0003 isolates tenants by row; their
secrets should be isolated by key as well.

## Decision

**Envelope encryption.** Every tenant has a data key (AES-256-GCM, bound to
the tenant as associated data) and a fingerprint key (HMAC-SHA256), both
random, stored wrapped in `tenancy.tenant_key`. New tenants get theirs at
provisioning.

**A ring of master keys** (`ERP_FIELD_KEYS`) wraps tenant keys: the first
wraps new ones, all of them unwrap. The master is an interface, so a KMS
can take the deployment key's place without touching the data. The legacy
deployment key stays as one master and as the opener of values sealed
before tenant keys.

**The sealed format names its key**: version 2 carries the tenant key id,
so any replica with the ring opens any value, including values sealed with
retired keys.

**Rotation is an operator action on the control plane**
(`:rotateKey`): a new key, the old one retired, every registered sealed
column re-sealed and re-fingerprinted in one transaction, recorded in the
tenant's audit chain. Contexts declare their sealed columns.

## Alternatives
- **Rotate the deployment key by re-encrypting everything at once.** Every
  tenant at the same time, no per-tenant isolation, no way to destroy one
  tenant's key.
- **A KMS call per seal and open.** Latency and cost on every request;
  envelope encryption keeps the KMS at the key level.
- **Per-row data keys.** Finer than needed; rotation would still rewrite
  rows, and fingerprints need one key per tenant anyway.

## Consequences
- One more table, one trigger relaxed for re-sealing, two columns of bank
  accounts updatable by the application for that purpose only.
- Tenant keys are cached unwrapped in each replica after first use.
- Fingerprints change when a tenant moves off the legacy key; the rotation
  rewrites them, so lookups keep working.
- Rotation policy and a KMS master remain to be built.
