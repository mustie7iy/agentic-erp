# Implementation plan

Based on Rimini Street, *The Rise of Agentic AI ERP* (rev. US-07012026), adapted
to a migration goal: the agentic layer runs **on our own core** rather than
permanently on top of SAP, and SAP is retired scope by scope.

## Reference architecture → this repo

| White-paper layer | Implementation | Status |
|---|---|---|
| Intelligent Persona Experience | CLI review queue today; web command center later | CLI done |
| AI Agent Layer | `internal/agent`: Claude exception agent (tool runner, read and escalate tools only) | AP exceptions done |
| Orchestration Layer | `agent.ProcessInvoices` composite pipeline; durable workflows (Temporal) later | Basic |
| Federated Data Fabric | `sap.Source`: file and OData V2; CDC later | Read path done |
| Composable Enterprise Platform | `internal/core` native services: ledger, AP, purchasing | AP/GL done |
| Governance & Security | `internal/governance`: identities, SoD, autonomy levels, thresholds, hash-chained audit | Done |

## Phases

**Phase 0: foundations (done).** Core ledger, SAP extract model, data-quality
gate, migration modes, reconciliation, governance, audit trail, CLI, tests.

**Phase 1: Procure-to-Pay pilot (in progress).**
- Done: three-way match, rule-based posting, Claude exception agent, human approval.
- Next: Postgres storage; payment proposals with segregation of duties (a separate
  identity releases payments); vendor master change workflow (bank changes always
  go to a human); invoice intake from PDF and email using Claude document input.
- Exit criteria: one pilot company code cut over; at least 60% of invoices
  posted with no human touch; no unauthorized postings; documented cost per invoice.

**Phase 2: Record-to-Report.** Period close, accruals, reconciliations, close
agent with variance commentary, financial statements, multi-currency and FX revaluation.

**Phase 3: Order-to-Cash.** Customers, sales orders, billing, cash application,
collections agent, AR migration (KNA1/KNB1/BSID equivalents from ACDOCA).

**Phase 4: Supply chain.** Inventory (MARA/MARD/MBEW), valuation, MRP-lite
replenishment agent, warehouse integration.

**Phase 5: decommission.** All scopes migrated, SAP set to read-only, then archived.

## Agent autonomy ladder

Agents start at L0 (suggest only). An agent moves up one level only when it
has eval results plus production evidence (override rate, error rate) for at
least one period, signed off by the process owner. Human corrections become
eval cases. Agents never change their own prompts or policies.

## Risks

| Risk | Mitigation |
|---|---|
| Migrated balances wrong | Independent reconciliation; cutover blocked on any difference |
| Agent posts wrong data | Agents cannot post exceptions; the ledger validates every entry; thresholds |
| Prompt injection via vendor documents | Vendor text treated as data; the agent has no write tools except escalate |
| Fraud via bank-detail change | Always a human action; bank data masked from agents |
| SAP licensing | Read-only integration; contract review before the pilot |
| Pilot never ends | Hard exit criteria per phase |
