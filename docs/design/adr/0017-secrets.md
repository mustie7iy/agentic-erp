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

### As built: a tenant names only its own vault secrets (PO 2026-11-16)

The batch 76 review (R5 B1, review:batch76) found connectors' `secret_ref`
resolved by the server's resolver with no tenant scope: a tenant
administrator could name `env:DATABASE_URL`, `file:/etc/passwd` or another
tenant's vault path, and the value was sent as the connector's credentials
to an address the tenant chose. The product owner decided on 16 Nov 2026:

- **A reference a tenant gives resolves only under its own namespace,**
  `vault:<mount>/tenants/<tenant_id>/<path>#<field>`
  (`secrets.TenantRef`). `env:` and `file:` name the replica's own
  environment and files, so they are refused for tenants; they stay for
  the server's settings, which only the operator sets. Path segments and
  the field are letters, digits, `.`, `_` and `-`, never `.` or `..`, so
  no reading of the path (Vault's, a proxy's, a URL decoder's) leaves
  `tenants/<tenant_id>/`. The check is the guard, not Vault: the server
  reads with one token, so a tenant may name any mount that token reads,
  but only its own namespace under it; a deployment keeps tenants' secrets
  in a mount of their own and grants the token nothing else under
  `tenants/`.
- **Refused when written and when used.** Creating or changing a
  connector with another reference answers 422
  `integration.invalid_secret_ref` (field `secret_ref`); a reference
  stored before the rule, or written around the API (an import), is
  refused on use by `secrets.ResolveTenant` as
  `integration.secret_unavailable`, nothing resolved and nothing sent.
  Every connector kind resolves through it (`sap_odata`, `bank_sftp`,
  `treasury_export`, `einvoice_provider`; `bank_ebics` resolves nothing
  until T44d).
- **The sweep (rule 17).** No other reference a tenant gives is resolved
  by the server: web clients' `credential_ref` (ADR-0052) is set by the
  operator through the control plane, under `ERP_WEB_CLIENT_CREDENTIALS`.
- **The walkthrough keeps its connectors' credentials in a vault**: a KV
  version 2 stand-in on loopback (`vaulttest/vaultstub`, `ERP_VAULT_ADDR`,
  `ERP_VAULT_TOKEN` as a `file:` reference), the treasury key and the
  e-invoicing token under the walkthrough tenant's namespace; step 35
  shows a `file:` reference refused.
- **Decided by default** (no ADR or PO answer settled it): existing rows
  are not migrated, since no pilot data exists and a migration cannot know
  where a secret was moved to; a stored `env:`/`file:` reference simply
  stops resolving.

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
