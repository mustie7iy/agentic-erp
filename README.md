# agentic-erp

Move off SAP one company code at a time, with every step proven by
reconciliation, and run the business processes on an agentic layer instead of
SAP screens.

Two objectives drive the design:

1. **Replace SAP's capabilities.** The [capability map](docs/capability-map.md)
   covers 120 capabilities across S/4HANA and SAP's cloud products, each built
   in the native core, rebuilt with agents, or integrated with a specialist product.
2. **Cut consultant and engineering cost.** Agents configure the system,
   migrate it (configuration as well as data) and run it day to day, working on
   configuration as code with validation, simulation and human approval. See
   [docs/agentic-delivery.md](docs/agentic-delivery.md).
3. **Answer any question on demand.** No catalogue of static reports: people
   ask in plain language and an agent builds the report or dashboard on the
   spot from governed data. See [docs/analytics.md](docs/analytics.md).

Written in Go. The only dependency is the Anthropic Go SDK.

## Why this approach

A big-bang SAP replacement is expensive and risky because everything has to
work on cutover day. This project follows a **strangler** pattern, combined
with the "Headless ERP" model from Rimini Street's *The Rise of Agentic AI ERP*:

1. **Connect.** Read SAP through its standard APIs. Nothing in SAP changes.
2. **Migrate a scope.** Load one company code and fiscal year into the native core.
3. **Prove it.** Reconcile the core against SAP independently: trial balance,
   open payables per vendor, POs, invoices. Any difference blocks cutover.
4. **Run it.** Process that scope's work natively. Rules handle the routine
   path, Claude agents investigate exceptions, and people approve.
5. **Repeat** for the next scope, then decommission SAP.

The savings come from shrinking what you license, upgrade and staff on SAP,
scope by scope, and from agents doing the configuration, migration, testing and
support work that consultants do today.

| Document | Contents |
|---|---|
| [docs/plan.md](docs/plan.md) | Two-track roadmap (business modules and delivery agents), exit criteria, risks |
| [docs/capability-map.md](docs/capability-map.md) | Every SAP capability, its approach, phase and status |
| [docs/agentic-delivery.md](docs/agentic-delivery.md) | Consultant roles mapped to agents, configuration as code, change tiers |
| [docs/analytics.md](docs/analytics.md) | On-demand reports and dashboards: how requests are answered, guardrails, components |
| [docs/architecture.md](docs/architecture.md) | Platform layers and full tech stack |
| [docs/migration.md](docs/migration.md) | Cutover protocol, migration modes, reconciliation controls |
| [docs/testing.md](docs/testing.md) | Functional and non-functional test suites, fuzzing, coverage gate |

## Quick start

```sh
make test       # functional, end-to-end and non-functional suites, race detector on
make coverage   # whole-suite statement coverage gate
make demo       # mock SAP -> migrate company 1000 -> invoice flow
```

Or step by step:

```sh
go build -o bin/erp ./cmd/erp
./bin/erp gen-mock-sap                       # deterministic mock SAP extract
./bin/erp migrate -company 1000 -year 2026   # quality gate, load, reconcile
./bin/erp migrate -company 2000              # next scope, same store
./bin/erp invoices                           # rules post clean matches, the rest go to review
./bin/erp invoices -agent                    # same, with the Claude agent writing recommendations
./bin/erp tasks                              # human review queue
./bin/erp approve -task TASK-00000003 -as controller -comment "surcharge agreed"
./bin/erp tb -company 1000                   # trial balance
./bin/erp audit-verify                       # check the tamper-evident audit chain
```

On-demand analytics (see [docs/analytics.md](docs/analytics.md) for the two-year setup):

```sh
./bin/erp metrics                            # governed metrics
./bin/erp query -measures revenue -by fiscal_year,fiscal_month -where "fiscal_month <= 9" -companies 1000
./bin/erp report -file examples/monthly-sales.json -companies 1000   # example dashboard, no API key needed
./bin/erp ask -as sales.director -companies 1000 "monthly sales vs last year this time"
```

`make demo-analytics` runs the whole analytics walkthrough on mock data.

### Phase 1: configuration as code and procure-to-pay

