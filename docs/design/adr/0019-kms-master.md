# ADR-0019: KMS master key in front of the ring; tenant keys move by re-wrapping

**Status:** proposed

## Context
ADR-0018 gave every tenant its own field-encryption keys, wrapped by a
master key of the deployment's ring (`ERP_FIELD_KEYS`). Those masters are
secrets the replicas hold in memory, decoded from the environment or a
vault; whoever reads a replica's environment can unwrap every tenant key.
A key management service keeps the master inside a hardware boundary,
authorizes each use, logs it and rotates it in place. The master was left
as an interface for this.

## Decision

**AWS KMS as a master** (`fieldcrypt.KMS`): `Wrap` is KMS `Encrypt`,
`Unwrap` is KMS `Decrypt`, both with the tenant id as encryption context,
so a wrapped key only unwraps for its tenant. The platform speaks the JSON
protocol directly, signed with the Signature Version 4 signer it already
has for S3 (`internal/platform/sigv4`); a service speaking the same
protocol (LocalStack, a compatible KMS) works through `ERP_FIELD_KMS_ENDPOINT`.

**The KMS master stands first in the ring.** With `ERP_FIELD_KMS_KEY` set,
new tenant keys are wrapped by it; the local masters of `ERP_FIELD_KEYS`
still unwrap what they wrapped. The master's name in `tenancy.tenant_key`
is `kms` unless the setting says `name=key`, so the key itself (an ARN, an
alias) can change without renaming what is stored.

**Tenant keys move onto the new master by re-wrapping, not re-sealing.**
`Keyring.Rewrap` unwraps every key of a tenant that another master wrapped
and wraps it again under the first master; the keys do not change, so no
row is rewritten. `erp-server rewrap` does it for every tenant of a cell.
After it, the masters the KMS replaced can leave `ERP_FIELD_KEYS`.

**KMS is on the path of first use, not of every seal.** Tenant keys stay
unwrapped in each replica's memory after first use (ADR-0018), so a
tenant's first request on a replica and each rotation call KMS; sealing
and opening do not.

## Alternatives
- **KMS `GenerateDataKey` per tenant.** The same envelope with KMS making
  the key; `Encrypt` of a key the platform generated keeps the one `Master`
  interface for local masters and KMS alike, and the fingerprint key needs
  generating anyway.
- **The AWS SDK.** Two calls and a signer the platform already had; the
  SDK would be the largest dependency of the module for them.
- **Moving tenants to KMS by rotation.** Rotation re-seals every sealed row
  of a tenant; re-wrapping touches `tenant_key` only and needs no outage of
  the local masters.

## Consequences
- Credentials for KMS are settings (`ERP_FIELD_KMS_ACCESS_KEY`,
  `ERP_FIELD_KMS_SECRET_KEY`, references allowed per ADR-0017); ambient
  credentials (instance roles, IRSA) are still to come.
- A KMS outage stops new tenants, rotations and the first request of a
  tenant on a fresh replica; tenants already in memory keep working.
- Other services (GCP KMS, Azure Key Vault) are further `Master`
  implementations; the ring and the re-wrap do not change.
