# ADR-0003: Pooled multi-tenancy with row-level security; cells for scale

**Status:** proposed

## Context
Many client enterprises share the platform; some need dedicated
infrastructure (size, regulation, residency). Isolation failures are the
worst possible defect.

## Decision
- Every tenant-owned table has `tenant_id uuid NOT NULL` as the **first
  column of its primary key and of every unique key and index**.
- Row-level security on every tenant-owned table:
  `USING (tenant_id = current_setting('app.tenant_id')::uuid)`. The
  application role cannot bypass RLS; the tenant is set per transaction by the
  application layer from the authenticated token.
- A **cell** is a full stack (service, database, bus) serving a set of
  tenants. A global directory maps tenant → cell and region. Large or
  regulated tenants get a dedicated cell with the identical schema.
- Within a cell, Citus can distribute tables by `tenant_id` without schema
  changes because every key already leads with it.

## Alternatives
- **Schema per tenant:** strong isolation, but tens of thousands of schemas
  make migrations and connection pooling painful.
- **Database per tenant:** strongest isolation, highest cost; offered via
  dedicated cells instead.

## Consequences
- Composite keys everywhere (slightly larger indexes).
- Cross-tenant queries are impossible by construction (platform operations use
  a separate, audited maintenance role).
- Moving a tenant between cells is a copy of rows filtered by `tenant_id`.