Users come from the active configuration (`-as <user id>`); the mock SAP role
design provides `buyer1`, `purchmgr1`, `whclerk1`, `mdclerk1`, `mdmgr1`,
`treas1`, `treasmgr1`, `owner1`, `controller1` and `architect1`.

```sh
# Configuration migrated from SAP customizing. The mock's role design has an
# SoD conflict (super1: invoice posting + payment release) that must be fixed
# in the draft before bootstrap.
./bin/erp config generate -company 1000,2000 -out draft.yaml
./bin/erp config bootstrap -file fixed.yaml -as architect1

# A change: proposed, simulated, regression-tested, approved by tier, activated.
./bin/erp regress capture
./bin/erp config propose -file change.yaml -title "Raise auto-post limit" -as analyst1
./bin/erp config approve -cr CR-0001 -as owner1
./bin/erp config approve -cr CR-0001 -as controller1
./bin/erp config activate -cr CR-0001 -as architect1
./bin/erp config rollback -to 1 -as architect1 -reason "..."

# Procure-to-pay.
./bin/erp req create -as buyer1 -vendor 100001 -line "Paper;10;25.00;520000;MAT-P"
./bin/erp req approve -id PR-... -as purchmgr1
./bin/erp po create -req PR-... -as buyer1            # PO output in data/outbox
./bin/erp gr post -po PO-... -line 10 -qty 10 -as whclerk1
./bin/erp vendor bank -as mdclerk1 -id 100001 -bank GB82WEST12345698765432 -reason "letter"
./bin/erp vendor approve -change MDC-... -as mdmgr1
./bin/erp pay propose -as treas1 -company 1000 -date 2027-06-30
./bin/erp pay release -run PAY-... -as treasmgr1      # pain.001 in data/bankfiles
./bin/erp capture -file invoice.pdf                   # Claude reads the PDF

# Delivery agents (need Anthropic credentials).
./bin/erp agent config "raise the price tolerance in company 1000 to 3%"
./bin/erp agent migration -company 1000 -year 2026
./bin/erp agent test -cr CR-0001
```

Against a real S/4HANA system:

```sh
SAP_USER=... SAP_PASSWORD=... SAP_CLIENT=100 \
  ./bin/erp migrate -source odata:https://my-s4.example.com -company 1000 -year 2026
```

`-agent` needs Anthropic credentials (`ANTHROPIC_API_KEY`, or an `ant auth login`
profile). The default model is `claude-opus-5-5`; override it with `ERP_AGENT_MODEL`.

## Layout

| Package | Role (white-paper layer) |
|---|---|
| `internal/sap` | SAP table structs (`ACDOCA`, `LFA1`, `EKKO`, `RBKP`, ...), file and OData V2 sources, mock generator. *Composable Enterprise Platform* |
| `internal/migration` | Data-quality gate, SAP→core mapping, load, independent reconciliation. Two modes: `documents` (replay every journal entry) and `balances` (opening balances plus open items) |
| `internal/core` | Native system of record: double-entry ledger, vendors, POs, goods receipts, supplier invoices, deterministic three-way match. *System of Record* |
| `internal/governance` | Named identities, autonomy levels L0–L3, segregation of duties, approval thresholds, hash-chained audit log. *Governance & Security* |
| `internal/analytics` | On-demand analytics: governed semantic layer, fact tables, query engine with row-level security, analytics agent, narrative check, dashboard renderer, saved reports |
| `internal/agent` | Composite AI: a rule posts clean invoices within limits; the Claude exception agent investigates the rest and writes a recommendation for a human. *AI Agent + Orchestration* |
| `internal/config` | Configuration as code: typed YAML, validators, change tiers, versioned repository with change requests, approvals, activation and rollback. *Configuration plane* |
| `internal/workflow` | Approval engine shared by requisitions, supplier changes, payment runs and configuration changes |
| `internal/p2p` | Procure-to-pay service: requisitions, contracts, POs and output, goods receipts, governed supplier master changes, payment runs |
| `internal/bankfile` | ISO 20022 pain.001 credit transfer files |
| `internal/capture` | Invoice capture: Claude structured extraction from PDF, deterministic validation |
| `internal/delivery/*` | Delivery track: configuration generated from SAP customizing, sandbox simulation, regression suites, evidence for change requests, and the configuration, migration and test agents |
| `internal/llm` | Shared Claude agent plumbing; `llmtest` is a scripted Messages API for tests |
| `internal/nfr` | Non-functional suites: performance, concurrency, security, reliability |
| `cmd/erp` | CLI |

