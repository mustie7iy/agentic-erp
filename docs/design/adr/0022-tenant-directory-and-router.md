# ADR-0022: Global tenant directory and router

**Status:** proposed

## Context
ADR-0003 and 05-scalability §5.4 scale the platform by cells: complete
copies of the stack, each serving a set of tenants, with a global directory
mapping tenant to region and cell and a router forwarding each request to
the right cell. Until T18 the platform was one cell, and the control plane
of ADR-0015 managed that cell's tenants only.

## Decision

**The directory is one table in a database of its own.** `directory.tenant`
holds tenant id, region, cell, the cell's public API URL and a status. It
holds no business data, lives outside every cell's database
(`ERP_DIRECTORY_URL`), and is created by whoever opens it.

**Cells write their own entries.** Every cell with a directory configured
keeps its tenants' entries current: the control plane registers a tenant
right after provisioning and after every status change, and a worker
reconciles every tenant of the cell once a minute, so a missed registration
heals itself. Statuses pass through from the cell; `read_only` is set in the
directory alone, for a tenant being moved between cells, and survives the
cells' writes until an operator clears it.

**The router verifies, looks up, forwards.** `erp-router` verifies the
bearer token with the same issuers the cells trust, reads the tenant claim,
finds the cell in the directory (cached for `ERP_DIRECTORY_CACHE`, 30 s,
hits and misses alike) and reverse-proxies the request unchanged; the cell
verifies the token again and serves it. Unknown tenants get 404, suspended
and offboarded tenants 403 before any cell is reached, a moving tenant's
writes 503 with a retry, a cell that does not answer 502. The answer names
the cell in `X-Erp-Cell`.

**Clients may still call a cell directly** (`ERP_CELL_API_URL`, the
`servers` entry of the contract); the router is the one address that
survives a tenant moving.

## Alternatives
- **Directory inside each cell's database.** Every cell would need every
  tenant's entry, and a tenant's move would be a cross-cell write.
- **Routing by hostname per tenant.** DNS per tenant is slower to change
  than a directory row and leaks tenant identity into the host name.
- **A managed gateway with a routing table.** Possible later; the router is
  small, speaks the platform's problem format and uses its verifier, and a
  gateway can front it.

## Consequences
- One more binary (`erp-router`), two settings on the cell
  (`ERP_DIRECTORY_URL`, `ERP_CELL_API_URL`), a worker, a hook in the control
  plane.
- Tokens are verified twice on the routed path; the router adds one lookup
  per tenant per cache period.
- Moving a tenant (05 §5.4) now has its directory steps; the row copy and
  verification are still manual.

## Amendment (2026-10-16, ADR-0031)

The router finds a token's tenant from the token's issuer and audience in a
`directory.issuer` table, written by the tenant's cell when a provider is
registered (a claim that keeps the pair unique across cells) and reconciled
in rounds, rather than from a claim; the tenant's cell is then looked up as
before. A token whose issuer and audience no tenant registered is refused
before any cell is reached.
- Transport (amended 2026-10-16, ADR-0033): cells register https URLs in
  production, and the router (and the global plane in it) reaches them
  over mutual TLS as `spiffe://<domain>/global/erp-router`, the one
  identity a cell's API and `/control` serve.
