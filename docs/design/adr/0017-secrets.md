# ADR-0017: Secrets by reference

**Status:** proposed

## Context
The server needs credentials (database, object store, page-token and
field-encryption keys) and connectors need theirs (SAP users, bank
transfers). Until T13 the settings had to carry the values in the
environment of every replica, and connectors could name only environment
variables (`env:NAME`). 02-architecture names a KMS or Vault; the first
client decides the cloud.

## Decision

**Secrets are named by reference.** A reference is `env:NAME`,
`file:/path` (a secret mounted by the orchestrator) or
`vault:<mount>/<path>#<field>` (HashiCorp Vault KV v2). The settings that
carry secrets accept a reference in place of the value; `secret_ref` on a
connector is one. The platform stores no secret value anywhere.

**One resolver per replica**, built from `ERP_VAULT_*` when the
configuration loads: `env:` and `file:` always, `vault:` when a Vault
address is set. Vault is reached with a token or by the Kubernetes login
method (the service account token read from its file); an expired login is
renewed once. Settings are resolved at start, so a replica with a secret it
cannot read does not start; connector references are resolved on use.

**No SDK**: two HTTP calls (read a KV v2 secret, log in) implemented in the
package, with a test double.

**The repository is scanned for secrets** in CI (gitleaks); tests use
test-only credentials that are plainly such.

## Alternatives
- **Sidecar or init-container injection into the environment.** Works
  with the same settings, but leaves credentials in every process's
  environment and gives connectors no reference scheme of their own.
- **A secrets table encrypted with the field key.** Would make the
  database a secrets store and the field key a master key.
- **Provider SDKs.** Heavy dependencies for two calls; further stores are
  further schemes.

## Consequences
- Deployments choose the store; Kubernetes secrets as files need no Vault.
- Key rotation (field encryption, page tokens) and per-tenant keys remain
  open; the sealed format is versioned for it.
- The resolver is part of the configuration object, so the API and the
  workers share it.
