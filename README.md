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

## Quick start

```sh
make test     # vet + all tests with the race detector
make demo     # mock SAP -> migrate company 1000 -> invoice flow
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
| `internal/agent` | Composite AI: a rule posts clean invoices within limits; the Claude exception agent investigates the rest and writes a recommendation for a human. *AI Agent + Orchestration* |
| `cmd/erp` | CLI |

## Guardrails built in

- **Money is never a float.** Amounts are integer minor units; quantities are integer milli-units.
- **The ledger validates everything**, including migrated SAP documents: balanced, known accounts, vendor on AP lines.
- **Agents cannot post exceptions.** Only a clean three-way match can be posted by a non-human, and only within the auto-post limit (default 50,000.00).
- **The exception agent has read-only and escalate tools only.** It cannot post, pay or change master data. Bank details are masked in what it sees, and vendor-supplied text is treated as untrusted data.
- **Segregation of duties** applies to agents: no identity can both post invoices and release payments.
- **Every decision is audited** in an append-only, SHA-256-chained log. `audit-verify` detects edits and deletions.
- **Reconciliation recomputes the SAP side from the raw extract**, so a transform bug shows up as a difference instead of being copied into both sides.

## Status

Phase 0 is complete and Phase 1 is partly built: a working vertical slice for
accounts payable and the general ledger, with deterministic migration and
reconciliation. None of the delivery agents (configuration, migration,
discovery, test and the rest) exist yet; the first of them are next in Phase 1.
The analytics agent follows in Phase 2. See [docs/plan.md](docs/plan.md).
Known gaps:

- Storage is an in-memory store with JSON snapshots. Postgres comes next.
- Single currency per company code. No FX revaluation yet.
- The OData entity and field mappings follow SAP API Business Hub names but have
  not yet been validated against a live system. Check them against your `$metadata`.
- No payment run, AR, inventory valuation, tax engine or payroll yet.
