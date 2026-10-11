# Architecture and tech stack

## Layers

```
┌───────────────────────────────────────────────────────────────────┐
│ EXPERIENCE      Role-based web workspaces · on-demand reports and │
│                 dashboards · approval and review queues ·         │
│                 configuration change reviews · chat · CLI         │
├───────────────────────────────────────────────────────────────────┤
│ AGENT LAYER     Business agents: AP exceptions (exists),          │
│                 analytics, invoice capture, close, collections,   │
│                 replenishment, maintenance, guardian              │
│                 Delivery agents: configuration, discovery,        │
│                 migration, custom code, integration, access, test,│
│                 operations, support, regulatory, enablement       │
├───────────────────────────────────────────────────────────────────┤
│ ORCHESTRATION   Invoice pipeline (exists) · durable workflows ·   │
│                 event bus · model gateway · policy checks (exists)│
├───────────────────────────────────────────────────────────────────┤
│ CONFIGURATION   Typed config repository (Git) · schema registry · │
│ PLANE           validators · sandbox clones · transaction replay  │
│                 simulator · change tiers · activation & rollback  │
├───────────────────────────────────────────────────────────────────┤
│ BUSINESS        GL and AP (exist) · AR · assets · controlling ·   │
│ SERVICES        purchasing · sales · inventory · manufacturing ·  │
│                 quality · maintenance · projects · HR core        │
├───────────────────────────────────────────────────────────────────┤
│ DATA            PostgreSQL · event outbox · object storage ·      │
│                 semantic layer (governed metrics) · columnar      │
│                 analytical store                                  │
├───────────────────────────────────────────────────────────────────┤
│ MIGRATION AND   SAP data extract (exists) · SAP customizing and   │
│ INTEGRATION     ABAP extract · SAP change data capture · quality  │
│                 gate and reconciliation (exist) · connectors      │
└───────────────────────────────────────────────────────────────────┘
  GOVERNANCE across all layers: named agent identities · segregation
  of duties · autonomy levels L0–L3 · change tiers 1–4 · approval
  thresholds · hash-chained audit log · agent evals
```

## Core rules

- **Agents propose.** Business services and the configuration plane decide. No agent writes to the ledger or to live settings directly.
- **People approve.** Exceptions, posting-relevant configuration and structural changes always need a named person.
- **Everything is audited.** Every posting, configuration change and agent decision lands in a tamper-evident chain.
- **Numbers come from queries.** The analytics agent plans and explains; the analytical store computes. See [analytics.md](analytics.md).
- **SAP is read-only.** Data, customizing and code are read from SAP; nothing is written back, so cutover stays reversible.

## Tech stack

Go end to end, including server-rendered web pages.

### In the repo today

| Area | Choice |
|---|---|
| Language | Go 1.26 |
| AI | Anthropic Go SDK, Claude Opus 5.5 by default, server-side refusal fallback |
| SAP integration | OData V2 client and JSON file extracts |
| Storage | PostgreSQL 17: row-level security per tenant, a unit of work per request, transactional outbox |
| Audit | Append-only, SHA-256 hash-chained log |
| Tests | `go test` with the race detector, plus a fake Claude API server |
| Analytics | Semantic layer (`semantic.yaml`), in-memory fact tables, Go query engine with row-level security, analytics agent, narrative check, HTML/SVG dashboards, saved and rerunnable reports, CSV export |
| Configuration format | YAML (`gopkg.in/yaml.v3`) |

### Planned

| Area | Choice | Why |
|---|---|---|
| Database | PostgreSQL 17: append-only ledger tables, row-level security per company code, point-in-time recovery | Reliable and audit-friendly system of record |
| Schema migrations | Atlas or goose | Versioned database changes |
| Configuration as code | YAML in Git, validated against Go types with CUE constraints; pull-request approvals | Every setting is versioned, reviewable and reversible |
| Sandbox clones | Postgres template databases cloned per change | Agents test changes on a copy in seconds |
| Simulator | Go replay engine over recent periods' transactions | Shows the posting impact of a configuration change before approval |
| Events | NATS JetStream, published from Postgres through an outbox | Event-driven agents without losing events |
| Workflows | Temporal, Go SDK | Payment runs, close, migration cycles and cutover survive restarts |
| APIs | gRPC/Connect with REST and OpenAPI | Typed internal APIs, REST for partners |
| Agent tool access | MCP servers for business services and the configuration plane | One governed way in for Claude and other vendors' agents |
| Model gateway | Built in Go | Model per task, prompt caching, cost per agent, spend caps, kill switch |
| Policy engine | Native Go (exists), Cedar as an option | Deterministic and testable |
| SAP analysis inputs | Customizing tables, ABAP source via abapGit or ADT, ST03N/STAD usage, CDHDR/CDPOS change logs | Feeds the discovery, configuration and custom-code agents |
| SAP deltas | SLT/ODP change data capture, or CDS views | Ongoing sync during the parallel run |
| Process mining | Go event-log analysis over SAP and core document flows | Discovery without a separate process-mining licence |
| Documents | S3-compatible object storage; Claude PDF input for invoices and orders | Document capture without a separate OCR product |
| Web UI | Server-rendered Go: templ + HTMX | Stays in Go; no TypeScript |
| Login | OIDC (Entra ID or Keycloak); mTLS identities for services and agents | Every agent has its own named identity |
| Ticketing | ServiceNow or Jira connector | Support agent works the existing queue |
| Analytical store at scale | Postgres change data capture into Parquet, queried by DuckDB embedded in Go; ClickHouse when volumes require it | Replaces today's in-memory fact tables without changing the semantic queries |
| Exports | Excel via excelize, PDF rendered server-side in Go | CSV exists today |
| Observability | OpenTelemetry, Prometheus, Grafana, Loki | Plus agent metrics: tokens, cost, override rate |
| Testing | testcontainers-go, reconciliation datasets, transaction replay, agent eval suites | Every change is regression-tested on real history |
| Deployment | Docker, Kubernetes with Helm (or ECS), Terraform, GitHub Actions | Continuous releases instead of upgrade projects |
| Tamper-proof audit | Audit chain anchored to write-once storage (S3 Object Lock) | Evidence for SOX auditors |

## Open decisions

- **Cloud provider** (AWS, Azure or GCP): decides KMS, object storage and Kubernetes choices.
- **SAP access**: SLT/ODP change data capture or only OData, and whether ABAP source and usage statistics can be exported. This decides what the discovery and custom-code agents can see.
- **First industry**: decides which configuration pack and which modules in phases 4–7 move forward.
