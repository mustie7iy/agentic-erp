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
- **In use** (T56d2). A change joins neighbouring periods that end up
  alike, so a period whose attributes did not change may still be retired
  and re-inserted; the in-use check therefore compares the attributes in
  force on each posted date, not period rows. A cost center's redate, lock
  or move is refused when its validity would end on or before the posting
  date of one of its lines (409 `ledger.cost_center_posted_after`), or when
  what a line read on its posting date would differ after it: the
  category, the profit center or the actual cost or revenue lock, or no
  period at all, as for a later start (409
  `ledger.cost_center_period_in_use`). The plan lock is not compared: no
  actual posting reads it. A line dated before the center's first period,
  which took a default's first period (§5 as built), read that period. The
  check computes in Go the date ranges in which the attributes differ and
  asks for the first line on the center within them
  (`journal_line_cost_center` index). A profit center's segment move is
  refused once any line carries the profit center (409
  `ledger.segment_in_use`). Both are planned at the request and again at
  the approval, with the center held for update: a posting under way holds
  it for key share, so the change waits for it and then sees its lines.
- **Same kind** (product owner 2026-10-25). A lock is compared with the
  lines that read it only: a change of a cost center's actual cost lock
  with its posted cost lines, of its revenue lock with its posted revenue
  lines; lines of the other kind do not block it. A change of the
  category, the profit center or the validity is compared with every
  line. The posting reads the actual cost lock on lines whose account's
  `gl_account.cost_element` is `primary_cost` and the revenue lock on
  those whose element is `revenue` (`validCostCenter`, §5). The check
  selects the lines by the account's `account_type` instead: the element is
  `primary_cost` only on `expense` accounts and `revenue` only on `revenue`
  accounts (`gl_account_cost_element`), a line on a `none` account takes
  no cost center (`not_allowed`), and the configuration may later set an
  account's element to `none` (tier 3, §4) while no path changes its type,
  so a cost line is a line on an `expense` account and a revenue line one
  on a `revenue` account, as they were when posted. A reversal copies its
  original's centers and account, so it counts as a line of the original's
  kind. The category's `revenue_allowed` is not a lock: a category change
  stays compared with every line.
- **Reversals after the end** (product owner 2026-10-25, as built): a
  reversal dated on or after a center's new end of validity counts in the
  posted-after check, as any line does.
- **Profit centers** (T56d4, product owner 2026-10-25). A profit center's
  redate or lock is checked against the lines carrying it, in the
  companies it is assigned to, as a cost center's: a validity ending on or
  before the posting date of such a line (409
  `ledger.profit_center_posted_after`), or its lock or validity on a date
  with such lines differing after the change, a later start included (409
  `ledger.profit_center_period_in_use`). A profit center has one lock,
  which every line carrying it reads, whatever its kind
  (`profitCenterState`, §5), so the same-kind rule compares it with every
  line; a line dated before the profit center's first period that took it
  from a default read that period, as for cost centers. The check of the
  cost center periods naming the profit center (409
  `ledger.profit_center_in_use`) stays, before the lines are read. Both
  checks run at the request and again at the approval, with the profit
  center held for update (`planProfitCenter`).

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
  - **Tables.** `hierarchy` (kind, code `^[0-9A-Z_-]{1,15}$`, 24 since
    000244, unique per
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
    `{kind}/{code}`, as every other resource; the list filters by kind,
    code and `is_standard`. **As built (fix, batch 46):** the list's page
    token is signed with the `is_standard` filter's value; it was signed
    with a pointer's address, so a second page under that filter was
    refused (`request.invalid_page_token`). A tree lists only the cost centers of companies the caller may act in, as every cost center read; its nodes are tenant-wide. The ETag is the tree's version, so two callers with different access may get different bodies under one ETag: harmless for If-Match (changes are planned against the whole tree), but a shared cache must vary by principal. `/v1/center-changes` filters by `hierarchy_id`. Event
    `HierarchyChanged` says what changed, not the tree; audit
    `ledger.hierarchy.create|request|node_create|node_rename|node_move|node_remove|place`.
  - **Amended: centers on inner nodes (SAP specialist, po:2026-11-09,
    A10(d) [M]; migration 000243).** SAP's sets may hold values beside
    their subsets, and a center's KHINR may name a group with subgroups,
    so a node holds centers and child nodes alike: "exactly one leaf" above
    reads "exactly one node". Decided by default (the leaf asked §3 to hold
    them; a generated child leaf per inner group would invent nodes and
    codes SAP does not have): 000243 drops the triggers' refusals of a node
    below a node holding centers and of a center at a node with children,
    and the plan no longer refuses adding or moving a node below centers,
    placing a center at a node with children (the API's and a new center's
    `standard_node_id` included), or the UNASSIGNED node and the standard
    root when they have children or centers. Removing a node with children
    or centers is still refused. The center reports already sum a node's
    subtree, its own centers included, so they are unchanged; a path ends
    at the center's node.
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
  **As built in T56c2 (review of T57a):** a tax code's accounts are its
  account in the single-rate form and each component's account in the
  multi-component form (T61a; a non-deductible component has none):
  `AutomaticAccounts` had listed only the first, so a component posting to
  a `primary_cost` account passed activation without a default and its
  posting would have been refused. The finding names the component
  (`tax_codes[X].components[K].account`). Every check of automatic
  accounts, this one and ADR-0057's refusal of an asset reconciliation
  account, reads this one list.
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
  (no cycle: configuration does not import the ledger). Since the
  invoices' simulation replays the window of the regression suite (07
  T72l decisions), `Simulator` is given that window; the assignment replay
  keeps the 90 days above, as the suite samples invoices, not journal
  lines, and records them as `assignment_window_days`.
- **Packs.** A pack's accounts may carry `cost_element`, checked by the
  pack validator (`config.Account.CostElementProblem`) and merged with the
  account. The released `ca/2026.1` pack is unchanged, and `ca/2026.2`
  (T53b, cash.yaml) carries the same accounts: its 5100 price
  differences and 6900 FX losses are expenses the system posts to, so a
  Canadian company turning cost center accounting on needs default
  assignments for them or sets them to `none`; 2026.3 (T62g) should decide
  whether the pack sets `none` for FX gains and losses (4900, 6900), which
  many controllers keep out of cost centers. **As built (T62g):** it does
  not; T62g's version, 2026.4 (2026.3 went to another step's tax
  settings), keeps the accounts as they are. Whether FX results are
  controlled is the company's controlling policy, not a country's law or
  practice a pack cites a source for, and a pack value would be merged into
  every adopting company that has not changed the account; a company
  turning cost center accounting on sets `none` or a default assignment
  itself, as above (decided by default, for the product owner to overrule).

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

