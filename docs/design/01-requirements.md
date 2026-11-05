# 1. Requirements

## 1.1 Purpose

A multi-tenant ERP platform that replaces SAP scope by scope. It is
**API-first**: every capability is exposed through a versioned, documented
API before any user interface, agent or integration uses it. Agents,
the web UI, the CLI, SAP connectors and partner integrations are all API
clients with no private back doors.

## 1.2 Functional scope

The data model and API are designed for the whole roadmap (see
`docs/capability-map.md`), and implemented in phase order.

| Phase | Contexts implemented | Foundation they rely on |
|---|---|---|
| 0–1 | Tenancy and organization, identity and access, configuration, business partners, general ledger, accounts payable, procurement, payments, workflow, audit, migration | All cross-cutting conventions in this design |
| 2 | Assets, banking, close, consolidation, controlling, tax, analytics | Ledger dimensions, fiscal calendar, currencies |
| 3+ | Sales, receivables, inventory, manufacturing, maintenance, projects, HR | Business partner roles, document pattern, item/material model |

## 1.3 Non-functional requirements

Targets are per environment unless stated. They drive the design choices in
[05-scalability.md](05-scalability.md) and are verified by the load and
resilience test suites before each phase goes live.

### Scale (design capacity, not launch volume)

| Dimension | Single largest tenant | Whole platform (one region) |
|---|---|---|
| Legal entities (company codes) | 500 | 50,000 |
| Named users | 20,000 | 500,000 |
| Concurrent active users | 3,000 | 50,000 |
| Journal lines per year | 500 million | 10 billion |
| Supplier invoices per year | 20 million | 400 million |
| Business partners | 5 million | 200 million |
| Online retention | 10 fiscal years | 10 fiscal years |
| API requests | 2,000/s sustained, 10,000/s peak | 50,000/s peak |

The architecture must reach these numbers by adding capacity
(nodes, partitions, cells), not by changing the architecture.

### Performance

| Operation | p95 | p99 |
|---|---|---|
| Read a single resource | 50 ms | 150 ms |
| List with filters (one page) | 150 ms | 400 ms |
| Post a journal entry or invoice (≤ 100 lines) | 200 ms | 500 ms |
| Approval decision | 150 ms | 400 ms |
| Trial balance for one company and period | 1 s | 3 s |
| Ledger reconciliation of two ledgers of one company and year, every page | 1 s | 3 s |
| Daily cash position of one company, 31 days (ADR-0051) | 2 s | 4 s |
| Liquidity forecast of one company over 100,000 open items, and its drill-down (ADR-0051, T53e4; product owner, 2026-11-04: computed on read, no stored flow table) | 3 s | 6 s |
| Group liquidity forecast across the caller's companies over 100,000 open items in total (ADR-0051 §9, T53e5; product owner, 2026-11-04) | 3 s | 6 s |
| Asset history sheet of one company, ledger and fiscal year (ADR-0057) | 1 s | 3 s |
| Depreciation run preview over 100,000 assets of one company and ledger, every page (ADR-0057) | 1 s | 3 s |
| Payment proposal over 100,000 open items | async, < 60 s | < 120 s |
| Depreciation run over 100,000 assets of one company and ledger (ADR-0057) | async, < 120 s | < 240 s |

### Availability and recovery

| Item | Target |
|---|---|
| Availability (production API) | 99.95% monthly |
| Recovery point objective (RPO) | ≤ 1 minute; 0 for posted financial data once synchronous replication is enabled |
| Recovery time objective (RTO) | ≤ 30 minutes for a region failure of a cell |
| Planned downtime for releases and schema changes | none (rolling deploys, online migrations) |

### Security and compliance

- Authentication through the tenant's identity provider (OIDC); MFA enforced by the IdP.
- Authorization on every request: permission, company access, segregation of duties.
- Tenant isolation enforced in the database (row-level security), not only in code.
- Encryption in transit (TLS 1.2+) and at rest; field-level encryption for bank accounts and national identifiers.
- Append-only financial records: corrections by reversal, never by update or delete.
- Tamper-evident audit trail for every business and configuration action.
- SOX: segregation of duties, approval evidence, change management for configuration.
- GDPR: personal-data inventory, blocking, erasure where retention rules allow, data residency per tenant.

### Operability

- Every request traceable end to end (trace id, tenant, actor).
- Metrics for every API operation and every agent (latency, errors, tokens, cost).
- Zero-downtime deployments and backward-compatible schema migrations.

## 1.4 Constraints

| Constraint | Source |
|---|---|
| Go for all services; no scripting languages in the platform | Product owner |
| PostgreSQL as the system of record | Phase 1 plan, ADR-0002 |
| API-first: contract approved before implementation | Product owner |
| No major re-architecture to reach the scale targets | Product owner |
| SAP remains read-only during migration | Migration strategy |

## Assumptions to confirm

These shape the data model. Each needs a yes or a correction in review.

1. **Multi-tenant SaaS.** One platform serves many client enterprises
   (tenants); each tenant has many legal entities. A tenant can also be
   given a dedicated cell (its own database cluster) for regulatory or size
   reasons, using the same schema. *If deployments will always be one
   enterprise per installation, tenancy stays in the model but is simpler to operate.*
2. **Calendar of legal requirements.** Gapless document numbering is
   required per legal entity and fiscal year for some document types
   (invoices in several EU countries); other numbers may have gaps.
3. **Currencies.** Three amounts per journal line, as in SAP ACDOCA:
   transaction (document) currency, company (local) currency and group
   currency.
4. **Fiscal calendars.** Up to 16 posting periods per fiscal year (12 plus
   4 special periods), non-calendar fiscal years allowed.
5. **Retention.** 10 years online, then archive; legal holds override.
6. **Regions.** Data residency by region (e.g. EU, US); a tenant's data stays
   in its home region.