**Platform foundation** (API-first; design in [docs/design](docs/design/), ADR-0014 for the request pipeline):

| Package | Role |
|---|---|
| `api/openapi.yaml`, `api/asyncapi.yaml` | The API and event contracts; `api/gen` is the server generated from the API contract by ogen (`make generate`) |
| `db/migrations` | The data model as versioned PostgreSQL migrations, applied by the embedded runner (`db.Migrate`) |
| `internal/platform/*` | Request pipeline: `telemetry`, `authn` (OIDC), `uow` (transaction, tenant and actor, idempotency, audit flush), `authz` (contract permissions), `problem` (RFC 9457), `audit` (hash chain), `outbox` (writer and relay), `natsbus` (JetStream), `pagination`, `etag`, `config`, `httpapi` (wiring) |
| `internal/operations` | Long-running operations: the first context on the foundation (`/v1/operations`) |
| `cmd/erp-server` | The API server: public listener, admin listener (`/livez`, `/readyz`, `/metrics`), outbox relay |

## Guardrails built in

- **Money is never a float.** Amounts are integer minor units; quantities are integer milli-units.
- **The ledger validates everything**, including migrated SAP documents: balanced, known accounts, vendor on AP lines.
- **Agents cannot post exceptions.** Only a clean three-way match can be posted by a non-human, and only within the auto-post limit (default 50,000.00).
- **The exception agent has read-only and escalate tools only.** It cannot post, pay or change master data. Bank details are masked in what it sees, and vendor-supplied text is treated as untrusted data.
- **Segregation of duties** applies to agents: no identity can both post invoices and release payments.
- **Every decision is audited** in an append-only, SHA-256-chained log. `audit-verify` detects edits and deletions.
- **Reconciliation recomputes the SAP side from the raw extract**, so a transform bug shows up as a difference instead of being copied into both sides.

## Running the API server

```sh
make db-up                      # PostgreSQL 17 in Docker on :55432
export ERP_OIDC_ISSUERS='[{"issuer":"https://your-idp.example.com","audience":"erp-api"}]'
export ERP_PAGE_TOKEN_KEY=$(head -c 32 /dev/urandom | base64)
export ERP_NATS_URL=nats://localhost:4222   # optional; without it events wait in the outbox
make run-server                 # migrates, then serves :8080 (API) and :9090 (admin)
```

All settings are environment variables, documented in
[`internal/platform/config`](internal/platform/config/config.go). Tokens must
carry the tenant id in the `tenant_id` claim, and the subject must be a
provisioned principal of that tenant. Every operation in the contract is
served: implemented ones run, the rest answer `501` until their context moves
onto the foundation ([docs/design/07-transition.md](docs/design/07-transition.md)).

## Status

Phases 0 and 1 are built. Phase 1 delivers configuration as code with the
configuration, migration and test agents, sandbox simulation and regression
suites, and procure-to-pay (requisitions with configured approvals,
contracts, POs with output, goods receipts, governed supplier changes,
payment runs with ISO 20022 files, invoice capture). On-demand analytics is
built ahead of its Phase 2 slot. The Phase 1 exit criteria that need a client
(a pilot company code cut over, measured consultant hours) cannot be met on
mock data. See [docs/plan.md](docs/plan.md) and [docs/testing.md](docs/testing.md).
Known gaps:

- The Phase 1 prototype still stores data in memory with JSON snapshots. The
  PostgreSQL foundation and the API server (T1) are built; contexts move onto
  them one at a time (T2 onwards).
- Single currency per company code. No FX revaluation yet.
- The OData entity and field mappings follow SAP API Business Hub names but have
  not yet been validated against a live system. Check them against your `$metadata`.
- Agents are tested against a scripted model; their behavior with the live API
  needs evals before any agent gets autonomy above L0.
- The CLI keeps state in files; two CLI processes writing at once are not coordinated.
- No AR, inventory valuation, tax engine or payroll yet (later phases).