As built in T56d, T56d2 and T56d4 (migrations 000155-000160; the commit-time check is T56d3):
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
  entered on such a date is refused (`not_valid_on_date`). Product owner
  2026-10-25: a posting dated before a default's validity may take the
  default whether it is automatic or manual, as built; a center entered on
  such a date stays refused.
- **Locks.** The posting reads the centers `FOR KEY SHARE` (a change takes
  the center `FOR UPDATE` and waits; text changes do not), then, in a new
  statement, their active periods `FOR SHARE`, so a posting that waited for
  a change reads the periods it wrote; after the period statuses and before
  the number range. Categories are read unlocked: an activation changing
  `revenue_allowed` applies to the postings after it. A race test proves a
  posting and a lock's approval serialize both ways.
- **One clock.** `keepsDefaultCostCenter`/`keepsDefaultProfitCenter` take
  today from `Endpoints.Now` and `CenterChanges.Now`, wired to the server's
  clock as `Setup.Now` is, instead of the database's `current_date` (on
  the company's date in its time zone from T72n4, §11 as built).
- **Simulation.** `AssignmentSimulation` leaves out lines whose cost center
  is not the account's active default: their document gave it, the rules
  do not decide it (the column does not record whether a center was
  entered or derived).
- **T56d2** (split for size, released with T56d): the in-use refusals of
  §3 as built. The journal API's lines take `cost_center_id`,
  `profit_center_id` and `segment_id` (JournalLineInput) on manual and
  parked entries and show each with its code (`cost_center_code`,
  `profit_center_code`, `segment_code`, read by joining the centers); a
  parked line stores the centers as entered (migrations 000159-000160:
  three nullable columns on `parked_entry_line` with foreign keys to the
  tenant's centers, added `NOT VALID` and validated), so its posting
  derives the rest on its posting date, and a header change keeps them.
  `GET /v1/journal-entries` filters by `cost_center_id` and
  `profit_center_id` (entries with a line on the center, listed with all
  their lines). `JournalEntryPosted` lines carry the three ids with their
  codes, absent when a line has none. All additive (ADR-0041).
- **T56d4** (product owner 2026-10-25): the lock comparisons of the in-use
  check are same-kind, a reversal after a new end counts as built, and a
  profit center's redate or lock is checked against the lines carrying it
  (409 `ledger.profit_center_posted_after`,
  `ledger.profit_center_period_in_use`), at the request and at the
  approval: §3 as built. Additive (ADR-0041): two new problem codes on
  `redateProfitCenter`, `lockProfitCenter` and the approval; no migration
  (the profit center indexes of 000158 serve the lookup).

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

As built in T56e (migrations 000161-000162; the capture agent's proposals are T56e2):
- **Items.** `requisition_item` and `purchase_order_item` gain
  `account_assignment` (`none` | `cost_center`, default `none`) and
  `cost_center_id`, tied by `requisition_item_account_assignment` /
  `purchase_order_item_account_assignment` (a cost center assignment names
  its cost center, and only it does; added `NOT VALID`, validated by
  000162); `supplier_invoice_item` gains `cost_center_id` and
  `profit_center_id`. The centers are the ledger's, with no foreign key
  across schemas (§2), as the items' `account_id`. The API takes
  `account_assignment` optionally: without it, `cost_center` when a cost
  center is given, else `none` (422 `procurement.invalid_account_assignment`
  when the two disagree), so existing clients are unchanged (additive).
- **Checked through the ledger.** `ledger.Accounts.CheckAssignments`, the
  ledger's published check, runs the posting's derivation and validation
  (`assign`, §5 as built) on the items without locks and names the items
  (`items[i].cost_center_id`, "Item n"). An order or requisition item takes
  no default: while cost center accounting applies to the date's fiscal
  year, an item on a primary cost account names its cost center (422
  `ledger.cost_center_required`), so `none` stays for accounts whose cost
  element is `none`, as decided above; the cost center is one of the legal
  entity, active, valid on the date and not locked for costs (422
  `ledger.cost_center_invalid` with the reason), its profit center valid
  and unlocked (`ledger.profit_center_invalid`). A requisition is checked
  when it is submitted, on that day (the company's date in its time zone
  from T72n4, §11 as built); an order when it is released, on its
  order date, before approval is asked and again as the release is
  approved; an order change's added items when it is submitted and as it
  is approved, on the changed order date. An order a migration run releases
  (`ReleaseLoadedOrder`) is not checked: it is valued directly, and its
  invoices' lines are derived and checked when they post; migrated account
  assignments (EKKN) are for T56h if the pilot needs them. Confirmed by
  the product owner on 2026-10-25: migrated orders are checked as their
  invoices post, not when they are loaded.
- **A released item keeps its assignment**, as its account (000161 extends
  `purchase_order_item_identity`; 422 `procurement.invalid_change`): its
  receipts, their reversals (102), returns (122) and the invoice's price
  difference all post to one center. An item added back under a removed
  number keeps the removed one's. Confirmed by the product owner on
  2026-10-25: a changed center means cancelling the item and adding one
  under a new item number by an order change.
- **Postings.** A goods movement's WE entry posts the item's account with
  the item's cost center (`GRIRItem.CostCenterID`); the GR/IR line, a
  balance sheet line, carries none. An invoice's price difference carries
  the order item's cost center when the price difference account is a
  primary cost account; on one whose element is `none` it derives as any
  line does. An item without an order posts its line with the item's cost
  and profit center; one on an order takes the order item's cost center
  (422 `payables.assignment_from_order` for another one, or a profit
  center, given). Non-deductible tax added to an item's line (T61d)
  carries its assignment. An invoice item without an order is checked at
  entry and at each change, on the invoice's posting date (else its
  invoice date), with defaults: without a cost center it takes the
  account's default, as its posting will. Every posting checks again.
  Own lines of a GR/IR posting (an invoice on a GR/IR order with items
  without an order item) now carry their centers into the ledger.
- **No fiscal year, nothing checked.** The check resolves the date's
  fiscal year from the legal entity's own periods; a date in none (an
  order released before the next year is set up) checks nothing, as no
  switch applies to it and its posting is refused for the date, so the
  check adds no `ledger.no_fiscal_period` to release or submission.
- **Order changes.** An item an order change resends without
  `account_assignment` or `cost_center_id` keeps the item's, as its
  tolerance, so clients that do not know the fields change cost-centred
  orders as before.
- **Rolling upgrade.** The previous release reads and writes items without
  the columns: a goods receipt or invoice it posts for a cost-centred item
  carries none of the item's centers. Cost-centred items exist only once
  this release writes them, and turning cost center accounting on is a
  tier 4 change, which belongs after the rollout (06 §6.4); the window is
  accepted.
- **Not built (no source in the design):** copying a requisition's items
  to the order (the client sends the order's items, as for every other
  field); assignment fields on `PurchaseOrderReleased`/`PurchaseOrderChanged`
  (a later additive change if a consumer needs them).
