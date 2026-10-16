# ADR-0023: Global control plane over the directory

**Status:** proposed

## Context
ADR-0022 gave the platform a tenant directory and a router. Placing a new
tenant still meant choosing a cell by hand and calling that cell's control
plane; a tenant's move between cells (05-scalability §5.4) had its
directory steps only in code; and no operator could see the tenants of
every cell in one place.

## Decision

**One small contract across cells**, `api/directory.yaml`, served by
`erp-router` on its admin listener under `/directory/v1` with the same
operator tokens the cells accept: the directory's tenants and cells,
provisioning with placement, and the three directory steps of a move.

**Cells register themselves.** Each cell's sync worker records its own
entry (`ERP_CELL_REGION`, `ERP_CELL_API_URL`, `ERP_CELL_CONTROL_URL`,
`ERP_CELL_ACCEPTING`) every minute, so the directory knows which cells
exist, where their control planes are and which take new tenants.

**Placement is a forwarding.** `POST /directory/v1/tenants` takes the cell
control plane's request, picks the accepting cell of the region with the
fewest tenants (or the cell named), records the choice under the request's
`Idempotency-Key`, and forwards the request with the operator's token and
key to that cell's control plane; the answer comes back with the cell
named, and the new tenant is entered in the directory at once. A retry
reaches the same cell, whose idempotency answers it. The global plane keeps
no audit trail of its own: the cell records who provisioned the tenant.

**A move is three directory calls** under `If-Match` on the entry's
version: `:freeze` (read-only at the router), `:move` to a registered cell
of the same region after the rows were copied and verified outside the
API, `:unfreeze`. The router follows within its cache period.

## Alternatives
- **Placement in the cells' control planes.** A cell cannot know the other
  cells' load; the directory does.
- **A separate global service.** The router already holds the directory
  connection and the operator issuers; one more listener costs less than
  one more deployment.
- **Automating the row copy.** Logical replication with a row filter is
  deployment-specific; the API fixes the steps around it.

## Consequences
- Entries carry a version; `read_only` is one of their statuses, set only
  here.
- Three more cell settings, two more router settings
  (`ERP_OPERATOR_ISSUERS`, `ERP_PAGE_TOKEN_KEY`), one more table
  (`directory.placement`).
- Capacity is a count of tenants; weighing cells by size or load is a
  later refinement.
- Transport (amended 2026-10-16, ADR-0033): cells register https URLs in
  production, and the router (and the global plane in it) reaches them
  over mutual TLS as `spiffe://<domain>/global/erp-router`, the one
  identity a cell's API and `/control` serve.
