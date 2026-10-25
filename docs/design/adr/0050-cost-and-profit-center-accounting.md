# ADR-0050: Cost and profit center accounting

**Status:** accepted (T56, product owner 2026-10-21: P1-P8 as recommended,
see "Decisions taken"). The steps T56a-T56l are in the tracker.

## Context
T56 is "Controlling I: cost and profit centres" (07 §7.6): "cost and profit
centre master data and hierarchies; nullable cost and profit centre columns
on `journal_line` with derivation; plan against actual and the centre
reports. Lane F." The pilots are a Canadian and a UAE company. Today the ERP
has no account assignment below the G/L account:

- **The journal line** (`ledger.journal_line`, 000005) carries the legal
  entity, ledger, period, account, the three amounts, partner, tax code,
  quantity and text; T33 added `purchase_order_id`/`po_item_no` for GR/IR
  and T46 `special_gl`. The table already reserves the place: "Controlling
  dimensions (Phase 2) are added here as nullable columns, keeping ledger and
  controlling one table as in SAP's universal journal" (03 §3.16 says the
  same for cost center, profit center, internal order and WBS element).
  There is no cost center, profit center, segment or functional area
  anywhere in the code; `ledger.PostingLine` and `ledger.Line` have none.
- **Accounts** (`ledger.gl_account`) have a type (`asset | liability |
  equity | revenue | expense`, `is_balance_sheet` generated) and a
  reconciliation kind; nothing says whether an expense account needs a cost
  object. The configuration document's `chart_of_accounts` carries the same
  fields (`config.Account`).
- **Procurement and payables** assign costs by account only: requisition
  and purchase order items have a mandatory `account_id`, supplier invoice
  items an `account_id` when they are not PO-based. Goods receipts post the
  PO item's account against GR/IR (T33); price differences go to one
  determined account (`account_determination.price_difference`).
- **Balances** are projected per ledger, company, account, year, period and
  transaction currency (`ledger.period_balance`, ADR-0006); the trial
  balance reads projected balances plus unprojected lines.
- **Custom fields** (ADR-0046) already foresee "a reference to a cost
  object, whose existence is checked on write", refused by the compiler
  "until cost objects exist (T56)". The test fixtures use a free-text custom
  field `cost_center` on invoices and orders, which is what a tenant would do
  today and what T56 replaces with a real field.
- **The SAP extract** (`internal/sap`) reads ACDOCA without `RCNTR`,
  `PRCTR` or `SEGMENT`, and no CO master data.
- **Parallel ledgers** (ADR-0045, on branch t52, not yet in main) write a
  copy of every line per ledger the company keeps, linked by
  `posting_group_id`; reports read one ledger, the leading one by default.

### What SAP does (the parity target)
Sources at the end; where I rely on memory it says so.

- **Cost centers (CO-OM-CCA)** are master data of a controlling area,
  assigned to one company code, with a cost center **category** (`KOSAR`),
  a person responsible, a currency, a profit center and a place in the
  **standard hierarchy**. Fields are time-dependent by validity period
  (`DATAB`/`DATBI` in `CSKS`); the **lock indicators** for actual and plan
  primary costs, secondary costs and revenues are day-based, with defaults
  from the category. A cost center sits in exactly one node of the standard
  hierarchy and in any number of alternative groups; group assignment is
  not time-dependent (a reorganisation restates history in reports).
- **Cost elements are G/L accounts** in S/4HANA: the account master carries
  a G/L account type (primary costs or revenue, secondary costs) and, per
  controlling area, a **cost element category**: 01 primary costs, 11/12
  revenues and sales deductions, 21 settlement, 41-43 overhead and
  allocations (secondary), 90 balance sheet statistical, 66 reporting only.
  A posting to a primary cost element needs exactly one real CO object
  (cost center, order, WBS element, profitability segment); others on the
  same line are statistical. **Default account assignments** (OKB9) supply
  a cost center per company code and cost element when none is entered.
- **The universal journal** (ACDOCA) carries the cost center (`RCNTR`),
  profit center (`PRCTR`), segment (`SEGMENT`), functional area
  (`RFAREA`) and partner profit center on every line; actual costs are
  ACDOCA lines, so CO actuals and the G/L reconcile by construction. Plan
  data is in **ACDOCP**, separated by **plan category** (BUDGET02 is cost
  center budgeting).
- **Profit centers (EC-PCA in S/4)** are master data of the controlling
  area, assigned to one or more company codes (`CEPC_BUKRS`), with a
  segment and a standard hierarchy. The profit center is derived in order:
  from the cost object's master (cost center → profit center), from the
  material and plant, from substitution rules, from document splitting, and
  finally from a **default (dummy) profit center**. The segment is derived
  from the profit center's master; changing it after postings needs a
  reorganisation.
- **Document splitting** gives the lines that have no cost object
  (payables, receivables, tax, cash) a profit center and segment, by
  splitting them in proportion to the base lines, inheriting on clearing,
  and balancing each document to zero per characteristic through
  zero-balance clearing lines. It is what makes a balance sheet per profit
  center or segment possible. Introducing it in a running system is a
  migration project at a fiscal year start (as far as I know; SAP calls it
  the subsequent implementation of document splitting).
- **Purchasing**: an account assignment category (K = cost center) on the
  requisition and PO item says which assignment is required; the account
  assignment is in `EKKN`/`EBKN`; goods receipt and invoice post to it; a
  PO item may carry several assignments split by percentage or quantity.

### Segment reporting (IFRS 8)
IFRS 8 applies to entities whose debt or equity is traded in a public market
(or that are filing to issue it), and to others that choose it. Operating
segments are what the chief operating decision maker reviews; a segment is
reportable at 10% of combined revenue, of the greater of combined profit or
loss, or of combined assets, and reportable segments must cover 75% of
external revenue. Segment assets and liabilities are disclosed only if they
are regularly given to the CODM. The pilots' listing status is not known to
me; that decides how much of this is needed (facts at the end).

## Decision

### 1. Scope of T56
In: cost centers, profit centers and segments as master data with validity
periods, company assignment, categories and lock indicators; standard and
alternative hierarchies; a **cost element category** on every account; cost
center, profit center and segment columns on every journal line, with
validation and derivation at posting; defaults for system postings;
account assignment on requisition, purchase order and supplier invoice
items; actual reports by center, hierarchy node, account and period; plan
values per center, account and period with plan against actual (decision
P5); the SAP migration of the master data and of the assignments on
migrated documents; agents proposing assignments; the custom-field cost
object reference of ADR-0046.

Out: internal orders, statistical orders, assessments, distributions,
overhead rates, activity types and activity allocation, settlement (T60);
margin and profitability analysis (T60 for the G/L-based margin, phase 3 for
profitability segments with sales); document splitting (decision P3);
functional area and cost-of-sales P&L (a later step, if the pilot presents
expenses by function); budget availability control; planning functions
(top-down distribution, copy with factors, versions with workflow: phase 9
planning); authorisation per center (decision P8); WBS elements (phase 7).

### 2. Ownership: the ledger owns the assignment objects
**Cost centers, profit centers, segments and their hierarchies live in the
ledger context (schema `ledger`)**, beside the chart of accounts; plan
values and the plan/actual report go to a new **`controlling`** context
(schema `controlling`) that depends on the ledger, which T60 extends with
orders and cycles.