- **Fixtures.** The free-text custom field `cost_center` of the test
  fixtures' purchase order and supplier invoice schemas is replaced by
  `region`; tests assign items with the real field.

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

As built in T56f (migration 000163):
- **The projection.** `ledger.assignment_balance` has the key above with
  the centers nullable (`assignment_balance_assigned`: at least one), so
  its primary key is a hash of the key, as `gr_ir_position`'s; only the
  projection writes it. The projector's mechanism is reused rather than
  copied: posting queues the entry in `balance_projection`, and the
  projector's one statement that marks a batch projected and adds it to
  `period_balance` fires the statement trigger
  `balance_projection_assignment`, which adds the same entries' assigned
  lines (first projection only, balances taken in key order). So both
  balances count each entry exactly once in one transaction, however often
  rounds fail or run side by side, and the previous release's projector,
  which knows nothing of the table, keeps it during a rolling upgrade (06
  §6.4). Tests: a schema test of the trigger (`db/assignment_balance_test.go`:
  once, upserted by key, written by the projection only) and a run-twice
  test (`TestAssignmentProjectionRunsOnce`: projectors side by side, then
  again).
- **Lines posted before.** The migration creates the trigger first, which
  waits for projector rounds under way and holds new ones until it
  commits, then adds every line already projected that carries a center,
  as 000054 backfilled GR/IR positions; lines still queued are left to the
  trigger. Postings wait while it runs. Lines carry centers only since
  T56d and the switches turn on from a fiscal year with no postings yet,
  so it adds at most the open year's assigned lines since T56d's release,
  though it scans the projected lines of every year;
  if that release has run long at volume before this one, the backfill
  should instead be an operation in batches (06 §6.4). Decided by the
  product owner (2026-10-25): the backfill stays in the migration, as
  nothing is in production yet; it is revisited before production if
  volumes exist by then.
