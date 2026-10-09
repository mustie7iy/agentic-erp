# Implementation plan

Based on Rimini Street, *The Rise of Agentic AI ERP* (rev. US-07012026),
adapted to two objectives:

1. **Replace SAP's capabilities** scope by scope: every capability in the
   [capability map](capability-map.md) is built in the native core, rebuilt
   with agents, or integrated with a specialist product.
2. **Cut consultant and engineering cost**: agents configure the system,
   migrate it from SAP and run it day to day; people approve and own
   decisions. See [agentic-delivery.md](agentic-delivery.md).
3. **Answer any question on demand**: no catalogue of static reports. People
   ask in plain language and the analytics agent builds the report or
   dashboard on the spot from governed data. See [analytics.md](analytics.md).

Architecture and tech stack: [architecture.md](architecture.md). Migration
protocol: [migration.md](migration.md).

## Roadmap: two tracks

Business modules and delivery agents ship together. The migration and
configuration agents come first, because every later module is configured and
migrated by them.

| Phase | Business track | Delivery track | Status |
|---|---|---|---|
| 0 Foundation | General ledger core, migration engine (extract, quality gate, load, reconcile), governance and audit chain | Deterministic migration and reconciliation; no delivery agents yet | Done |
| 1 Procure-to-Pay and platform basics | Purchasing, contracts, goods receipt, invoice verification (done), invoice capture, payments, supplier master, MDG workflows, org structure, workflow, output, user roles | Migration agent; configuration agent with the configuration-as-code engine, validators and sandbox simulation; test agent regression replay | Built (exit criteria need a pilot client) |
| 2 Record-to-Report | Asset accounting, banking and cash, close, consolidation, parallel ledgers, FX, controlling (cost and profit centers, CO-PA), tax, e-invoicing, extensibility; semantic layer, analytical store and on-demand analytics for finance and procurement | Analytics agent, discovery agent, custom-code agent, access agent, regulatory agent | Analytics slice built early; rest planned |
| 3 Order-to-Cash | Customer master, quotations and orders, pricing, billing, rebates, returns, AR, credit, collections and disputes, revenue recognition, order intake, CRM integration; sales metrics in the semantic layer; proactive anomaly alerts | Integration agent, support agent (first line), enablement agent | Planned |
| 4 Supply chain | Material master, inventory and valuation, batches and serials, shipping, ATP, MRP, demand planning and S&OP, warehouse, transport and trade integrations, sourcing and supplier network | Operations agent: monitoring, incident triage, continuous releases | Planned |
| 5 Manufacturing and quality | BOMs and routings, planning and capacity, discrete, process and repetitive production, variant configuration, subcontracting, quality management, product costing, MES integration | Configuration and test agents extended to manufacturing customizing | Planned |
| 6 Assets, service and EHS | Plant maintenance, predictive maintenance, mobile maintenance, field service, real estate and lease accounting, EHS incidents, product compliance | Support agent moves to L2/L3 with configuration fixes | Planned |
| 7 Projects and product lifecycle | Project system, portfolio management, engineering change, document management, recipe development, subscription billing | Configuration agent extended to project and PLM customizing | Planned |
| 8 Human capital | Core HR and org management, time and attendance, workforce scheduling; payroll, talent, travel and expense, contingent workforce through integrations | Access agent covers HR data privacy controls | Planned |
| 9 Analytics, GRC and sustainability | Planning and budgeting, forecasting and what-if analysis, spend analytics, process controls, risk management, data privacy, archiving, ESG reporting | All delivery agents in steady state; autonomy raised where evidence supports it | Planned |
| 10 Industry packs | Industry configuration packs on demand, starting with the first client's industry | Configuration agent generates packs from the client's SAP industry customizing | Planned |
| 11 Decommission SAP | SAP read-only for audit, then archived and switched off | | Planned |

### Phase 1 delivered

| Track | Built |
|---|---|
| Delivery | Configuration as code (`internal/config`): strict typed YAML, deterministic validators, change tiers 1–4, versioned repository with change requests, evidence gates, tiered human approvals, activation, one-step rollback, tamper detection |
| Delivery | Configuration generated from SAP customizing (T001, SKB1, T030, T007A, T169G, T012K, T052, AGR_DEFINE/TCODES/USERS) with reviewer notes |
| Delivery | Sandbox simulation (replay invoices under current and proposed configuration, decision and ledger impact) and regression suites captured from real transactions |
| Delivery | Configuration agent, migration agent (account classification with confidence, sandbox rehearsals, document-level difference explanations, cleansing proposals) and test agent (suites, UAT scripts) |
| Business | Requisitions with configured approval tiers, contracts, purchase orders with email/EDI (X12 850) output, goods receipts and reversals with over-delivery tolerance |
| Business | Governed supplier master changes (create, bank change, unblock) with approval; immediate block; payment holds after bank changes |
| Business | Payment runs: proposal with exclusions, release by a separate role, all-or-nothing posting and clearing, ISO 20022 pain.001 files |
| Business | Invoice capture from PDF with Claude structured outputs and deterministic validation |
| Platform | Users and roles from configuration; SoD enforced at validation and at run time; approval workflow engine; idempotent migration reruns |
| Quality | Functional, end-to-end, performance, concurrency, security, reliability and fuzz suites in CI with a coverage gate; see [testing.md](testing.md) |

### Phase 1 exit criteria

- One pilot company code cut over through the protocol in [migration.md](migration.md).
- At least 60% of supplier invoices posted with no human touch; no unauthorized postings.
- Pilot company code configured by the configuration agent from the client's SAP customizing, with every change through the approval loop.
- Documented consultant hours and agent cost for the pilot, against the integrator's quote for the same scope.

## Agent autonomy ladder

Business and delivery agents start at L0 (suggest only). An agent moves up one
level only with eval results plus production evidence (override rate, error
rate) for at least one period, signed off by the process owner. Human
corrections become eval cases. Agents never change their own prompts or
policies.

## Risks

| Risk | Mitigation |
|---|---|
| Migrated balances wrong | Independent reconciliation; cutover blocked on any difference |
| Agent posts wrong data | Agents cannot post exceptions; the ledger validates every entry; thresholds |
| Agent misconfigures the system | Configuration as code, deterministic validators, sandbox replay, change tiers with human approval, one-step rollback |
| Analytics agent invents or misstates numbers | Numbers computed only by the analytical store; commentary validator; governed metric definitions; every answer shows its query |
| Prompt injection via vendor documents | Vendor text treated as data; business agents have no write tools except escalate |
| Fraud via bank-detail change | Always a human action; bank data masked from agents |
| SAP customizing or ABAP not exportable | Confirm access to customizing tables, ABAP source and usage statistics during discovery |
| SAP licensing | Read-only integration; contract review before the pilot |
| Scope too broad | Capabilities move only in phase order and only for licensed, used scope found by discovery |
| Pilot never ends | Hard exit criteria per phase |