Reasons:
- They are dimensions of the journal, read in every posting transaction
  and needed by the journal's own invariants (an expense line has a cost
  center; the cost center belongs to the line's company). In the ledger's
  schema these are foreign keys and commit-time checks the database holds
  (CLAUDE.md: a schema test for every database-enforced rule); across
  schemas they would be application checks through a port, as partner
  blocks are (`ledger.Partners`), with no foreign key.
- No new dependency direction: procurement and payables already depend on
  the ledger, and validate account assignment through it as they validate
  accounts today.
- It is SAP's universal-journal shape: one table, CO objects on its lines.
- Internal orders (T60) differ: they have a lifecycle (release, budget,
  settlement, close) that is controlling's. T60's ADR decides whether the
  order master is controlling's behind a ledger port (`ledger.CostObjects`,
  like `Partners`) or the ledger's; this ADR's columns and checks are
  written so a second cost object slots in (§5: "exactly one real cost
  object").

### 3. Master data
```
ledger.segment              (tenant_id, id, code, name, description)                  UNIQUE (tenant_id, code)
ledger.profit_center        (tenant_id, id, code, segment_id, name, description,
                             responsible, status)                                    UNIQUE (tenant_id, code) unless rejected
ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id)           insert-only
ledger.profit_center_period (tenant_id, id, profit_center_id, valid_from, valid_to,
                             locked, change_id, retired_by_change_id)                EXCLUDE overlapping active validity
ledger.cost_center          (tenant_id, id, legal_entity_id, code, name, description,
                             responsible, status)                                    UNIQUE (tenant_id, legal_entity_id, code) unless rejected
ledger.cost_center_period   (tenant_id, id, cost_center_id, legal_entity_id, valid_from, valid_to,
                             category, profit_center_id, lock_actual_cost,
                             lock_actual_revenue, lock_plan, change_id,
                             retired_by_change_id)                                   EXCLUDE overlapping active validity
ledger.center_change        (tenant_id, id, cost_center_id | profit_center_id, kind,
                             payload, reason, status, workflow_request_id, ...)     one pending per center
```
As built in T56a (migration 000128):
- **Texts on the center, attributes in periods.** Name, description and
  person responsible are on the center and change at once without approval
  (decision P1), their history in the audit trail; what a posting reads
  (category, profit center, locks; a profit center's lock) is in the
  effective-dated periods. `valid_to` is exclusive and NULL open-ended, as
  everywhere (03 §3.1). The person responsible is a name (SAP `VERAK`), not
  a principal; a principal reference can be added when per-person access
  (P8 b) is wanted.
- **Periods are written once.** A change computes the center's periods
  after it, retires (`retired_by_change_id`) the periods that are not kept
  and inserts the new ones; a trigger refuses any other update of a
  period. So a period a posting read is never edited, every earlier state
  is kept, and the posting's `FOR SHARE` on the period it reads (T56d)
  meets the change's update of the same row.
- **Statuses.** A center is `pending` from its create request, `active`
  when a controller approves it, `rejected` when the create is rejected or
  withdrawn (its code is then free); a trigger allows nothing else, and
  keeps code and a cost center's company fixed.
- **Database rules.** A cost center period names the cost center with its
  legal entity (foreign key to `(tenant_id, id, legal_entity_id)`) and a
  profit center assigned to that legal entity (foreign key to
  `profit_center_company`); assignments are never removed. Application
  checks add that the profit center is active and valid over the period
  (422 `ledger.profit_center_invalid` with the reason), and that a profit
  center's new validity still covers the cost center periods naming it
  (409 `ledger.profit_center_in_use`); the cost center change reads the
  profit center `FOR SHARE`, its redate takes it `FOR UPDATE`.
- **Categories** are a code (`^[0-9A-Z_]{1,10}$`), checked since T56c
  against `controlling.cost_center_categories` (§4 as built).
- **In use.** The refusals that need postings (`cost_center_period_in_use`,
  `cost_center_posted_after`, `segment_in_use`) come with T56d2, now that
  lines carry the centers (T56d); until then a center change is not refused
  for its postings.
  A change joins neighbouring periods that end up alike, so a period whose
  attributes did not change may still be retired and re-inserted; T56d's
  in-use check therefore compares the attributes in force on each posted
  date, not period rows.

- **Codes**: `^[0-9A-Z_-]{1,10}$`, as SAP's `KOSTL`/`PRCTR` (10 characters,
  migrated with leading zeros stripped by the existing ALPHA rule).
- **Company assignment.** A cost center belongs to one company, fixed
  (SAP's `CSKS-BUKRS`; a changed company is a new cost center). A profit
  center and a segment are tenant-wide (the tenant is the controlling area);
  a profit center is assigned to the companies it may be posted in. A cost
  center's profit center must be assigned to the cost center's company.
  Cross-company totals are in group currency only (`amount_gc`).
- **Validity.** The first period's `valid_from` is the center's start;
  attributes change by a new period from a date (SAP's time-dependent
  fields), never by editing a period already posted against (409
  `ledger.cost_center_period_in_use`, a new period is added). Ending a
  center's validity before its last posting is refused (409
  `ledger.cost_center_posted_after`). Centers are never deleted (03 §3.1).
- **Categories** come from the configuration (§4): a category
  (`administration`, `production`, `sales`, `service`, `management`... as the
  tenant names them) supplies the lock defaults and whether revenue may be
  posted (SAP's category settings).
- **Locks** (day-based, on the period): actual primary costs, actual
  revenue, plan. Secondary-cost locks come with T60.
- **Segment from profit center.** A profit center names its segment once;
  changing it after the profit center was posted is refused (409
  `ledger.segment_in_use`): a segment move is a reorganisation with
  reposting, not an attribute change (SAP).
- **Hierarchies.** `ledger.hierarchy` (kind `cost_center | profit_center`,
  code, `is_standard`), `ledger.hierarchy_node` (parent, code, name, order)
  and `ledger.hierarchy_member` (node, center). One standard hierarchy per
  kind per tenant; every center is in exactly one leaf node of it (a
  commit-time check), and in any number of alternative hierarchies, at most
  once each. Nodes form a tree (no cycles, checked on write; depth at most
  12). Hierarchies are not time-dependent: reports show history under
  today's tree, as SAP does (decision P7).

  As built in T56b (migration 000139):
  - **Tables.** `hierarchy` (kind, code `^[0-9A-Z_-]{1,15}$` unique per
    kind, `is_standard` with at most one standard per kind, all three
    fixed), `hierarchy_node` (parent in the same hierarchy by foreign key,
    code unique among the hierarchy's nodes, `position` among siblings) and
    `hierarchy_member` (the center by kind: the member names its hierarchy
    with its kind, so a cost center cannot sit in a profit center tree).
    Nothing is deleted (000011): a removed node or place carries
    `removed_by_change_id`, and is then fixed.
  - **Database rules.** Triggers lock the hierarchy row and refuse a
    cycle, a 13th level, a node below a node holding centers, a center at
    a node with children or at a removed node, removing a node with
    children or centers, moving or removing the root, and changing a
    place's center or hierarchy. A deferred constraint trigger checks at
    commit that every active center sits in the standard hierarchy of its
    kind (on the center becoming active and on a standard place moving or
    being removed). Schema tests for each (`db/hierarchies_test.go`).
  - **Change control.** Every change to a tree (`node_create`,
    `node_rename`, `node_move`, `node_remove`, `place`) is a
    `center_change` naming the hierarchy (and, for `place`, the center: one
    pending change per center still holds), with the same approval request,
    controller and re-planning at approval as T56a's changes; the
    hierarchy row is locked from planning to writing, after the center, in
    both the request and the decision. This includes alternative
    hierarchies, which §11's table lists as needing no approval: one path
    for every tree change lets every such operation be `x-autonomy:
    propose` (agents propose, never change a tree), and an alternative
    tree feeds reports. Relaxing it is a policy step for the PO. Creating a
    hierarchy (empty, with its root) needs no approval.
  - **New centers.** `POST /v1/cost-centers` and `/v1/profit-centers` take
    an optional `standard_node_id`, a leaf of the tenant's standard
    hierarchy of the kind, checked at the request and again at approval,
    when the center is placed there. Without one (product owner
    2026-10-21: no breaking change to the create operations) the center
    is placed, when its creation is approved, in the standard hierarchy's
    `UNASSIGNED` leaf, for a controller to place later: the approval adds
    that leaf below the root when the tree has none, and creates the
    standard hierarchy with it when the tenant has none, as 000139 does
    for older centers. Proposing a center (`x-autonomy: propose`) so
    changes no tree; the approver's decision does, and an empty node
    changes no center. Approvals of two first centers are serialized per
    tenant and kind (an advisory lock): the second finds the hierarchy
    and leaf the first created; a standard hierarchy created through `POST
    /v1/hierarchies` at the same moment can still win, and the approval
    then gets 409 `ledger.standard_hierarchy_exists` and succeeds when
    approved again. An `UNASSIGNED` node with child nodes, a root that is
    still a leaf holding centers or is itself coded `UNASSIGNED`, or
    another hierarchy of the kind coded `STANDARD` where the tenant has no
    standard one refuses the approval (422
    `ledger.hierarchy_change_invalid`); the approver fixes the tree first,
    or rejects the creation for it to be requested again with
    `standard_node_id`.
    Centers that existed before
    000139 (T56a was not released) were placed in a `STANDARD` hierarchy's
    `UNASSIGNED` leaf, created per tenant and kind; a create still pending
    then has no node, and its approval places it in that leaf too.
  - **API.** `GET|POST /v1/hierarchies`, `GET /v1/hierarchies/{id}` (the
    tree in order, each node with its level and centers; the ETag is the
    hierarchy's version, moved on by each applied change), `GET
    /v1/hierarchies/{id}/nodes/{node_id}` (a subtree), `POST
    /v1/hierarchies/{id}/nodes/{node_id}:add|:rename|:move|:remove`,
    `POST /v1/hierarchies/{id}:place` (`node_id` null takes a center out of
    an alternative hierarchy), and `GET /v1/cost-centers/{id}/hierarchy-paths`,
    `/v1/profit-centers/{id}/hierarchy-paths` (root to leaf per
    hierarchy). Hierarchies are addressed by id rather than
    `{kind}/{code}`, as every other resource; the list filters by kind and
    code. A tree lists only the cost centers of companies the caller may act in, as every cost center read; its nodes are tenant-wide. The ETag is the tree's version, so two callers with different access may get different bodies under one ETag: harmless for If-Match (changes are planned against the whole tree), but a shared cache must vary by principal. `/v1/center-changes` filters by `hierarchy_id`. Event
    `HierarchyChanged` says what changed, not the tree; audit
    `ledger.hierarchy.create|request|node_create|node_rename|node_move|node_remove|place`.
- **Default profit center** per company (SAP's dummy profit center), a real
  profit center flagged `is_default`, collects P&L lines nothing else
  derives. Its balance is a close task (T64): it should be empty or
  explained.

### 4. Configuration: categories and rules, not the centers
The configuration document gains (T56c):
```yaml
controlling:
  cost_center_categories:
    - {code: ADMIN, name: Administration, revenue_allowed: false}
    - {code: SALES, name: Sales, revenue_allowed: true}
companies:
  - code: CA01
    profit_center_accounting: true        # P2
    default_profit_center: CA-DUMMY
    chart_of_accounts:
      - {number: "6100", name: Rent, type: expense, cost_element: primary_cost}
      - {number: "8200", name: Income tax expense, type: expense, cost_element: none}
      - {number: "4000", name: Revenue, type: revenue, cost_element: revenue}
    default_assignments:                   # SAP OKB9
      - {account: "7400", cost_center: "1000"}           # cash discount received
      - {account: "7900", cost_center: "1000"}           # FX gains and losses
```
- `cost_element` per account: `none` (no cost object; the only value for
  balance sheet accounts), `primary_cost` (category 01: one cost object
  required), `revenue` (11/12: profit center required, a cost center only
  if its category allows revenue and then statistical); `secondary` values
  are reserved for T60. Default by type: expense → `primary_cost`, revenue
  → `revenue`, balance sheet → `none`; a pack (ADR-0048) may set `none` for
  accounts that are not controlled (income taxes, FX revaluation if the
  tenant wants).
- `default_assignments` give a cost center per company and account (later
  also per amount range or document type, not now). **Activation refuses a
  document in which an account the system posts to automatically
  (`account_determination.*`, `fx_valuation`, bank statement posting rules,
  cash discount, price difference) needs a cost object and has neither a
  default nor a rule deriving one from the document** (`config`
  validation), so a payment run, a revaluation or a statement posting is
  never refused for a missing cost center at run time.
- Tiers: categories and texts cosmetic (1); `cost_element` and
  `default_assignments` posting-related (3, with simulation: the
  simulation replays the period's postings and lists lines that would now
  be refused or reassigned); turning `profit_center_accounting` on for a
  company is structural (4) and allowed only from a fiscal year start or
  for a company with no postings yet in the open year, so a year's P&L by
  profit center is complete.

As built in T56c (migration 000140):
- **Document.** `controlling.cost_center_categories` (`code` as a period's
  category, `name`, `revenue_allowed`, and `lock_actual_cost`,
  `lock_actual_revenue`, `lock_plan`: the locks a new cost center of the
  category starts with, SAP's category defaults); per account
  `cost_element` (`none`, `primary_cost` on an expense only, `revenue` on a
  revenue only; `secondary` is refused until T60; empty takes the type's
  default, `config.Account.CostElementOrDefault`); per company
  `cost_center_accounting`, `profit_center_accounting`,
  `default_profit_center` and `default_assignments` (`{account,
  cost_center}`, one per account, only for an account that needs a cost
  center; by account only: amount ranges, document types or a business
  area are not built).
- **`cost_center_accounting`** is a company switch this section did not
  name: P6's "every `primary_cost` line has a cost center" applies, and
  the activation check below runs, only once it is on, from a fiscal year
  like `profit_center_accounting`. Without it a tenant's first document
  could not activate (a default needs a cost center, a cost center needs a
  category from the document), and every existing document with an
  expense FX loss or price difference account would be refused at its next
  change. PO to confirm.
- **Activation check** (`config.Validate`): with cost center accounting on,
  every account the company's automatic postings use
  (`config.Company.AutomaticAccounts`: the account determination with
  payables, bank, FX gains and losses, GR/IR, price differences, cash
  discount and down payments; tax and withholding tax codes; the
  revaluation's adjustment, unrealised gain and loss accounts; the house
  banks' accounts; the bank statement posting rules) whose cost element is
  `primary_cost` needs a default assignment: one error per account at
  `companies[X].default_assignments[ACCOUNT]`, naming every setting that
  posts to it. Price differences are included although T56e derives the
  order item's cost center first: the default is the fallback when an item
  has none. The default profit center is required with profit center
  accounting and allowed only with it.
- **Ledger** (`ledger.Setup`). `gl_account.cost_element` (accounts set up
  before took their type's default); `cost_center_category`, deactivated
  rather than deleted, the category of every cost center period by foreign
  key, and removing one an active period has is refused;
  `company_controlling` keeps the fiscal year each switch applies from
  (NULL off) and the
  default profit center (a foreign key to `profit_center_company`);
  `default_assignment` the default cost center by legal entity and account
  (a foreign key to the company's cost center). Check and Apply read the
  same rules (`readControlling`): a switch is turned on only while the
  company has no postings in the fiscal year of today (`Setup.Now`) or
  later, and applies from that year; it is never turned off (a finding,
  and the trigger `company_controlling_stays_on`); a default cost or profit
  center must be active (the profit center assigned to the company), valid
  from today with no end and not locked (for actual costs; the profit
  center at all) on any day from today. Activation does not re-run Check,
  so Apply refuses a document that no longer fits (409
  `ledger.controlling_invalid`, naming each finding) and locks the
  company's period statuses from the current year when it turns a switch
  on, so postings under way finish first.
- **Center changes.** A create or move names an active category, read for
  share (422 `ledger.cost_center_category_invalid`); a new cost center's
  first period takes the category's locks. A lock or redate that would
  leave a default cost center or profit center unusable from today is
  refused (409 `ledger.default_center_in_use`), so automatic postings
  never meet a default they cannot use.
- **Tiers.** A category's name is cosmetic (1); adding or removing a
  category and its `revenue_allowed` and locks, `cost_element`,
  `default_assignments` and `default_profit_center` are posting-related
  (3); `cost_center_accounting` and `profit_center_accounting` are
  structural (4). This follows §11's table ("tier 3 or 4") rather than the
  bullet above for categories: `revenue_allowed` and the lock defaults
  decide what is posted.
- **Simulation** (`ledger.AssignmentSimulation`, with
  `ledger.AssignmentDisclosure`): for each company, the P&L accounts whose
  assignment the proposal changes, with the leading ledger's lines and
  entries of the last 90 days on each, how a line is assigned before and
  after ("cost center required", "default cost center 1000", "default
  profit center DUMMY"...) and whether lines without a cost center would
  now be refused; recorded by company, shown to each reader for the
  companies they may access, as payables' invoice replay is. It records
  nothing when no assignment changes. Lines carry no center until T56d, so
  every line on a newly refused account counts. The ledger now imports
  configuration's published `Simulator` and `Evidence`, as payables does
  (no cycle: configuration does not import the ledger).
- **Packs.** A pack's accounts may carry `cost_element`, checked by the
  pack validator (`config.Account.CostElementProblem`) and merged with the
  account. The released `ca/2026.1` pack is unchanged, and `ca/2026.2`
  (T53b, cash.yaml) carries the same accounts: its 5100 price
  differences and 6900 FX losses are expenses the system posts to, so a
  Canadian company turning cost center accounting on needs default
  assignments for them or sets them to `none`; 2026.3 (T62g) should decide
  whether the pack sets `none` for FX gains and losses (4900, 6900), which
  many controllers keep out of cost centers.

### 5. Account assignment on the journal line
Expand migration (T56d): `cost_center_id uuid`, `profit_center_id uuid`,
`segment_id uuid` on `ledger.journal_line`, nullable, no default (no
partition rewritten); foreign keys `(tenant_id, legal_entity_id,
cost_center_id)` → `cost_center (tenant_id, legal_entity_id, id)` (so the
company always matches) and to `profit_center`/`segment`, added `NOT VALID`
and validated per partition; partial indexes per partition on `(tenant_id,
legal_entity_id, cost_center_id, fiscal_year, fiscal_period) WHERE
cost_center_id IS NOT NULL` and the same for profit center, built
concurrently and attached (the runner's `migrate:partitioned-index`). Lines
posted before T56 keep NULL; nothing is backfilled.

**Which lines must carry what** (the commit-time check, beside the balance
trigger, for postings dated in fiscal years from T56's activation):
| Account's `cost_element` | Cost center | Profit center (PCA on) | Segment |
|---|---|---|---|
| `primary_cost` | required (or, from T60, another real cost object) | required, derived | derived |
| `revenue` | optional, only a revenue-allowed category, statistical | required | derived |
| `none`, P&L account | refused | required, derived or default | derived |
| `none`, balance sheet | refused | optional (entered only; no splitting, P3) | derived if a profit center is given |

**Derivation** (in the ledger, at posting, for every posting path:
journal entries, parked entries, `ledger.Postings` for payables, payments,
GR/IR, down payments, clearings, revaluation, statements, migration),
first hit wins:
1. Cost center: as given by the line; else the source document's (PO
   item, invoice item: §6); else `default_assignments` for the company
   and account; else refused (422 `ledger.cost_center_required`).
2. Profit center: given by the line, or the cost center's profit center in
   the period valid on the posting date; a given profit center that differs
   from the cost center's is refused (422
   `ledger.profit_center_mismatch`), as SAP's cost-center-derived profit
   center wins; else, for a P&L line, the company's default profit center.
3. Segment: always the profit center's; a given segment is refused unless
   it equals it.

**Validation**: the center exists in the line's company (the FK), is valid
on the posting date, and is not locked for the line's kind (cost or
revenue); the profit center is assigned to the company, valid and not
locked: 422 `ledger.cost_center_invalid` / `ledger.profit_center_invalid`
with the reason (`unknown`, `not_in_company`, `not_valid_on_date`,
`locked_for_costs`, `locked_for_revenue`, `revenue_not_allowed`) and the
line number, mapped from the constraint as the ledger's other problems are.
The posting reads each center period it uses `FOR SHARE`, as it reads the
period status, so a lock or validity change (which takes it `FOR UPDATE`)
waits for postings under way and postings after it see it.

- **Reversals** copy the original lines' assignments and are not refused
  for a center locked or expired since: a reversal undoes, it does not
  charge new cost (the period must be open, as today).
- **Parallel ledgers (T52)**: derivation runs once per posting and every
  ledger's copy carries the same assignments; a ledger-specific posting
  derives and validates for its own lines the same way.
- **Corrections** are repostings, never updates (ADR-0006): a manual entry
  on the same account from one center to another (SAP's KB11N, here an
  entry with two lines on one account), parked first when an agent
  proposes it.
- `ledger.Line`, `ledger.PostingLine`, the journal API and the
  `JournalEntryPosted` event gain the three ids and the codes (additive,
  ADR-0041).

As built in T56d (migrations 000155-000158; the rest is T56d2, below):
- **Columns and keys.** `cost_center_id`, `profit_center_id`, `segment_id`
  on `journal_line`. PostgreSQL 17 cannot add a `NOT VALID` foreign key to
  a partitioned table, so 000155 adds each key `NOT VALID` to every
  partition under the parent's name, 000156 validates them partition by
  partition and then adds the parent's, which takes the validated ones over
  without a scan (new partitions get them from the parent). The cost center
  key is `(tenant_id, cost_center_id, legal_entity_id)` → `cost_center`; the
  profit center's is `(tenant_id, profit_center_id, legal_entity_id)` →
  `profit_center_company`, so a line's profit center is assigned to its
  company by the database; the segment's → `segment`. 000157-000158 build
  the two partial indexes per partition (`migrate:partitioned-index`).
- **Commit-time check: T56d3.** The table above held by the database
  would refuse, during the rolling upgrade, the previous release's
  postings for a company whose center accounting is on (it writes no
  centers), against 06 §6.4. It is a contract step of its own, in the
  release after the one that ships T56d and T56d2, once no replica writes
  lines without centers. Until then the ledger's derivation is the only
  check, and every line is written through it.
- **Derivation and validation** (`ledger.assign`, `assignment.go`) run in
  `postAll`, which every posting path ends in (manual and parked entries,
  `Postings`, GR/IR, clearings, down payments, revaluation, statements,
  migration), once per posting group, so every ledger's copy carries the
  same (T52). Parking checks an entry the same way without locks (the
  posting checks it again). The order is §5's: the line's cost center, else
  the account's active `default_assignment` while cost center accounting
  applies to the posting's fiscal year (else 422
  `ledger.cost_center_required`); the line's profit center, else the cost
  center's in its period on the posting date (a different one given: 422
  `ledger.profit_center_mismatch`), else for a P&L line with profit center
  accounting the company's default (always there:
  `company_controlling_needs_default`, so `ledger.profit_center_required`
  is the database check's alone, T56d2); the segment is the profit
  center's (422 `ledger.segment_mismatch` for another, or one without a
  profit center). A center given is validated whether or not accounting is
  on. Reasons of `ledger.cost_center_invalid`: those listed above, plus
  `not_active` (a pending or rejected center) and `not_allowed` (a cost
  center on a `none` account); of `ledger.profit_center_invalid`:
  `unknown`, `not_in_company`, `not_active`, `not_valid_on_date`, `locked`
  (a profit center has one lock). The rules read are T56c's:
  `gl_account.cost_element`, `company_controlling`'s years compared with
  the posting's fiscal year, active `default_assignment` rows and the
  category's `revenue_allowed`.
- **A default before its validity.** A default is valid from the day it was
  set (T56c), so an automatic posting dated earlier, such as a revaluation
  at a past period end, would otherwise be refused. A center that came from
  a default (the cost center, its profit center, or the default profit
  center) on a date before its first period takes its first period's
  attributes. The posting path does not tell automatic from manual
  postings, so a manual line taking a default is treated alike; a center
  entered on such a date is refused (`not_valid_on_date`). The tracker
  scoped this to automatic postings: product owner to confirm the wider
  rule.
- **Locks.** The posting reads the centers `FOR KEY SHARE` (a change takes
  the center `FOR UPDATE` and waits; text changes do not), then, in a new
  statement, their active periods `FOR SHARE`, so a posting that waited for
  a change reads the periods it wrote; after the period statuses and before
  the number range. Categories are read unlocked: an activation changing
  `revenue_allowed` applies to the postings after it. A race test proves a
  posting and a lock's approval serialize both ways.
- **One clock.** `keepsDefaultCostCenter`/`keepsDefaultProfitCenter` take
  today from `Endpoints.Now` and `CenterChanges.Now`, wired to the server's
  clock as `Setup.Now` is, instead of the database's `current_date`.
- **Simulation.** `AssignmentSimulation` leaves out lines whose cost center
  is not the account's active default: their document gave it, the rules
  do not decide it (the column does not record whether a center was
  entered or derived).
- **T56d2** (split for size): the in-use refusals of §3, the journal API's
  lines (ids and codes, entered assignment on
  manual and parked entries, stored on parked lines), the journal list's
  `cost_center_id` / `profit_center_id` filters and the
  `JournalEntryPosted` fields. **T56d is not released without T56d2:**
  until then the API takes no entered center (a manual primary cost line
  without a default is refused with no way to give one), and a center
  change is not refused for the lines that carry the center.

### 6. Procurement and payables capture the assignment
- **Requisition and PO items** gain `account_assignment` (`none` | `cost_center`;
  `none` stays only for accounts whose `cost_element` is `none`, until
  inventory in phase 4 brings stock items) and `cost_center_id`, validated
  at release against the ledger (the cost center valid on the order date
  and not locked); copied from requisition to order. One cost center per
  item; a split across centers is several items in T56 (SAP's multiple
  account assignment by percentage is a later step if the pilot uses it:
  facts at the end).
- **Goods receipts and price differences** post with the PO item's cost
  center (GR line on the item's account; the price difference line of an
  account-assigned item, too, as SAP charges price differences of a
  non-valuated receipt to the item's assignment); GR/IR lines are balance
  sheet and carry none.
- **Supplier invoice items** without a PO gain `cost_center_id` (and an
  optional `profit_center_id` for `none` P&L accounts); PO-based items take
  the PO item's. Non-deductible tax (T61 §6, SAP NVV) is added to the base
  line and so carries its assignment.
- The **capture agent** proposes a cost center per item from the supplier's
  history and the invoice text (eval cases added, ADR-0039); a clerk or the
  default confirms. A posted invoice with a wrong center is corrected by a
  reposting (§5), not by editing the invoice.
- Custom fields (ADR-0046) gain the `cost_object` reference type (a cost
  or profit center, checked on write against the ledger), lifting the
  refusal; it is for extra statistical references, not the posting
  assignment, which is a first-class field.

### 7. Reporting
- **Actuals by assignment**: the projector (ADR-0006) maintains
  `ledger.assignment_balance`, keyed by ledger, company, account, cost
  center, profit center, segment, year, period and transaction currency,
  in the same round and transaction as `period_balance`, for lines with any
  assignment. `period_balance` is unchanged (the trial balance and T52e's
  reconciliation do not grow). A schema test proves that per ledger,
  company, account and period the assignment balances of P&L accounts add
  up to `period_balance` for fiscal years from activation.
- **Reads** (contract first, `ledger.cost_center.read`): `GET
  /v1/cost-center-balances` (company, ledger default leading, period
  range, by center or hierarchy node, by account; projected plus
  unprojected lines, exact at the read, as the trial balance) and `GET
  /v1/profit-center-balances` (P&L per profit center or node, and per
  segment), with drill-down to journal lines through the journal list's new
  `cost_center_id`/`profit_center_id` filters. A node's total is its
  centers' sum under today's hierarchy.
- **Plan against actual** is controlling's (§8), reading actuals through a
  ledger port (`ledger.AssignmentBalances`).
- **Analytics**: the store (ADR-0044) extracts the three new journal
  columns (an additive change to the journal dataset contract) and
  snapshots of centers, periods and hierarchies; `semantic.yaml` gains
  `cost_center`, `cost_center_name`, `cost_center_node`, `profit_center`,
  `segment` dimensions and `plan_amount`/`plan_variance` metrics. Ad-hoc
  questions, dashboards (T59) and agents use the semantic layer; the API
  reads are the authoritative figures and the fallback (ADR-0044's lineage).
- **Segment report**: P&L by segment (revenue, expenses, result) from
  profit-center balances; segment assets and liabilities only with
  document splitting (P3).

### 8. Plan values (minimal, decision P5)
`controlling.plan_category` (code, name, e.g. `PLAN`, `FORECAST`; locked or
open) and `controlling.plan_line` (category, fiscal year, period, company,
cost center or profit center, account, amount in local currency
`money.Minor`, optional quantity), SAP's ACDOCP in miniature. Entered by
upload (CSV/XLSX through T63's import or the API, idempotent by a batch
key: a batch replaces the plan of its category, year and centers) and
listed; a category is locked by a person (`controlling.plan.lock`, human
only) so a budget does not move after approval. Plan against actual: `GET
/v1/cost-center-plan-actual` (category, company, periods, center or node:
plan, actual, variance and percent). No planning functions, no
availability control, no plan in transaction currency, no plan per
parallel ledger (it compares with the leading ledger).

### 9. Document splitting: not in T56 (decision P3)
Decided (P3): no document splitting now. P&L by cost center, profit
center and segment needs none; a balance sheet by profit center or segment
does, and only if the CODM reviews segment assets (IFRS 8.23) or a local
rule asks for it. Splitting touches every posting path (active split of
payables, tax and cash lines by base line, passive inheritance on
clearing, zero-balance lines), is SAP's most expensive FI setting to get
wrong, and is introduced only from a fiscal year start. The columns on
every line (§5) and the derivation in one place keep it addable later as a
step of its own (T56l), from a fiscal year start, for new postings only.
Classic EC-PCA's periodic transfer of payables and receivables to profit
centers (1KEK) is not proposed: it is a second ledger of balances that
reconciles only by job.

### 10. Migration from SAP (T56h)
- **Master data**: `CSKS` (KOKRS, KOSTL, DATAB, DATBI, BUKRS, KOSAR,
  VERAK, PRCTR, KHINR, BKZKP, BKZER, PKZKP, WAERS) with texts `CSKT`
  (KTEXT, LTEXT by SPRAS); `CEPC` (PRCTR, DATAB, DATBI, SEGMENT, KHINR,
  LOCK_IND) with `CEPCT` and `CEPC_BUKRS`; segments `FAGL_SEGM`/`FAGL_SEGMT`;
  cost element categories from `CSKB-KATYP` (in S/4 maintained in FS00) onto
  `cost_element`; default assignments from OKB9 (`TKA3A` and its detail
  tables, as far as I know). OData where standard APIs exist (cost center
  and profit center read APIs; names checked against the pilot's release),
  else the file extract. Texts in the tenant's language, English otherwise.
- **Hierarchies**: `SETHEADER`/`SETNODE`/`SETLEAF` with set class 0101
  (cost center groups) and 0106 (profit center groups); the standard
  hierarchy is the one named in the controlling area (`TKA01-KHINR` for
  cost centers). `SETLEAF` holds value **ranges** (VALSIGN, VALOPTION,
  VALFROM, VALTO), which the loader expands against the migrated centers;
  a center not in exactly one standard leaf is a quality-gate issue, as
  is a range that matches nothing.
- **Postings**: ACDOCA's `RCNTR`, `PRCTR`, `SEGMENT` join the extract and the
  journal load when documents are migrated; an opening-balance-only
  migration at a year start needs none (P&L is zero; balance sheet lines
  carry no assignment without P3).
- **Through the services**: the loader creates centers through the same
  master-data operations as the API (validation, audit), approved by the
  migration run's approval, not one by one.
- **Controlling area mapping**: SAP's controlling area is the tenant here;
  a source with several controlling areas for the tenant's companies is
  migrated with codes prefixed per area if they collide (a quality-gate
  report, decided by the migration lead).

### 11. Change control (decision P1)
Cost and profit centers are **master data maintained through an API**, not
the configuration document: they change monthly in a living business,
are created by controllers rather than architects, and SAP treats them as
master data with change documents, not customizing. Changes are
**master-data change requests** in the ledger (`ledger.center_change`:
kind, payload, status, workflow request, as `partner.partner_change`):
| Change | Approval |
|---|---|
| Texts, description, person responsible, alternative hierarchy membership (T56b: approved like every tree change, see §3 as built; PO to confirm); creating a segment or changing its texts (T56a: a segment changes nothing until a profit center names it) | none (audited) |
| Create a cost or profit center; new validity period (profit center reassignment, category); extend or end validity; standard hierarchy move; profit center company assignment; a profit center's segment; set or clear a lock | one approver with `ledger.cost_center.approve` (controller, `approvals.DefaultCenterChangeRole`), not the requester, never an agent (T56a: `center_change` approval requests) |
| Segment of a posted profit center; company of a cost center; delete | refused (reorganisation or a new center) |
| Categories, `cost_element`, default assignments, PCA activation | configuration change request, tier 3 or 4 (§4; a category's name tier 1, cost and profit center accounting tier 4, as built in T56c) |
Every change writes an audit record (ADR-0011: `ledger.cost_center.request`
when asked for, `ledger.cost_center.create|redate|lock|move|reject` when
decided, `ledger.cost_center.texts` (which texts, not their values); the same for
`ledger.profit_center` with `assign`; `ledger.center_change.reject|cancel`;
`ledger.segment.create|update`) and an event through the outbox
(`CostCenterChanged`, `ProfitCenterChanged`, `SegmentChanged`; the person
responsible is left out of events). Locking and unlocking are `x-agent:
false`; the other change requests are `x-autonomy: propose`.

### 12. Agents
Agents may propose: a cost center on captured invoices and parked entries;
repostings of lines on the default profit center or with a likely wrong
center (as parked entries); new centers and hierarchy changes as
master-data change requests (`x-autonomy: propose`); plan uploads as drafts;
explanations of plan/actual variances. Agents may **never** approve a
master-data change, lock or unlock a center or plan category, or change
the configuration rules (403 `identity.agent_not_allowed`;
`ledger.cost_center.approve` and `controlling.plan.lock` are approval
permissions, `is_approval`, which agents cannot hold).

## Alternatives
- **Centers in the configuration document** (tiered change requests, like
  the chart of accounts). Strong control and simulation for free, but a
  new cost center would need a process owner and controller (tier 3) or an
  architect (tier 4) and an activation, and the document grows with every
  center. Kept for the rules (§4), rejected for the centers (P1).
- **A `controlling` context owning the master data behind a ledger port.**
  Cleaner CO ownership, but no foreign keys from the journal, the "cost
  center in the line's company" and "one leaf per center" rules become
  application checks, and every posting calls across a context. Rejected
  for centers (§2); still open for T60's orders.
- **Cost center as a custom field (ADR-0046).** What tenants do today; no
  validity, no derivation, no balances, no FK. Rejected.
- **Extend `period_balance` with the assignments.** One table, but the
  trial balance, the reconciliation and every balance read would scan and
  group the finer key. Rejected for a separate projection.
- **Document splitting now.** Balance sheet by profit center from day one,
  at the cost of every posting path and the highest configuration risk.
  Deferred (P3).

## Consequences
- Every P&L line from T56's activation has a profit center (with PCA on)
  and every primary cost line a cost center; posting can be refused for an
  assignment, which every client and agent must handle (new 422s).
- Automatic postings never fail on assignment, because activation refuses
  configurations that would let them.
- History before activation has no assignments; center reports start at
  activation (or at the migrated documents).
- Profit center and segment balance sheets are not available until P3's
  splitting step.
- T60 builds orders and allocations on these columns, categories and
  reports; T57 assets take a cost center from the asset master through the
  same derivation; T64 gets close tasks ("default profit center empty or
  explained", "plan/actual variances commented"); T68 reads segment and
  profit center from lines and consolidates by company (partner profit
  center for intra-segment eliminations is not in T56).

## Boundaries with the other phase 2 steps
- **T60 (orders, allocation, margin).** T56 supplies the master data,
  journal columns, the `primary_cost`/`revenue` categories with
  `secondary` reserved, derivation and the actual and plan reports. T60
  adds internal orders as a second real cost object (so "exactly one real
  object" becomes a choice), secondary cost elements, assessment and
  distribution cycles posting through the ledger with sender/receiver
  centers, settlement, overhead and the G/L-based margin by profit center.
  T56 posts no allocation; its only cost movement between centers is the
  manual reposting.
- **T52 (parallel ledgers).** T56d lands after T52's posting and clearing
  steps are in main: assignments are derived once and copied to every
  ledger's lines; reports take `ledger_id`, defaulting to the leading
  ledger; plan compares with the leading ledger.
- **T55 (analytical store).** T56i adds the columns and master-data
  snapshots to the dataset contracts and the semantic dimensions; no new
  extraction mechanism.
- **T64 (close).** Consumes the reports; close tasks above. No period logic
  of its own in T56.
- **T68 (consolidation).** Reads segment from lines for segment notes;
  intercompany by partner profit center is T68's or later.
- **T61 (tax).** Tax lines are balance sheet and carry none; non-deductible
  tax inherits the base line's assignment.

## Steps
Ordered so the pilot's must-haves come first; each ends green (lint, tests,
coverage 100%, reviewer). Lane F (ledger), after T52 for T56d.

| Step | Work | Acceptance criteria |
|---|---|---|
| T56a | **Center master data** (ledger) | `cost_center`, `cost_center_period`, `profit_center`, `profit_center_period`, `profit_center_company`, `segment` with RLS, `tenant_id` first, `center_change`, EXCLUDE on validity, periods written once; codes, company rules (the in-use refusals need postings and move to T56d); `/v1/cost-centers`, `/v1/profit-centers`, `/v1/segments`, `/v1/center-changes` (contract first; list, read, create, texts, redate, lock, move, assign; creation and every period, segment or company change as a `center_change` approval request with the approval table of §11); audit and events; `FailingTx` tests on every path; schema tests per database rule |
| T56b | **Hierarchies** (done; as built in §3) | Standard and alternative hierarchies per kind; one leaf per center in the standard (commit-time check), no cycles, depth ≤ 12; node moves as change requests; `GET /v1/hierarchies/{id}` with members; schema tests |
| T56c | **Configuration rules** (done; as built in §4) | `controlling.cost_center_categories`, `cost_element` per account (defaults by type), `default_assignments`, `profit_center_accounting`, `default_profit_center`; tiers as §4; activation refuses an automatically posted account needing a cost object without a default; packs (ADR-0048) set `cost_element` for their accounts; simulation lists lines that would be refused or reassigned |
| T56d | **Assignment on the journal** (built in part; the rest is T56d2 and T56d3, see §5 as built) | The in-use refusals T56a leaves out: `ledger.cost_center_period_in_use` (a change whose periods would differ from what posted lines read), `ledger.cost_center_posted_after` (validity ended before a posting), `ledger.segment_in_use` (a posted profit center's segment); expand migration (columns, FKs NOT VALID then validated per partition, partial indexes concurrently); derivation and validation in every posting path incl. parked and `Postings`; commit-time check of §5's table; reversals copy; T52 copies carry the same; errors and constraint mappings as §5; journal API, list filters and event fields (additive); `FOR SHARE` on center periods with a race test against a lock |
| T56d2 | **Assignment in the journal API; in-use refusals** (released with T56d) | `ledger.cost_center_period_in_use`, `ledger.cost_center_posted_after`, `ledger.segment_in_use` (attributes in force on each posted date); journal API lines with the three ids and codes, entered on manual and parked entries (stored on parked lines), list filters `cost_center_id`/`profit_center_id`, `JournalEntryPosted` fields (additive, openapi and asyncapi) |
| T56d3 | **Commit-time check of §5's table** (contract step: the release after the one shipping T56d and T56d2) | Deferred constraint trigger on `journal_line` holding the table for fiscal years from the switches, the segment always the profit center's, reversals exempt; constraint names mapped to the §5 problems (`ledger.profit_center_required` for a P&L line without one); schema tests per rule |
| T56e | **Procurement and payables** | `account_assignment` and `cost_center_id` on requisition and PO items (validated at release), GR and price-difference lines with the item's center; invoice items' `cost_center_id`/`profit_center_id`; capture agent proposals with eval cases; the free-text custom field in fixtures replaced; ADR-0034 note |
| T56f | **Actual reports** | `ledger.assignment_balance` maintained by the projector (exactly once, run-twice test); `GET /v1/cost-center-balances`, `/v1/profit-center-balances` (by center, node, segment, account, period; exact at the read); schema test that assignment balances add up to `period_balance` on P&L accounts; drill-down via journal filters |
| T56g | **Plan and plan/actual** (P5) | `controlling` context and schema; plan categories with a human-only lock; plan lines by upload and API, idempotent by batch; `GET /v1/cost-center-plan-actual`; actuals through `ledger.AssignmentBalances` |
| T56h | **SAP migration** | Extract and load of CSKS/CSKT, CEPC/CEPCT/CEPC_BUKRS, FAGL_SEGM(T), CSKB categories, OKB9 defaults, sets 0101/0106 with ranges expanded; ACDOCA RCNTR/PRCTR/SEGMENT; quality-gate issues (center outside the standard hierarchy, range matching nothing, unknown profit center); mock generator extended; reconciliation of migrated center totals |
| T56i | **Analytics** | Dataset contract change (journal columns, center and hierarchy snapshots); semantic dimensions and plan metrics; isolation tests unchanged and passing |
| T56j | **Controlling agent** | Proposes assignments and repostings (parked), new centers as change requests, explains plan/actual variances; evaluation suite; never approves or locks |
| T56k | **Cost object custom fields** | ADR-0046's `cost_object` reference type, checked on write; the compiler's refusal lifted; ADR-0046 amended |
| T56l | **Document splitting** (deferred, P3) | Not built now; if needed later, its own ADR amendment first, from a fiscal year start |

T56a-T56h are the pilot's must-haves (T56g because the tracker's exit
criterion names plan against actual, P5). T56i, T56j and
T56k can follow the pilot's first close. T56d needs T52's posting steps in
main and runs when no other lane is changing `journal.go`.

## Decisions taken (product owner, 2026-10-21)
All eight as recommended:
- **P1** (a): centers are master data maintained through the API with
  change requests (§11). Texts, descriptions and the person responsible
  need no approval (audited); creating a center, a new or re-dated validity
  period, a lock, and moving a center (profit center, company assignment,
  standard hierarchy node) need one controller with
  `ledger.cost_center.approve` who is not the requester. Agents may propose
  but never approve.
- **P2** (a): with profit center accounting on for a company, every P&L line
  carries a profit center, falling back to the company's default profit
  center.
- **P3** (a): no document splitting now; if a balance sheet per profit
  center or segment is needed later, it is introduced from a fiscal year
  start (T56l, deferred).
- **P4** (a): segments are master data now and are stored on lines, derived
  from the profit center.
- **P5** (a): a minimal plan per center, account and period with plan
  against actual in T56 (T56g).
- **P6** (a): every `primary_cost` account (expense by default) needs a cost
  center; exceptions are set to `none` by the pack or the tenant.
- **P7** (a): one current hierarchy; reports restate history under it.
- **P8** (a): company-level access as today; per-center access is a later
  step if the pilot asks.
- **No breaking change** (product owner, 2026-10-21, after T56b): creating
  a center does not require a standard hierarchy node; one created without
  goes to the standard hierarchy's `UNASSIGNED` leaf when its creation is
  approved (§3 as built).

The options as proposed:

| # | Decision | Options | Recommendation |
|---|---|---|---|
| P1 | Where cost and profit centers are maintained | (a) master-data API with change requests and one approver for financially relevant changes; (b) the configuration document (tier 3/4) | (a): centers change often and are a controller's job; the rules about them (categories, cost element categories, defaults) stay in configuration |
| P2 | Profit center accounting | (a) required on every P&L line when on for a company, with a default profit center; (b) optional, only where derived; (c) not now | (a): a P&L per profit center that does not add up to the company's is not a report; the default profit center makes gaps visible instead of refusing postings |
| P3 | Document splitting | (a) none in T56, P&L by profit center and segment only; (b) from the pilot's go-live; (c) a later step from a fiscal year start when needed | (a), with (c) if the pilot must show segment assets/liabilities or a balance sheet per profit center; please confirm the pilots are not listed (IFRS 8) |
| P4 | Segments | (a) segment master, derived from the profit center, stored on lines now; (b) later with segment reporting | (a): cheap now, and storing it on the line is what makes a later segment move a visible reorganisation rather than a silent restatement |
| P5 | Plan values | (a) minimal plan per center, account and period with categories and plan/actual in T56; (b) plan out, phase 9 planning | (a): the tracker's exit criterion asks for plan against actual, and controllers will compare against a budget from the first month |
| P6 | Which lines need a cost center | (a) every `primary_cost` account (expense by default), exceptions set to `none` by the pack or tenant; (b) only accounts flagged by the tenant | (a): SAP's default; forgetting to flag an account is worse than having to exclude one, and activation proves automatic postings have defaults |
| P7 | Hierarchy history | (a) one current hierarchy, reports restate history (SAP); (b) time-dependent hierarchies | (a): reorganisations are rare, restated comparatives are what managers expect, and (b) multiplies every report's logic |
| P8 | Access per center | (a) company-level access as today; (b) responsible persons see only their centers' reports | (a) for the pilot; (b) is a later step with T67's access agent if the pilot asks |

Information needed from the pilot (not decisions): whether the Canadian and
UAE companies are one tenant (one group, one controlling area in SAP) or
two; the number of cost and profit centers and how often they change; the
SAP controlling area set-up and whether new G/L document splitting is
active in the source; whether either company is listed or files under
IFRS 8; where the budget comes from (SAP CO plan, SAC/BPC, Excel) and at
what granularity; whether purchase orders use multiple account assignment
(split by percentage); whether the P&L is presented by function (cost of
sales) or by nature; the migration date (year start or mid-year with
documents).

## Sources
- SAP Help, Cost Center Master Data (time dependency, lock indicators):
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/5e23dc8fe9be4fd496f8ab556667ea05/ab0ad553088f4308e10000000a174cb4.html
- SAP Help, Cost Center Categories (lock defaults per category):
  https://help.sap.com/docs/SUPPORT_CONTENT/ficontrolling/3361878478.html
- SAP Help, Cost Center Group (one node in the standard hierarchy, many alternative groups):
  https://help.sap.com/docs/SAP_ERP/b42281c1645049a4b07c0ab25ec8b899/512ec6535e601e4be10000000a174cb4.html ;
  Processing Cost Centers: https://help.sap.com/docs/SAP_ERP/b42281c1645049a4b07c0ab25ec8b899/5a2ec6535e601e4be10000000a174cb4.html
- SAP Help, Cost Elements (S/4HANA Cloud) and Cost Element Categories:
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/c56f622a2edf491b9f1b596b55587009/13b27f3ce6d44b6888c53244eaa4581d.html ;
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/5e23dc8fe9be4fd496f8ab556667ea05/8196ebe296f54f9990eca346df4e02b4.html
- SAP Help, Universal Journal:
  https://help.sap.com/docs/SAP_S4HANA_ONPREMISE/651d8af3ea974ad1a4d74449122c620e/523b8a55559ad007e10000000a44538d.html
- SAP Help, Profit Center Determination and Derivation of Segment (S/4HANA Cloud):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/a376cd9ea00d476b96f18dea1247e6a5/9d949972f5924be792e8ad5b7871ddfb.html ;
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/cdccca8e03d74101a0135863bc522b49/50696c5978d947acbf7bb468867504cb.html
- SAP Help, Document Splitting and Making Settings for Document Splitting:
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/0fa84c9d9c634132b7c4abb9ffdd8f06/4911c9cc2a934a18e10000000a42189b.html ;
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/651d8af3ea974ad1a4d74449122c620e/49121407cbff280ee10000000a42189c.html
- SAP Help, Profit Center and Segment Reorganizations:
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/3cb1182b4a184bdd93f8d62e3f1f0741/0038e5b61d0c47b8b30a8ba374bcdb6b.html
- SAP Help, Cost Centers - Plan/Actuals; Maintain Category for Planning (ACDOCP, BUDGET02):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/c0c54048d35849128be8e872df5bea6d/40aaf2cb1d104c608011d037fc72dac5.html ;
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/c56f622a2edf491b9f1b596b55587009/eb9bf27fce10431e830d472402f470d3.html
- SAP Help, Account Assignment in Purchase Order (categories, EKKN, multiple assignment):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/c0c54048d35849128be8e872df5bea6d/d3c5fdf24efa43ce8686a8a8e3ed6ce8.html
- IFRS 8 Operating Segments (scope, CODM, 10% and 75% thresholds, segment assets):
  https://www.ifrs.org/content/dam/ifrs/publications/html-standards/english/2025/issued/ifrs8.html

**From memory, to check against the pilot's release:** the CSKS/CEPC field
lists, OKB9's tables (TKA3A and detail), set classes 0101/0106 and
`TKA01-KHINR`, the names of the cost center and profit center read APIs,
and that subsequent introduction of document splitting is a year-start
migration scenario.