- **Reads.** `GET /v1/cost-center-balances` and `GET
  /v1/profit-center-balances` (`x-slo-class: report`): one legal entity (the caller's company access, as the trial
  balance), ledger (default leading) and fiscal year, period range
  (`fiscal_period_from`/`_to`), optionally a center, a hierarchy node, a
  segment and an account; one row per center (a profit center's with its
  segment), account, period and transaction currency with debit and credit
  in the three currencies, paged by center, account or period. Exact at
  the read as the trial balance: the projected balances plus the lines of
  entries still queued. A node filter takes the node's and its
  descendants' centers from the tree as `GET
  /v1/hierarchies/{id}/nodes/{node_id}` reads it (`loadTree`, decision
  P7); a node's total is the sum of its centers' rows (the API returns the
  rows, not a node subtotal). A node of the other kind, unknown or removed,
  has no rows. Only profit and loss accounts: the profit center report
  leaves out balance sheet lines with a profit center entered (P3); a
  balance sheet line carries no cost center.
- **Permissions** (product owner, 2026-10-25): both reports need
  `ledger.cost_center.read` and `ledger.entry.read`, the trial balance's,
  so their amounts stay behind the permission of every other ledger
  amount. The contract lists the second in `x-also-permissions`, which the
  authorizer requires beside `x-permission` (04-api-design); either alone
  is 403 `identity.permission_missing`
  (`TestCenterBalancesNeedBothPermissions`).
- **No node subtotals** (product owner, 2026-10-25): the API returns the
  rows of a node's centers, and a client sums them for the node's total.
- **P&L adds up.** `TestAssignmentBalancesAddUp` checks that per ledger,
  company, account and period the profit center balances of the P&L
  accounts equal `period_balance` with profit center accounting on. It is
  in the ledger package's tests, not `db`'s, because `period_balance` is
  kept by the projector's Go code, which the `db` package cannot import.
- **Drill-down** is the journal list's `cost_center_id` and
  `profit_center_id` filters with the company, ledger and year (T56d2),
  narrowed to a period by its dates (`posting_date_from`/`_to`); no journal
  filter is added here, so the entries listed are all the center's, with
  all their lines, not only the row's account. Drill-down by account
  (product owner, 2026-10-25) comes through the journal list's
  `account_id` filter, the contract change T73j2 makes for the trial
  balance's accounts. **As built in T73j2:** `account_id` combines with
  `cost_center_id` or `profit_center_id`, so a center balance's row (a
  center and an account) lists the entries with a line on each; a period
  is `fiscal_period` (added with it) or the dates as before.
- **Budget.** Both reports, every page of one company and period, are
  held to the trial balance's budget (01-requirements: p95 1 s) at its
  volume (360,000 projected rows, 2,000 pending lines,
  `TestCenterBalancesBudget`); the number of centers (100 of each) is the
  test's, as no requirement names one. The rows are loaded period by
  period, as the projector writes them.
- **Not built:** node subtotals (decided above); cross-company totals.
  The `ledger.AssignmentBalances` port came with the plan (T56g, §8 as
  built).

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
plan, actual, variance and percent), and by profit center `GET
/v1/profit-center-plan-actual` (product owner, 2026-10-27, T56g3). No planning functions, no
availability control, no plan in transaction currency, no plan per
parallel ledger (it compares with the leading ledger).

As built in T56g (migration 000164):
- **The context.** `internal/controlling`, schema `controlling` with its
  outbox. It imports the ledger's published port only (`internal/layout`:
  `controlling` may import `ledger` and `tenancy`; no context imports
  it): `ledger.AssignmentBalances` answers what a plan of a company's year
  may name (`PlanObjects`: its active cost centers, the active profit
  centers assigned to it, the profit and loss accounts set up in it, the
  periods of the year in its calendar), the cost centers of a center or
  hierarchy node (`CostCenters`, as the cost center report takes them,
  §7) and the actuals by cost center and account (`CostCenterActuals`:
  the leading ledger's local amounts, debit positive, projected plus
  pending as the report reads them); for the profit center report
  (T56g3) `ProfitCenters` and `ProfitCenterActuals` likewise, as the
  profit center report reads them. No foreign key crosses into the
  ledger's schema.
- **Categories.** `controlling.plan_category`: a code
  (`^[0-9A-Z_]{1,10}$`, as the cost center categories') unique in the
  tenant, a name, `locked`. `POST /v1/plan-categories` creates an open one
  (`controlling.plan.write`); `:lock` and `:unlock` take the caller's
  version, read the category FOR UPDATE and need `controlling.plan.lock`,
  an approval permission, so only a person locks (`x-agent: false`; 403
  `identity.agent_not_allowed`), as §12 says. A category has no rename
  and is never deleted (not asked for).
- **Batches.** `POST /v1/plan-batches` carries a category, company, fiscal
  year, `batch_key` and up to 5,000 lines (a cost center or a profit
  center, an account, a period, the amount in the company's local
  currency, debit positive as on the journal, an optional positive
  quantity with its unit). Every line is checked before anything is
  written (422 `controlling.plan_batch_invalid`, `lines[N].field`, at most
  100). The batch replaces the plan of its category, company and year on
  every center it names, in every period and on every account; other
  centers' plans stay. Lines are never deleted (business records are not, 03 §3.1
  "Lifecycles instead of deletes"; `TestDesignRules`): the replaced ones get
  `replaced_by_batch_id`, once, so earlier plans stay readable and the plan
  is the lines not replaced (`plan_line_once` on those). The batch row
  keeps the key, once per category, company and year, and the SHA-256 of
  its lines in any order: the key with the same lines answers the batch as
  it was applied (`replayed`), writes and publishes nothing, also after
  later batches, so a late retry never undoes a newer plan; the key with
  other lines is 409 `controlling.plan_batch_reused`. The category is held
  FOR UPDATE for the batch, so batches of a category run one after the
  other and a lock waits for them; a trigger refuses writing or replacing
  lines of a locked category (`plan_category_open`, 409
  `controlling.plan_category_locked`), the category read FOR SHARE.
  Audited per batch (`controlling.plan_batch.apply`) and published as
  `PlanBatchApplied` (the centers whose plan it replaced, the counts).
  `GET /v1/plan-lines` lists the current lines of a company and year
  (filters: category, center, account, batch).
- **The cost center's plan lock** (T56g2, product owner 2026-10-27). The
  lock that counts is the cost center period's `lock_plan`
  (`ledger.cost_center_period`, migration 000128, set by `:lock` through a
  center change, §3); `ledger.cost_center_category.lock_plan` (migration
  000140) is only the lock a new cost center of the category starts with
  (§4). A fiscal period is locked for a cost center when it shares a day
  with an active period of the center whose `lock_plan` is set (valid_to
  exclusive, the fiscal period's `end_date` inclusive, in the calendar of
  any of the company's ledgers). A batch whose lines are otherwise valid is
  refused as a whole when it would change a center's plan in a locked
  period: a line planning the center there, or a current line of the
  center there, which the batch would mark replaced, since a batch
  replaces its centers' plan in every period of the year. 422
  `controlling.cost_center_plan_locked` names each such line, and for a
  replaced line the batch's first line on the center, as
  `lines[N].cost_center_id` (code `plan_locked`, at most 100); nothing is
  written, as the ledger refuses a posting on a center locked for actual
  costs with 422 naming the line (§5). Leaving the locked periods' lines
  out of the replacement instead was not built: it would change what a
  batch replaces, which the product owner did not decide. Invalid lines
  are reported first (`controlling.plan_batch_invalid`). Profit
  center lines are not affected: `lock_plan` is the cost center's. The
  check sits after the batch key's: a batch already applied is answered as
  it was (`replayed`) after a later lock, as after a category lock.
  *Where it is enforced:* in the service, through the ledger's port
  (`PlanObjects` returns `PlanLocked`, the locked periods per cost
  center), not by a trigger: the cost center periods are the ledger's, a
  trigger on `controlling.plan_line` would read across the context
  boundary that no foreign key crosses either (§8 above); the current
  lines are read with the category held FOR UPDATE, so no other batch
  changes them meanwhile; and the ledger
  enforces its own actual-cost and revenue locks in the posting service
  in the same way, not in the database. *Concurrency (ADR-0008):*
  `PlanObjects` reads the cost centers `FOR KEY SHARE`, held until the
  batch commits, and reads their periods in a later statement. Every
  change of a cost center's periods is applied by `CenterChanges.Approved`,
  which first locks the center `FOR UPDATE` (`decided`, then
  `applyCostCenterChange`). So a batch meeting a lock being applied waits
  for it and, read committed, reads the periods it wrote and is refused;
  a lock meeting a batch under way waits for the batch, whose lines were
  planned before the lock and stay (as a category lock waits for a batch).
  `TestPlanLockAndBatchSerialize` drives both orders. No migration.
- **Upload.** The batch is the upload: a client, or T63's import of
  CSV/XLSX files when it exists, sends the file's lines as a batch. No CSV
  endpoint of its own was built.
- **Plan against actual.** `GET /v1/cost-center-plan-actual`
  (`controlling.plan.read` and, as for the center reports,
  `ledger.entry.read`; `x-slo-class: report`): category, company, year,
  period range (`fiscal_period_from`/`_to`), cost center or hierarchy node
  (its subtree's cost centers under today's tree, as §7). One row per cost
  center and account summed over the periods: the plan, the actual, the
  variance (actual less plan) and the variance in percent of the plan, a
  decimal string of two places computed exactly (`big.Rat`, no floats) and
  rounded half away from zero, absent when the plan is zero (the center
  reports show no percent, so this is the first; the API's `Decimal`, as
  rates and percentages elsewhere). A cost center and account with a plan
  or an actual has a row. Pages are cut by a cursor over cost center and
  account: each side reads its first keys after the page's start, one
  more than the page, and the page is the first keys of the two. An
  unknown category, or a company the caller cannot act in, has no rows.
- **Plan against actual by profit center** (T56g3, product owner
  2026-10-27). `GET /v1/profit-center-plan-actual` has the cost center
  report's shape, permissions (`controlling.plan.read` and
  `ledger.entry.read`), company access, paging and money handling, with
  `profit_center_id` in place of `cost_center_id` and a node of a profit
  center hierarchy (its subtree's profit centers under today's tree, as
  `GET /v1/profit-center-balances` takes it). One row per profit center
  and account: the category's profit center lines (a batch's cost center
  lines are the cost center report's only; plan lines name one center,
  never both) against the leading ledger's actual as the profit center
  report reads it (§7): the profit center each journal line carries,
  entered or derived at posting (§5: from the cost center, or the
  company's default profit center), summed over the line's segments, on
  profit and loss accounts only, so a balance sheet line with a profit
  center entered is not counted (P3). The same code computes both
  reports (`paKind`); the ledger reads both kinds' actuals with one query
  (`actuals`), as its two center reports share one. Where ADR-0050 and 03
  did not settle a point the cost center report's behaviour was taken
  and is open to the product owner: actuals include derived profit
  centers, the default (dummy) profit center among them; a node's rows
  are its centers' rows, no subtotal (as decided for §7); no breakdown by
  segment, as plan lines carry none. No budget test, as the cost center
  report has none. No migration.
- **Not built:** a profit center's `locked` is not read
  by batches; agents' plan uploads as drafts (§12, T56j); a category's
  rename.

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

As built in T56h1 (no migration; T56h is split into T56h1-T56h4 in the
tracker: hierarchies and categories T56h2, OKB9 defaults T56h3, ACDOCA's
centers and the reconciliation of center totals T56h4):
- **Extract**: `internal/sap` carries CSKS, CSKT, CEPC, CEPCT, CEPC_BUKRS,
  FAGL_SEGM and FAGL_SEGMT with the fields of the list above, plus those
  the load needs that the list lacks (CSKT's and CEPCT's keys, CEPC's
  KOKRS and person responsible VERAK, CEPC_BUKRS's, FAGL_SEGMT's), all
  pending the SAP specialist (docs/migration.md). A scope keeps its
  company codes' cost centers, the profit centers assigned to them with
  all their assignments, the segments those name and their texts. The
  OData mapping reads none of them yet (the read APIs' names are not
  recorded). The mock's company 3000 has a segment, two profit centers
  (one locked from 1 October) and four cost centers (one moving profit
  center on 1 July, one locked for actual revenues).
- **Through the services, the run their approval** (decided by default:
  §10 says "approved by the migration run's approval, not one by one", and
  migration runs have no approval of their own; partner.Migration
  settled the same for suppliers: the run, started by a person with
  `integration.migration.run` and reconciled, is the governed act):
  `ledger.CenterMigration` creates each center active at once and records
  its creation as a `center_change` inserted `applied`, without a workflow
  request, its reason naming the run and SAP's key, its payload the periods
  (and a profit center's companies) it created; a later run's assignment
  of a loaded profit center to more companies is an applied `assign`
  change the same way. The checks are those of an approved create: an
  active category (read for share), a profit center active, assigned to
  the company and valid over each period, a place at the standard
  hierarchy's UNASSIGNED leaf (created with the hierarchy when missing).
  The audit and the `CostCenterChanged`/`ProfitCenterChanged`/
  `SegmentChanged` events are the API's.
- **Periods from slices** (decided by default: §3 keeps centers'
  attributes time-dependent as SAP's slices): each slice is a period
  (valid_to the day after DATBI, 99991231 open-ended), slices saying the
  same of what the platform keeps joined; the slices must adjoin. A
  migrated cost center's locks are SAP's (BKZKP, BKZER, PKZKP), not its
  category's defaults for a new one. A profit center has one segment (§3),
  so slices naming different segments are refused (a reorganisation).
- **Texts** (decided by default: the platform has no tenant language, so
  "the tenant's language, English otherwise" is English): a center's name
  and description are its English text of its latest slice, else another
  language's with a warning; the person responsible is VERAK of the latest
  slice, which the platform requires (a center without one is refused).
- **Categories**: KOSAR is the platform category's code, which the
  configuration must have (the gate names the category to add); no
  mapping table, as none is decided.
- **Controlling areas**: a profit center code in two areas is refused by
  the gate; prefixing per area (above) is not built.
- **Re-runs**: what an earlier run loaded is kept; a change SAP made since
  is a difference in the reconciliation, which compares each company's
  cost and profit centers (count and periods) where either side has any.
  Assigning a loaded profit center is refused while an API change of it is
  pending (`ledger.center_change_pending`), which would otherwise become
  unapprovable. Creating a center locks its standard hierarchy and the API
  locks a center before its hierarchy, so the load first locks every
  loaded profit center it assigns or names, in id order (review:T56h1).
- **Scope**: a cost center with a slice in a scope company comes with all
  its slices, so one in two company codes is refused rather than loaded
  truncated.
- **Hierarchy**: until T56h2 loads SAP's sets, migrated centers sit at the
  standard hierarchy's UNASSIGNED leaf, so the walkthrough's step 12 builds
  its structure in that standard hierarchy instead of creating one.

As built in T56h2 (no migration; the extract's tables and fields are
pending the SAP specialist, docs/migration.md):
- **Extract**: `internal/sap` carries TKA01 (KOKRS, KHINR), SETHEADER,
  SETHEADERT (a set's text, not in the list above), SETNODE and SETLEAF of
  classes 0101 and 0106, and CSKB (KOKRS, KSTAR, DATBI, KATYP). A scope
  keeps those of the controlling areas of the centers it keeps; a company
  code without centers has no known area (TKA02 is not read; read since,
  below). The mock's
  area 1000 has the standard hierarchy H1000 (H3000 Canada, H3100
  administration as the range 3100 to 3199, H3200 sales; since A10(d)
  H3000 holds 3100 itself and H3100 the range 3101 to 3199), an alternative
  REGIONS (Quebec as 3100 to 3299 without 3100 and 3300), the profit
  centers' PH1000, and its company 3000's accounts as cost elements, the
  exchange rate accounts not.
- **Where SAP's trees go** (decided by default: §3 keeps one standard
  hierarchy per kind and tenant, and T56b creates it coded STANDARD when a
  first center needs it): each area's standard tree loads as a node below
  the root of the tenant's standard hierarchy of its kind
  (`CenterMigration.StandardHierarchy`, created as T56b does when the
  tenant has none), rather than becoming it, so a tree the tenant already
  has (API nodes, an earlier run's UNASSIGNED leaf with its centers) is
  kept, and several areas fit one tree. Every other set no set includes
  is an alternative hierarchy coded as it.
- **The profit centers' standard hierarchy** (decided by default, nothing
  read names it: §10 names TKA01-KHINR for cost centers only, and a field
  named from memory is left out for the specialist): the one tree holding every set
  the area's profit centers name in CEPC-KHINR; more than one is refused.
- **Through the services, the run their approval**, as the centers: each
  node is a `node_create` and each alternative place a `place`
  center change inserted `applied`, with no approval request, its reason
  naming the run (`CenterMigration.AddNodes`, `PlaceCenters`), planned as
  an approved change is (`tree.plan`) and audited and published as the
  API's. A center is created at its standard leaf (`StandardNodeID`, the
  create change placing it). The run places only centers it created,
  which no other transaction sees, so it does not lock them before the
  hierarchy as the API does.
- **Values** (§10: "expanded against the migrated centers"): VALSIGN I
  includes and E excludes, VALOPTION EQ one value and BT a range, compared
  in SAP's internal form (digits alone padded to ten), against the
  extract's centers of the area; other options (patterns) are refused, not
  guessed. A center's standard leaves are those its values take plus the
  one its KHINR names; exactly one is required, and at most one in an
  alternative tree. A value taking no center is a warning, not an error
  (decided by default: a group may hold a range for centers not yet
  created, and it places nothing), as is an area with groups but no
  standard hierarchy, whose centers go to the UNASSIGNED leaf as before.
- **Re-runs**: a set loaded (its node by `integration.external_ref`, one
  per tree it is in) is kept, not renamed or moved; one SAP added is added
  below its parent; a center an earlier run loaded keeps its places, also
  when a later run first loads a tree (a controller places it). The
  report counts `hierarchies`, `hierarchy_nodes` and `hierarchy_places`.
- **Cost elements** (decided by default: the chart is configuration,
  which the configuration draft migrates from SAP, `delivery/sapconfig`,
  so CSKB-KATYP goes there, as T042* does, rather than into the run): 01
  is `primary_cost` on an expense account, 11 and 12 `revenue` on a
  revenue account, and a profit and loss account with no CSKB row `none`
  (from memory: S/4HANA keeps non-operating accounts without a cost
  element, pending the specialist); anything else leaves the type's
  default with a note. The run does not compare CSKB with the active
  configuration: T56h2 lists no such gate issue.
- **OData**: the mapping of these tables and of T56h1's (the cost and
  profit center read APIs) needs their names from the pilot's
  `$metadata`, which is not recorded, so it is split off as T56h5 rather
  than guessed.

Answered by the SAP specialist (product owner, po:2026-11-09; questions
A8-A12; docs/migration.md "What to check on a real SAP system" records
A8 and A10-A12 in full, A9 is recorded here only; [H] high, [M] medium and to verify on the
pilot system, [L] low and to verify in SE11). What changes the built load
is a tracker leaf under the T56h leaves:
- **Master data (A8, [H])**: the tables, fields and meanings §10 and T56h1
  read are confirmed (CSKS keyed KOKRS, KOSTL, DATBI; CEPC KOKRS, PRCTR,
  DATBI; `E` the stored English key). A company code belongs to exactly
  one controlling area, named by TKA02 (BUKRS to KOKRS): **to read**
  (leaf), so a company code without centers has a known area.
- **Centers on journal lines (A9, for T56h4)**: RCNTR, PRCTR and SEGMENT
  are in the master data's internal form [H]. PRCTR is derived from
  CSKS-PRCTR at posting [M]: a cost center without a profit center gets the
  dummy profit center, and a line keeps the derivation of its posting date,
  so it may differ from today's master after a change; SEGMENT comes from
  CEPC-SEGMENT unless a BAdI (FAGL_DERIVE_SEGMENT) or a manual entry set it
  or the profit center's segment changed after posting [M]. ACDOCA also
  carries KOKRS [H], blank for a company code without CO active. So T56h4
  is to check a line's profit center against the cost center period valid
  at its posting date and to read ACDOCA-KOKRS (leaf); a segment differing
  from its profit center's is loaded or refused as its own leaf decides,
  amending §5 if loading needs time-dependent segments (leaf).
- **Groups (A10, A11)**: TKA01-KHINR [H]; the profit centers' standard
  hierarchy is TKA01-PHINR [L], and the CEPC-KHINR derivation above stays
  the fallback until it is verified; the set tables as read [H]. A node may
  hold values beside subsets and a center's KHINR need not be a lowest node
  [M]: the gate's refusal of a group with both is **to lift** (leaf). Values
  as read [H for numeric codes]; only I with EQ or BT in groups kept in
  KSH2/KCH2 [M], other options (GS02) stay refused. SETHEADER-SETNAME holds
  24 characters (15 in the CO transactions) [M]: **to hold 24** (leaf).
- **Cost elements (A12)**: CSKB's fields and KATYP 01, 11, 12 confirmed
  [H] (others: 22, 41-43, 90); no CSKB row means no cost element [H],
  which SKA1-GLACCOUNT_TYPE (`N`, `P`, `S`, `X`) cross-checks. The slice
  valid at the cutover date is the one to use, not the latest [M]: **to
  change** (leaf), the latest only when none covers that date, flagged.
- **Not recorded here**: the answer on the ledger to extract (A16, RLDNR)
  is taken up by lane t56h3b with the OKB9 defaults (T56h3); the KOSAR
  keys the pilot uses wait for the pilot customer.

As built in T56h4 (no migration; ACDOCA's fields are §10's, pending the
SAP specialist, docs/migration.md):
- **Extract**: `internal/sap` ACDOCA carries RCNTR, PRCTR and SEGMENT. The
  mock's company 3000 books payroll on cost center 3100, its purchases
  (all before 3120 moves on 1 July) on 3120, each with its profit center
  and segment, and sales revenue on profit center P3100 alone. The OData
  mapping does not read them (their properties are not recorded; T56h5).
- **Which center a line names** (decided by default: the extract reads no
  controlling area on a line, and §10 maps SAP's area onto the tenant):
  the cost center of the line's company code (CSKS-BUKRS) and the profit
  center assigned to it (CEPC_BUKRS) of that code, a company code taken to
  belong to one area as T56h1 already takes it; one in two areas of the
  company code is refused by the gate rather than chosen.
- **Through the posting** (§5): `journalPosting` gives each line it posts
  the run's loaded centers (`integration.external_ref`), and the
  ledger's derivation and validation check them as any posting's, so a
  profit center other than the cost center's on the posting date, a
  center not valid or locked then, or a cost center on an account taking
  none refuses the document and fails the run. A center the load did not
  load (refused) refuses the document by name rather than posting it
  without. That is every document in documents mode; in balances mode
  (decided by default: §10 needs no centers for the opening entry, which
  carries balances, but the documents posted as entries of their own, an
  open down payment's and an open foreign item's, are SAP's lines, which
  keep their assignment) only those, whose lines the gate checks too.
- **Quality gate** (the leaf's two issues): a line's cost center not among
  its company code's in the extract, a profit center not assigned to it,
  or a center in two of its areas (not migrated); a segment that is not
  the profit center's, or a segment on a line without a profit center
  (decided by default: §5 takes a line's segment from its profit center,
  and SAP derives SEGMENT from PRCTR, so the gate does not derive the
  profit center from the cost center to check it).
- **Reconciliation** (documents mode, where either side has any; a
  balances run's opening entry carries its profit and loss without
  centers, so its totals could not agree): per
  company, SAP's lines on profit and loss accounts summed by RCNTR, PRCTR
  and SEGMENT in company code currency against the leading ledger's
  actuals of the loaded centers over the fiscal year, read through
  `ledger.AssignmentBalances` (`CostCenterActuals`, `ProfitCenterActuals`:
  the center reports' reads, §7), a segment's the sum of its profit
  centers' (the profit center report's segment rows). Totals per center
  over all accounts, not per center and account, as the leaf asks: the
  trial balance already compares each account. Three checks and their
  differences (`cost center total`, `profit center total`, `segment
  total`).
- **The ledger of SAP's lines** (review:T56h4): SAP holds a line once per
  ledger (ACDOCA RLDNR), so lines of a non-leading ledger would post, and
  add to every center total, twice. The SAP specialist (PO 2026-11-09)
  names the standard leading ledger 0L, to be confirmed in FINSC_LEDGER,
  and asks for the export to hold its lines alone with the load's refusal
  kept as a safety net. So the scope keeps the lines of its leading
  ledger (`sap.Scope.Ledger`, `sap.LeadingLedger` 0L when empty; not yet
  in the API's run scope, decided by default as no pilot is known to
  differ), counts the others' lines (`Extract.OtherLedgers`), and the
  quality gate refuses an extract holding any, naming each ledger and its
  lines, as the reconciliation does without computing a control. The
  OData mapping labels every line the leading ledger, so its ledgers are
  a T56h5 question.

As built in T56h3 (no migration; OKB9's tables and fields are the SAP
specialist's answers, PO 2026-11-09, to be validated in SE11 on the
pilot's release, docs/migration.md):
- **Extract**: `internal/sap` carries TKA3A (MANDT, BUKRS, KSTAR, KOKRS,
  KOSTL, AUFNR, PRCTR, REGEL, XGSBER, BSSUBST), TKA3C (keyed also by
  BWKEY and GSBER, blanks kept) and TKA3P (keyed also by PRCTR), kept by
  the scope's company codes. There is no released OData API for OKB9, so
  only the file extract carries them. The mock's company 3000 has one row
  per case below.
- **Where they go** (decided by default, as T56h2 settled CSKB: the
  chart and `default_assignments` are configuration, and §4's activation
  check reads them): into the configuration draft (`delivery/sapconfig`,
  `erp config generate`), not the run. The draft's notes are the issues
  the leaf asks of a quality gate, and the proposal the configuration
  change takes after the migration.
- **What is proposed** (the specialist's rule: the platform keeps one cost
  center per company and account): a TKA3A row with REGEL blank and a
  cost center (KOSTL) is the company's default for the account when the
  cost center is one the migration loads in the company (every CSKS
  slice of it in the company code and TKA3A's area, the latest
  open-ended without BKZKP, as §4 takes a default only valid without end
  and open for actual costs) and the account is in the company's chart
  and needs a cost center (`primary_cost`). It is not in the draft
  (review:T56h3 round 1): the draft is the tenant's first configuration,
  provisioned before the run loads the cost centers, and the ledger
  activates a default only for a cost center it holds (§4, T56c,
  `ledger.Setup.Check`). Its note gives the entry (`propose after the
  migration: {account, cost_center}`) for a posting-related change once
  the run has loaded the cost center, which the walkthrough's step 3
  proposes, has replayed and approved, and activates.
- **What is noted, not proposed**: REGEL 1, 2 or 3, naming the TKA3C or
  TKA3P rows SAP would read, and never TKA3A's own cost center as a
  fallback (the specialist: not to be assumed); another REGEL value;
  BSSUBST set (named beside REGEL when both are); an order-only or
  profit-center-only default, or none; a cost center the extract does not
  have in the company (or has in another company code too, which the run
  refuses), or whose latest slice ends or is locked; an account not in
  the chart or needing no cost center. An order or profit center beside a
  proposed cost center is noted as not proposed (a default holds a cost
  center alone). XGSBER is extracted and not used: it orders
  business-area assignment, which only REGEL 2 rows read, and those are
  not proposed.
- **History**: SAP keeps none (a change overwrites the row); the
  configuration's versions keep the platform's, so no snapshot table is
  added.
- **Not checked**: the orders and profit centers a row names (AUFK with
  JEST/JCDS, CEPC), as no such row is proposed; the ledger checks the
  cost center again when the change activates (`ledger.Setup.Check`).

As built for the SAP specialist's answers (lane sapctr, po:2026-11-09;
fixes of the leaves above, each named by its question):
- **TKA02 (A8)**: `internal/sap` carries TKA02 (BUKRS, KOKRS), kept by
  the scope's company codes, read only by the configuration draft. A
  company code's area is TKA02's (decided by default: the specialist
  answers that a company code belongs to exactly one area, which TKA02
  names), else, for an extract without it (no released OData API), the
  areas of its centers as before; the scope keeps the cost elements
  (CSKB) of the TKA02 areas too, but not their groups, which place
  centers and are kept by the areas of the centers loaded. The mock
  assigns 3000 to area 1000 and 1000 and 2000 to area 2000, which has no
  cost elements.
- **Centers of a line's posting date and area (A9(a), (b), (d))**: ACDOCA
  carries KOKRS, and a line's centers are those of its company code in
  that area (replacing T56h4's "a company code taken to belong to one
  area" and its refusal of a center in two areas); a line naming a center
  with KOKRS blank (CO not active) is refused. The gate, and the
  reconciliation reading the extract again, compare a line's profit center
  with its cost center's CSKS slice valid on BUDAT, not the latest, and
  refuse a cost center no slice of which covers BUDAT (decided by default:
  the ledger refuses both at posting, which fails the run late; the gate
  names them first). A slice without a profit center accepts the line's
  (SAP's dummy profit center, which the ledger takes beside a cost center
  without one). The mock's lines carry their company code's TKA02 area.
- **Values on inner groups (A10(d) [M])**: the gate no longer refuses a
  group with both subgroups and values, or a KHINR naming a group that is
  not a leaf; a center sits at the group whose values take it or that its
  KHINR names, inner or not (exactly one per tree, as before), and the
  ledger holds it there (§3 as amended, migration 000243). A group an
  earlier run loaded with centers takes the subgroups SAP added since.
- **Group codes of 24 characters (A11(c) [M])**: SETHEADER-SETNAME holds
  24, so the hierarchy's and the node's codes take up to 24 of 0-9, A-Z, _
  and - (migration 000244 widens 000139's checks; the contract's code
  patterns of the hierarchy list's filter, `HierarchyCreate` and
  `HierarchyNodeAdd` widen with them, a request-side change that breaks no
  client), and the gate's group code check with them.
- **A segment SAP did not derive from the profit center (A9(c) [M])**:
  refused, with an issue naming the case (the BAdI FAGL_DERIVE_SEGMENT or
  a manual entry), not loaded (decided by default: §3 gives a profit center
  one segment without validity and §5 every line its profit center's,
  which the journal line's foreign key holds; loading SAP's segment would
  need time-dependent or line-level segments, a change of §3 and §5 no
  decision asks for). §5 is not amended. A profit center whose segment
  changed after posting has slices naming two segments, already refused as
  a reorganisation. The pilot's lines are to be profiled for the case.

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

As built (T72n4, center changes on the company's date; product owner
2026-10-31: changes take effect from the company's date in its time zone,
not UTC's, as they decide which center a posting of that day takes):
- **Every place a center's validity is judged against a today.** (1) A
  center change's check that a default cost or profit center stays usable
  from today on (409 `ledger.default_center_in_use`), at the request
  (`redate`/`lock` of either kind) and again at the approval
  (`CenterChanges`); (2) the setup's check of the controlling rules
  (`readControlling`, Check and Apply): whether a default center is valid
  and unlocked from today, and the fiscal year of today that center
  accounting is turned on from; (3) a requisition's account assignments
  as it is submitted (§6 as built: "on that day"), until now the
  database's `current_date`. Each now judges on the company's date in its
  `companies[].time_zone` (UTC when it sets none), with `clock.DayIn` on
  the server's clock (`Endpoints.Now`, `CenterChanges.Now`, `Setup.Now`,
  procurement's `Endpoints.Now`). The ledger reads the zone from the
  active configuration (`ledger.Settings`, wired to
  `configuration.Settings`, the port payments and procurement use) by the
  legal entity's code (`config.CompanyLocation`); the setup reads it from
  the document it checks or applies, the zone that document puts in
  force. A profit center may be the default of companies in several
  zones: each judges it on its own date. A handler wired without the
  configuration, or a stored zone the binary cannot load, fails the
  change rather than judging it in UTC. The setup's check of a document
  whose zone does not load leaves the date-dependent controlling checks
  for that company out: validation reports the zone
  (`companies[<code>].time_zone`, 422), and the application, which only
  sees validated documents, fails.
- **Left as they are.** A posting derives and validates its centers on its
  own posting date (§5), an order on its order date, an order change on
  its changed order date and a supplier invoice on its posting or invoice
  date (`CheckAssignments`): those dates are the documents', chosen by
  their callers, and no clock or zone is involved. A posting date that a
  caller leaves out and the context defaults to the database's
  `current_date` (a supplier invoice posted or reversed without one, a
  goods receipt reversed without one) is dated in UTC; that default
  decides the period and the rates too, beyond centers, and is not this
  step's (reported for the product owner). Hierarchies are not
  time-dependent ("today's tree", decision P7) and read no date.
- **What the consumer sees.** The refusal's `detail` names the date and
  the zone (`… after the change it is locked from 2026-04-16 (the
  company's date in America/Toronto) …`), as do the setup's findings;
  to a caller who cannot access the company (a requester locking a
  profit center an inaccessible company defaults to, an approver deciding
  a change of a cost center in one) it says "the date of a company you
  cannot access" instead, as the refusal already leaves out how many
  companies default to a profit center; a
  requisition's refusal at submission says it was checked on the
  company's date and names the date and the zone (integration of batch
  26, once T72n5's `today` helper gave the zone). The problem schema has no extension
  members, so they are in the detail, as T72n3's are. The contract's
  descriptions of `redateCostCenter`, `lockCostCenter`,
  `redateProfitCenter`, `lockProfitCenter`, `submitRequisition` and the
  requisition item's `account_assignment` say so.
- **Determinism.** The checks run in requests and in the approval's
  deciding transaction; no workflow code reads a clock.
- **No migration.** The zone is configuration (ADR-0051 as built T72n1);
  nothing is stored.
- **Walkthrough.** Not extended: the step changes the time base of
  existing rules, its server runs on the host's clock, and its company has
  no default centers (step 12 creates centers without turning center
  accounting on), so neither refusal can be shown at a chosen hour; the
  package tests (`TestCenterChangesOnTheCompanysDate`,
  `TestControllingDefaultsOnTheCompanysDate`,
  `TestSubmissionOnTheCompanysDate`) fix the clock at 22:00 in Toronto and
  01:00 in Dubai, each failing when judged in UTC.

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
| T56d | **Assignment on the journal** (built with T56d2; the commit-time check is T56d3, see §5 as built) | The in-use refusals T56a leaves out: `ledger.cost_center_period_in_use` (a change whose periods would differ from what posted lines read), `ledger.cost_center_posted_after` (validity ended before a posting), `ledger.segment_in_use` (a posted profit center's segment); expand migration (columns, FKs NOT VALID then validated per partition, partial indexes concurrently); derivation and validation in every posting path incl. parked and `Postings`; commit-time check of §5's table; reversals copy; T52 copies carry the same; errors and constraint mappings as §5; journal API, list filters and event fields (additive); `FOR SHARE` on center periods with a race test against a lock |
| T56d2 | **Assignment in the journal API; in-use refusals** (released with T56d; built, see §5 as built) | `ledger.cost_center_period_in_use`, `ledger.cost_center_posted_after`, `ledger.segment_in_use` (attributes in force on each posted date); journal API lines with the three ids and codes, entered on manual and parked entries (stored on parked lines), list filters `cost_center_id`/`profit_center_id`, `JournalEntryPosted` fields (additive, openapi and asyncapi) |
| T56d4 | **Profit center changes against posted lines; same-kind lock check** (built, see §3 as built) | Product owner 2026-10-25: a cost center lock change compared with posted lines of its kind only (cost lines on expense accounts, revenue lines on revenue accounts); a reversal after a new end counts; a profit center's redate or lock refused against the lines carrying it, 409 `ledger.profit_center_posted_after` / `ledger.profit_center_period_in_use` (additive, x-problem-codes), at the request and again at the approval |
| T56d3 | **Commit-time check of §5's table** (contract step: the release after the one shipping T56d and T56d2) | Deferred constraint trigger on `journal_line` holding the table for fiscal years from the switches, the segment always the profit center's, reversals exempt; constraint names mapped to the §5 problems (`ledger.profit_center_required` for a P&L line without one); schema tests per rule |
| T56e | **Procurement and payables** (built, see §6 as built; the capture agent's proposals split to T56e2) | `account_assignment` and `cost_center_id` on requisition and PO items (validated at release), GR and price-difference lines with the item's center; invoice items' `cost_center_id`/`profit_center_id`; capture agent proposals with eval cases; the free-text custom field in fixtures replaced; ADR-0034 note |
| T56e2 | **Capture agent proposals** (split from T56e: capture enters only order items today, which take their order item's center, so proposals need captured items without an order first) | The capture agent proposes a cost center per item without an order from the supplier's posted history and the document text, confirmed by a clerk or the default; ADR-0039 eval cases (documents and expected proposals) in `evals/capture` |
| T56f | **Actual reports** (built, see §7 as built) | `ledger.assignment_balance` maintained by the projector (exactly once, run-twice test); `GET /v1/cost-center-balances`, `/v1/profit-center-balances` (by center, node, segment, account, period; exact at the read); schema test that assignment balances add up to `period_balance` on P&L accounts; drill-down via journal filters |
| T56g | **Plan and plan/actual** (P5) | `controlling` context and schema; plan categories with a human-only lock; plan lines by upload and API, idempotent by batch; `GET /v1/cost-center-plan-actual`; actuals through `ledger.AssignmentBalances` |
| T56h | **SAP migration** (split: T56h1 centers and segments, built, see §10 as built; T56h2 hierarchies and categories; T56h3 OKB9 defaults; T56h4 ACDOCA's centers and center totals) | Extract and load of CSKS/CSKT, CEPC/CEPCT/CEPC_BUKRS, FAGL_SEGM(T), CSKB categories, OKB9 defaults, sets 0101/0106 with ranges expanded; ACDOCA RCNTR/PRCTR/SEGMENT; quality-gate issues (center outside the standard hierarchy, range matching nothing, unknown profit center); mock generator extended; reconciliation of migrated center totals |
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
