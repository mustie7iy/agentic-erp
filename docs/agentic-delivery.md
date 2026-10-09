# Agentic delivery: replacing consultant effort

One of the platform's two core objectives is to cut SAP consultant and
engineering cost. On an SAP program most of the budget goes to people
configuring customizing in SPRO, writing mapping specs, testing, building
interfaces, maintaining Z-code and working support tickets. Each of those jobs
becomes an agent that works against configuration as code, with a
deterministic check and a human approval before anything goes live.

## Roles and agents

| SAP role today | Agent | What the agent does | What stays human |
|---|---|---|---|
| Functional consultant (FI, CO, MM, SD, PP) | Configuration agent | Generates configuration from interviews, policy documents and the client's existing SAP customizing; explains every setting; simulates the effect before activation | Process owner approves; solution architect reviews posting-relevant changes |
| Business analyst / blueprint lead | Discovery agent | Mines SAP change logs, document flows and transaction usage to show how the business actually runs; produces scope and fit-gap; flags unused functionality | Business leads confirm scope and target processes |
| Data migration consultant | Migration agent | Proposes field mappings with confidence scores, drafts fixes for data-quality findings, runs rehearsals, explains every reconciliation difference down to the document | Controllers sign off reconciliation; data owners approve cleansing |
| ABAP developer | Custom-code agent | Inventories custom code, drops what usage data shows is never run, maps the rest to configuration, rules or a Go extension, and writes tests for each | Engineer reviews generated extensions |
| Integration consultant | Integration agent | Inventories IDoc partner profiles and RFC destinations, generates connector configuration and mappings, monitors flows and replays failures | Approves new external connections and credentials |
| Security / GRC consultant | Access agent | Derives least-privilege roles from actual usage, runs SoD analysis on every role change, prepares periodic access reviews | Managers certify access; compliance owns the SoD rule set |
| Test consultant | Test agent | Builds regression suites from real historical transactions and replays them on every configuration change; writes UAT scripts | Key users run UAT and accept |
| Basis / operations | Operations agent | Watches health and capacity, triages incidents with runbooks, rolls out releases continuously so there are no upgrade projects | On-call engineer for production incidents |
| AMS support (L1–L3) | Support agent | Answers how-to questions, finds root cause from the audit trail and logs, proposes the fix as a configuration change request | Approves the change request |
| Localization consultant | Regulatory agent | Monitors legal and tax changes per country, drafts configuration updates and statutory report changes | Tax and legal experts sign off |
| Trainer / change manager | Enablement agent | Writes process docs and training directly from live configuration; answers user questions in context | Change lead owns communication |
| Reporting consultant / BI developer | Analytics agent | Builds any report or dashboard on request from the governed semantic layer, explains the numbers, and saves or schedules it. See [analytics.md](analytics.md) | Finance and business owners define the metrics |

## Configuration as code

Configuration is typed files in Git (YAML validated against Go types, with CUE
constraints) and is never edited directly in production. The configuration,
migration, support and regulatory agents all use the same loop:

1. **Request**: a person asks, a ticket arrives, or the regulatory agent detects a change.
2. **Propose**: the agent writes a configuration change with an explanation of each setting.
3. **Validate**: deterministic checks for schema, consistency, segregation of duties and complete account determination.
4. **Simulate**: the change is applied to a sandbox clone and recent periods are replayed; differences in postings are shown.
5. **Approve**: the owner approves; posting-relevant changes need a second approver.
6. **Activate**: a versioned release with an audit entry and one-step rollback.

### Change tiers

| Tier | Examples | Who approves |
|---|---|---|
| 1 Cosmetic | Texts, layouts, notification wording | Agent applies after validation |
| 2 Operational | Approval routes, tolerances, payment terms | Process owner |
| 3 Posting-relevant | Account determination, tax codes, valuation, number ranges | Two approvers, with simulation evidence |
| 4 Structural | Company codes, ledgers, chart of accounts, fiscal year | Solution architect and controller, planned cutover only |

## Migrating configuration, not only data

The configuration agent reads the client's own SAP customizing and generates
the equivalent configuration, so nobody re-keys years of setup by hand.

| Area | SAP sources |
|---|---|
| Enterprise structure | T001 company codes, T001W plants, TVKO sales orgs, T024E purchasing orgs, T001L storage locations |
| Finance | SKA1/SKB1 chart of accounts, T030 account determination, T052 payment terms, T007A tax codes, T043G tolerances, NRIV number ranges |
| Logistics | T156 movement types, T161 PO types, T169G invoice tolerances, T134 material types, T001K valuation areas |
| Sales | TVAK order types, T683 pricing procedures, T685 condition types, TVFK billing types, TVLK delivery types |
| Security | AGR_DEFINE roles, AGR_1251 authorizations, AGR_USERS assignments, USR02 users |
| Custom code and usage | TADIR Z-objects, ABAP source via abapGit or ADT, ST03N/STAD transaction usage, CDHDR/CDPOS change logs |

## How the saving is measured

Measured per scope against the client's current SAP spend and partner quotes,
not promised up front.

- **Consultant hours per scope**: hours to configure, migrate and cut over one company code, against the systems integrator's quote for the same scope.
- **Request to production**: time from a change request to activated configuration.
- **Tickets resolved by agents**: share of support tickets closed without a consultant.
- **Reconciliation rehearsals**: migration cycles needed to reach zero differences.
- **Upgrade effort**: engineering days spent on releases (target: no upgrade projects).
- **Agent cost**: model spend per scope, per change and per ticket, tracked by the model gateway.
