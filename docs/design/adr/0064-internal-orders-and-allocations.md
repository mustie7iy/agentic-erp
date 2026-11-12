# ADR-0064: Internal orders and allocations (controlling II)

**Status:** accepted (T60a, 2026-11-11; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding). It is the ADR that ADR-0050 §2 and
its boundary with T60 ask for. It decides at the level the slices T60a-T60g
of `docs/design/plan-breadth-week1.md` need; each slice records what it
built under "As built".

## Context
ADR-0050 gave the ledger cost centers, profit centers and segments as
dimensions of the journal, and a `controlling` context for plan values. It
left to T60: internal orders "as a second real cost object (so 'exactly one
real object' becomes a choice), secondary cost elements, assessment and
distribution cycles posting through the ledger with sender/receiver
centers, settlement" (ADR-0050, Boundaries), and one open question: whether
the order master "is controlling's behind a ledger port
(`ledger.CostObjects`, like `Partners`) or the ledger's" (ADR-0050 §2).

The product owner's breadth-first decision (2026-11-10) asks for a first
usable version of every capability by 2026-12-01, the main path only. The
plan splits the tracker's groups Internal orders (CO-OM-OPA) and Overhead
allocation into seven slices: T60a order created and released, T60b costs
posted to an order, T60c budget against actual, T60d settlement rule, T60e
settlement and close, T60f distribution cycle, T60g assessment cycle. What
each leaves for after 1 December is in the plan's "Later" paragraphs and
the tracker's "later:" leaves; none of it is decided here.

What exists: `controlling` (schema, outbox, plan categories and batches,
T56g) depends on the ledger and reads it through
`ledger.AssignmentBalances`; the ledger's derivation (`assign`,
`assignment.go`) gives every line its cost center, profit center and
segment in `postAll`, which every posting path ends in; `ledger.Postings`
is the port other contexts post through, with `source_context` and
`source_document_id`; `cost_element: secondary` is reserved in the
configuration and refused (ADR-0050 §4).

## Decision

### 1. The order master is controlling's
**Internal orders live in the controlling context** (table
`controlling.internal_order`), not in the ledger. An order has a lifecycle
(created, released, later budgeted, settled and closed) that is
controlling's work, and none of the journal's own invariants needs it as a
table of the ledger's schema: what a posting needs to know of an order is
small and stable (it is released, it is of the line's company, its
responsible cost center), and the ledger asks for it through a port (§2).
This is the alternative ADR-0050 rejected for centers and left open for
orders; the reasons it was rejected for centers (validity periods read by
foreign key in every posting, one leaf per center in a hierarchy) do not
apply to an order.

An order belongs to one legal entity and has a **code** unique in it
(`^[0-9A-Z_-]{1,12}$`, entered; number ranges and order types come later), a
**name**, a **responsible cost center** (an active cost center of the same
company, checked through `ledger.AssignmentBalances`) and a **status**:
`created`, then `released`. T60e adds `closed`. Company and code never
change, and a release is not taken back; the database holds both.

- **Created and released directly by permission**, with no approval
  request (plan default 9): `controlling.order.write` creates,
  `controlling.order.release` releases, `controlling.order.read` reads.
  Release reads the order `FOR UPDATE` at the caller's version
  (`If-Match`, ADR-0008) and moves it once.
- **People only.** Creating and releasing are `x-agent: false`, and
  `controlling.order.release` is an approval permission (`is_approval`),
  which agents cannot hold, as locking a plan category is (ADR-0050 §12):
  master data that decides where costs may be posted is a person's. Agents
  read orders.
- **Company access** as everywhere (04 §4.9): an order of a company the
  caller may not act in is answered as missing.
- Each change is audited (ADR-0011) and published as
  `InternalOrderChanged` through the controlling outbox (ADR-0007).

### 2. The order is the second real cost object, behind `ledger.CostObjects`
`ledger.journal_line` gains `internal_order_id` (nullable, no default, **no
foreign key**: the order is another schema's; a partial index per
partition for the order's lines). The ledger owns an interface,
`ledger.CostObjects`, that the controlling context implements and
`internal/app` wires, exactly as `ledger.Partners` is implemented by the
partner context: the ledger asks, for the orders its lines name, each
one's company, code, whether it takes postings, and its responsible cost
center.

**Dependency direction.** Unchanged: `controlling` imports `ledger`
(ADR-0050 §2); `ledger` imports no context. The port is an interface in
the ledger's published API, so the ledger calls controlling without
importing it. `internal/layout`'s rules hold as they are.

**The rule** (ADR-0050 §5's table, its `primary_cost` row): a line on a
primary cost account carries **exactly one real cost object, a cost center
or an order**.
- A line naming an order must not name a cost center (422, naming the
  line). The order must be known, of the line's company and released (422
  naming the line and why).
- The line's **profit center and segment derive from the order's
  responsible cost center**, in its period on the posting date, as they
  derive from a line's own cost center; the line's `cost_center_id` stays
  empty, so the cost center reports and the order's actuals never show the
  same amount twice.
- An order on a line whose account takes no cost object (`cost_element:
  none`) or on a revenue account is refused in the first version: an order
  collects costs.
- The responsible cost center's locks are for its own postings and do not
  refuse a line posted to the order.
- A **reversal copies** the original's order and is not checked again, as
  it copies centers (ADR-0050 §5); since T60e it holds the order as a
  posting does, and an entry on an order closed since is not reversed.
- Derivation runs once per posting group, so every ledger's copy carries
  the same order (ADR-0045).
- The posting holds the orders it names until its transaction ends, so a
  status change (which takes the order `FOR UPDATE`) waits for postings
  under way and postings after it see it.

**Actuals** of an order are the leading ledger's lines carrying it, by
account and period, read by the ledger and shown by controlling
(`GET /v1/internal-order-balances`), with totals.

### 3. Budget, settlement rule, settlement (T60c-T60e)
- **Budget**: one overall amount per order in the company's currency
  (`controlling.order_budget`), changed with the caller's version and
  audited with the amounts before and after. The read shows budget, actual
  and what remains and says when the actual exceeds the budget; **no
  posting is refused for it** (plan default 10; question 1 below).
- **Settlement rule**: one receiver per order, a cost center of the
  order's company, taking the whole (`controlling.settlement_rule`).
- **Settlement** posts through `ledger.Postings.Post` with
  `source_context` `controlling`: per account, a credit on the order and a
  debit on the receiving cost center for the order's unsettled balance up
  to a period, so each account's balance is unchanged, as the manual
  reposting between centers of ADR-0050 §5 (plan default 11; question 2).
  Settling again with nothing new posts nothing. An order with a zero
  balance may be **closed**; a closed order takes no posting.

### 4. Secondary cost elements (T60g)
A secondary cost element is **an account of the chart with `cost_element:
secondary`**, which the configuration accepts from T60g. The ledger takes
lines on such an account only from entries whose `source_context` is
`controlling`; a manual entry on one is refused, and the database holds
the rule. Primary costs stay on their accounts. Every entry on it
balances on the account itself, so **it nets to zero in the company** and
never carries a balance; what it changes is which cost center, and so
which profit center and segment, bears a cost. What follows from that for
balances, statements and the other contexts is under "As built", T60g.

### 5. Cycles post through the ledger (T60f, T60g)
A cycle (`controlling.allocation_cycle`, its receivers and its runs) has
one sender cost center and receiver cost centers with fixed percentages
adding up to 100, all active and of one company.
- A **distribution** moves the sender's primary costs of a period to the
  receivers on their original accounts; an **assessment** charges them in
  one amount on the cycle's assessment account, a secondary cost element.
- Shares are split with `money.Allocate`, so they add up to the sender's
  amount to the minor unit (ADR-0005).
- A run is one balanced entry through `ledger.Postings.Post`
  (`source_context` `controlling`), **one run per cycle and period**,
  synchronous, not reversible in the first version (plan default 12). A
  sender without costs posts nothing.
- An assessment leaves the sender's primary costs on their accounts, so
  **a sender assessed for a period is not run for it again**, by any of
  its cycles: a second run would charge the same costs twice (T60g).

## Alternatives
- **The order master in the ledger**, with foreign keys from the journal
  line. The database would hold "the order is of the line's company", but
  the ledger would own a lifecycle (budget, settlement, close) that is not
  the journal's, and every later order slice would change the ledger.
  Rejected; the port keeps the check in one place and the order's company
  and status are held for the posting's transaction by a row lock.
- **The order as a custom field** (ADR-0046). No release, no derivation of
  the profit center, no balances. Rejected, as for cost centers.
- **A statistical order beside a real cost center on one line.** SAP
  allows it; it is in the plan's "Later" list and not built.
- **Allocations as controlling's own lines, outside the journal.** Reports
  would have to merge two sources and the ledger would no longer be the
  only place a cost is recorded. Rejected: cycles and settlements are
  journal entries (ADR-0050 §2, the universal journal's shape).

## Consequences
- Posting can be refused for an order (new 422s), which clients handle as
  they handle center refusals.
- Every posting that names an order calls the controlling context once; a
  posting that names none does not.
- The journal line's order has no foreign key: the application check, in
  `postAll`, is the only writer's check, as partner blocks are.
- Cost center reports do not include costs posted to orders until they are
  settled to a cost center (T60e).

## Decided by default (ADR-0053 d), for the product owner to overrule
1. Orders are created and released by permission, without an approval
   request (plan default 9).
2. Creating and releasing an order are people's: `x-agent: false`, and
   `controlling.order.release` is an approval permission.
3. An order's code is entered, up to 12 characters of `0-9`, `A-Z`, `_`
   and `-`, unique per company.
4. The order response and the event carry the responsible cost center's
   code beside its id.
5. An order, once created, is not changed or deleted in the first version
   (changes by approval are in the plan's "Later").

## Questions for the product owner or a specialist (none blocks a slice)
1. **Order budget** (product owner; plan question 5): should a posting
   that exceeds the budget be refused or warned about?
2. **Settlement and assessment accounts** (accounting specialist; plan
   question 6): is settlement on the original accounts acceptable, and a
   secondary cost element kept as an account in the chart?
   Since T60e also: a settlement is posted in local currency and its
   group amount converted at the settlement date's rate, so an order's
   balance in group currency need not be zero afterwards; and it settles
   the leading ledger's balance in every ledger the company keeps. Is
   either to be settled differently?
   Since T60g also, each stated here without a source and for the
   specialist to confirm: a secondary cost element is an expense account
   of the chart (SAP S/4HANA keeps secondary cost elements as G/L
   accounts of the type "secondary costs", ADR-0050 "What SAP does";
   this chart has no such type); it carries no balance, so it is in no
   opening balance, no carry-forward and no revaluation, and reads as
   zero in a company's statements; an assessment charges the sender's
   primary costs only, not what other cycles charged to it; one lock on a
   cost center, for actual costs, refuses primary and secondary costs
   alike (SAP locks them apart, `docs/migration.md`, BKZKS).

## As built

### T60a: an internal order created and released (migration 000267)
- **Table** `controlling.internal_order`: `legal_entity_id`, `code`
  (`internal_order_code_once`, unique per tenant and company), `name`,
  `responsible_cost_center_id`, `status` (`created`, `released`),
  `version` and the audit columns; row-level security; never deleted. A
  trigger (`internal_order_lifecycle`) holds that an order starts as
  `created`, keeps its company and code, and stays released. Schema test:
  `db/internal_order_test.go`.
- **API**: `POST /v1/internal-orders` (201; 409
  `controlling.internal_order_exists`; 422
  `controlling.internal_order_cost_center_invalid` with the field error's
  code `unknown`, `not_in_company` or `not_active`; a legal entity the
  caller may not act in is 422 `request.reference_not_found`),
  `GET /v1/internal-orders` (filters `legal_entity_id`, `code`, `status`,
  `responsible_cost_center_id`; cursor paging, by `code`),
  `GET /v1/internal-orders/{id}`, `POST /v1/internal-orders/{id}:release`
  (`If-Match`; 409 `controlling.internal_order_released`).
- **The cost center** is read through a new method of the ledger's port,
  `ledger.AssignmentBalances.CostCenterRefs` (company, code, active), used
  for the check and to name the center on every order shown. An active
  cost center stays active, so the read takes no lock.
- **Event** `InternalOrderChanged` (`created`, `released`); audit actions
  `controlling.internal_order.create` and `.release`.
- **Permissions** `controlling.order.read`, `controlling.order.write`,
  `controlling.order.release` (approval).
- **Walkthrough**: `step_internal_order`, Carla creates and releases
  Maple's order `FAIR-<year>` under cost center CC-1100.

### T60b: costs posted to an internal order (migrations 000268-000270)
- **Column** `ledger.journal_line.internal_order_id`, nullable, no foreign
  key; the check `journal_line_one_cost_object` (a cost center or an
  order, never both), added `NOT VALID` (000268) and validated partition
  by partition (000269); the partial index
  `journal_line_by_internal_order` per partition (000270,
  `migrate:partitioned-index`). Schema test: `db/internal_order_test.go`.
- **The port** `ledger.CostObjects` (`Orders(ids, hold)`, in
  `assignment.go`), implemented by `controlling.CostObjects` and wired in
  `internal/app` on the ledger's API endpoints. With `hold` it reads the
  orders `FOR SHARE` in id order. A posting whose lines name no order asks
  nothing. A line naming an order where the port is not wired
  (`ledger.Postings` and `ledger.ManualEntries`, whose lines cannot name
  one yet) is an error, never a line posted unchecked.
- **Derivation** (`assignOrders` before `assign`, in `postAll`): the order
  is known, of the posting's company and released, and the line names no
  cost center; the account is a primary cost account; the profit center
  and segment are the responsible cost center's in its period on the
  posting date (a different profit center given: 422
  `ledger.profit_center_mismatch`; a center without a profit center leaves
  the company's default, as for a cost center). Refusals are 422
  `ledger.internal_order_invalid` on `lines[i].internal_order_id` with the
  reason `unknown`, `not_in_company`, `not_released`, `with_cost_center`,
  `not_a_cost_account` or `cost_center_not_valid_on_date`. The responsible
  cost center's own locks (`lock_actual_cost`) do not refuse the order's
  line: the order is the cost object. The center and its periods are
  read under the posting's locks, as a line's own cost center is.
- **Decided by default:** an order is posted only on primary cost
  accounts (no revenue, no balance sheet line); the default cost center of
  an account is not taken by a line naming an order; an order's line is
  accepted whether or not cost center accounting is on for the year.
- **Parked entries and entry templates keep no order** in this version: a
  line naming one is refused (422 `ledger.internal_order_not_parkable`),
  also when a service principal's entry above its limit would be parked,
  rather than parked without it. Orders on kept lines are a gap for the
  product owner (not in the plan's lists).
- **API**: `internal_order_id` on `JournalLineInput`; `internal_order_id`
  and `internal_order_code` on `JournalLine` (the code read through the
  port after the entry's ETag is taken; an order is never deleted);
  `JournalEntryPosted` lines carry `internal_order_id`. Additive
  (ADR-0041). `ledger.PostingLine` gains no order yet: T60e adds it with
  the settlement, which is the first posting of another context on an
  order.
- **Actuals**: `GET /v1/internal-order-balances` (`controlling.order.read`
  and `ledger.entry.read`): one order's actual costs of a fiscal year per
  account and period from the leading ledger's lines
  (`ledger.AssignmentBalances.OrderActuals`), debit, credit and balance in
  the company's currency, with the order's code and name and the
  selection's totals on every page; cursor over account number, account
  and period. An order unknown or of a company outside the caller's access
  is 422 `request.reference_not_found`. Budget:
  `TestInternalOrderBalancesBudget` (p95 1 s; 20,001 lines of an order
  among 400,002).
- **Walkthrough**: `step_internal_order` extended: Alex posts CAD 1,500.00
  to the order, a line with an order and a cost center is refused, and
  Carla reads the order's actuals.

### T60c: an order's budget against its actual costs (migration 000271)
- **Table** `controlling.order_budget`: at most one row per order (the
  primary key; `order_budget_order`, a foreign key to the order), `amount`
  in minor units of the company's currency (`order_budget_not_negative`),
  `version` and the audit columns; row-level security; never deleted. An
  order without a row has no budget. Schema test:
  `db/internal_order_test.go` (`TestOrderBudgetRules`).
- **API**: `PATCH /v1/internal-orders/{id}/budget` sets or replaces the
  budget (200 with the budget against the actual; 412
  `request.version_mismatch`; 422 `controlling.order_budget_invalid` on
  `budget.currency` or `budget.amount`; 404 for an order unknown or of a
  company outside the caller's access). `GET
  /v1/internal-orders/{id}/budget` gives `budget`, `actual`, `remaining`
  (the budget less the actual, negative once exceeded) and `exceeded`
  (the actual is above the budget), each amount with its currency, with
  the order's code and name.
- **`PATCH`, not the plan's `PUT`.** The plan names `PUT` for the budget
  and the settlement rule; the API has no `PUT` (04 §4.2: `GET`, `POST`
  and `PATCH` alone, held by `api` `TestOperations`), and a part of a
  resource changed with `If-Match` is a JSON Merge Patch elsewhere (the
  tenant's auth policy). The paths are the plan's; the body's one field,
  `budget`, is required and replaced whole.
- **The caller's version** is the budget's own, not the order's: `GET`
  answers it as `ETag` and in `version`, 0 for an order whose budget was
  never set, so the first `PATCH` sends `If-Match: "0"`. The order is read
  `FOR UPDATE` for the change, so two changes at one version serialize
  and the second is refused; the order's own version does not move, and a
  budget change does not disturb a release.
- **The actual** is the order's balance in the leading ledger over every
  fiscal year, since the budget is one overall amount: a new method of
  the ledger's port, `ledger.AssignmentBalances.OrderBalance`, read from
  the lines through the order's index (000270); it is the total of `GET
  /v1/internal-order-balances` over the years. Budget:
  `TestInternalOrderBudgetReadBudget` (p95 1 s; 20,001 lines of an order
  among 400,002).
- **Nothing is refused** (plan default 10): a posting beyond the budget
  is accepted, and a budget may be set below the actual.
- **Event** `InternalOrderBudgetChanged` (`budget`, `previous_budget`
  when there was one, `version`); audit action
  `controlling.internal_order.set_budget` on the order, with
  `budget_before` (when there was one), `budget_after` and `currency`.
- **Permission** `controlling.order.budget` (not an approval permission).
- **Decided by default:** setting a budget is a person's (`x-agent:
  false`), as creating and releasing an order are (§1); agents read it.
  Both operations also need `ledger.entry.read`, as the order's balances
  do, because the answer carries the actual, a ledger amount. A budget
  is zero or more, zero included; it may be set on an order in any
  status; it is replaced, never removed; a `PATCH` of the amount already
  set is a change like any other (a new version, audited and published).
- **Walkthrough**: `step_order_budget`, Carla gives `FAIR-<year>` a
  budget below its costs (shown as exceeded, CAD -500.00 remaining),
  corrects it to CAD 5,000.00 and reads CAD 3,500.00 remaining after the
  CAD 1,500.00 posted in `step_internal_order`.

### T60d: an order's settlement rule (migration 000272)
- **Table** `controlling.settlement_rule`: at most one row per order (the
  primary key; `settlement_rule_order`, a foreign key to the order),
  `receiver_cost_center_id` (the ledger's, no foreign key), the audit
  columns and a `version` that only counts the row's replacements (an
  updatable table has one, `platform.touch`); row-level security; never
  deleted. Schema test: `db/internal_order_test.go`
  (`TestSettlementRuleRules`).
- **API**: `PATCH /v1/internal-orders/{id}/settlement-rule` (the plan's
  `PUT`, as for the budget) sets or replaces the receiver (200 with the
  rule; 412 `request.version_mismatch`; 422
  `controlling.settlement_rule_receiver_invalid` on
  `receiver_cost_center_id` with the reason `unknown`, `not_in_company`,
  `not_active` or `not_valid_today`; 404 for an order unknown or of a
  company outside the caller's access). `GET
  /v1/internal-orders/{id}/settlement-rule` gives the receiver's id and
  code with the order's code and name; an order without a rule is
  answered 200 without a receiver, so the read always carries the `ETag`
  the `PATCH` takes.
- **The order's version** is the caller's (the plan: "replaced with the
  order's version"): `If-Match` names it, the order is read `FOR UPDATE`,
  and its version moves with the rule, so of two changes at one version
  the second is refused, and a release and a rule change at one version
  do not both pass.
- **The receiver** is checked when the rule is set: known, of the
  order's company, active (`ledger.AssignmentBalances.CostCenterRefs`)
  and with a period in force today (a new method of the port,
  `CostCenterValidOn`). Today is the company's date in its time zone
  (`config.DocumentDay` over `controlling.Settings` and the endpoints'
  clock, wired in `internal/app`), as center changes are judged
  (T72n4). The check holds no lock: a center change may end the period
  later, and the settlement (T60e) posts to the receiver on its own
  date, where the ledger checks it again.
- **Audit** action `controlling.internal_order.set_settlement_rule` on
  the order, with the receiver's id and code before (when there was one)
  and after. **No event**: the plan adds none for the rule; T60e
  publishes the settlement.
- **Decided by default:** setting the rule needs
  `controlling.order.write` (the plan adds no permission for it) and is
  a person's (`x-agent: false`), as creating an order is (§1); agents
  read it. The receiver may be the order's responsible cost center. The
  rule may be set on an order in any status; it is replaced, never
  removed. A `PATCH` naming the receiver the rule already has is a change
  like any other: both versions move and it is audited, as for the
  budget. The rule shows the receiver's code beside its id, as the order
  shows its responsible cost center (§1, default 4).
- **Walkthrough**: `step_order_settlement`, Carla names CC-1100 as the
  receiver of `FAIR-<year>`; a cost center that is none and a change at
  the replaced version are refused.

### T60e: an order settled and closed (migrations 000273-000275)
- **The journal takes controlling's entries.** `ledger.journal_entry`'s
  list of source contexts gains `controlling` (000273 replaces the check
  `NOT VALID`; 000274 validates it partition by partition, as T60b's
  check was), and the API's open enumerations of `source_context` name
  it. Schema test: `db/internal_order_test.go`
  (`TestJournalSourceControlling`).
- **Table** `controlling.order_settlement` (000275): the order (foreign
  key), its company, the fiscal year and period settled up to, the
  posting date, the receiver, the journal entry (once,
  `order_settlement_entry_once`) with its document number, the amount
  moved in minor units of the company's currency and the number of
  accounts; row-level security; insert-only (no `UPDATE` or `DELETE` is
  granted). The order's status gains `closed`, from `released` alone and
  for good (`internal_order_lifecycle`). Schema test:
  `TestOrderSettlementRules`.
- **The ledger's port** (the files touched: `ports.go`,
  `assignmentbalances.go`, `assignment.go`, `journal.go`).
  `ledger.PostingLine` gains `InternalOrderID` and `ledger.Postings` a
  `CostObjects` field, which `internal/app` wires for the controlling
  context alone; a line naming an order is checked as a manual entry's
  is, and without the port it is an error. A new read,
  `ledger.AssignmentBalances.OrderAccountBalances`, gives the order's
  balance per account, the accounts whose balance is not zero in account
  number order, over every period up to one named (earlier years
  included) or over all. `ledger.CostOrder` gains `Closed`: a line naming
  a closed order is refused with the reason `closed` (422
  `ledger.internal_order_invalid`).
- **Settlement** (`POST /v1/internal-orders/{id}:settle`, body
  `fiscal_year` and `fiscal_period`): the order is read `FOR UPDATE`; it
  must be released (409 `controlling.internal_order_not_released`, 409
  `controlling.internal_order_closed`) and have a rule (409
  `controlling.settlement_rule_missing`); the period is a regular posting
  period of the company's year (`ledger.Calendar.Periods`; 422
  `ledger.no_fiscal_period`, 422 `controlling.period_not_regular`). The
  unsettled balance is the order's balance per account up to the period.
  With none, nothing is posted, kept or published and the answer says
  `settled: false`. Otherwise one entry is posted through
  `ledger.Postings.Post` (`source_context` `controlling`, document type
  `SA`, local currency, dated the period's last day, its source document
  the settlement): per account, in account number order, the order
  credited with its balance and the receiver debited with it. The entry
  balances per account, so each account's balance is unchanged. The
  settlement is kept, audited (`controlling.internal_order.settle`) and
  published (`InternalOrderSettled`); the answer carries it with its
  entry's id and number. Because the settlement's own lines are in the
  period, the order's balance up to it is zero afterwards and a second
  settlement of that period posts nothing; a cost posted later into a
  settled period is unsettled again and moves with the next settlement.
  A period **before** the one the order was last settled up to is
  refused (409 `controlling.settlement_period_before_last`, review round
  1): that settlement's credit is dated its own period, so the balance up
  to an earlier period still holds what it moved and settling it would
  move the same cost twice. The last period is read from the order's
  settlements under the order's lock.
- **`GET /v1/internal-orders/{id}/settlements`**: the order's
  settlements, latest first, by cursor (`posting_date`), each with the
  receiver's code, the amount with its currency and the entry.
- **Close** (`POST /v1/internal-orders/{id}:close`, `If-Match` the
  order's version): the order is read `FOR UPDATE`, must be released,
  and its balance must be zero on every account over every year (409
  `controlling.internal_order_not_settled`, naming an account and its
  balance); it moves to `closed` once, audited
  (`controlling.internal_order.close`) and published as
  `InternalOrderChanged` (`closed`).
- **A closed order takes no posting**: a line naming it is refused (the
  reason `closed`), and so is the reversal of an entry that posted to it
  (409 `ledger.internal_order_closed`): a reversal copies its original's
  order without deriving it again (§2), so `postAll` now holds the orders
  a reversal's lines carry `FOR SHARE` and refuses a closed one; its
  balance was zero when it was closed and nothing settles it afterwards.
- **The lock order** (T60b's review asked for it to be settled here). A
  posting takes its period statuses `FOR SHARE`, then the orders its
  lines name `FOR SHARE`, then its centers, then the number range
  (`postAll`). A settlement takes its order `FOR UPDATE` first and then
  posts, so inside it the same order follows: period statuses, the order
  again (its own lock), centers, number range. A close takes the order
  `FOR UPDATE` and nothing after it but its own update. The order stays
  acyclic: the only locks a posting can hold while it waits for an order
  are its period statuses, `FOR SHARE`, which the settlement takes `FOR
  SHARE` too, and a new `FOR SHARE` does not queue behind a period
  close's waiting `FOR UPDATE` (03 §3.7, entry locks); a posting never
  holds a center lock or the number range while it waits for an order,
  since it takes them after. A settlement therefore never waits for a
  posting that waits for it. What the lock gives: a posting to the order
  is either committed before the settlement reads the balance (and
  moved) or waits for the settlement (and stays unsettled until the
  next); a posting after a close finds the order closed; two settlements
  never move one balance. Proved by
  `TestInternalOrderSettlementLockOrder` (a posting held waiting for the
  order while the settlement posts) and `TestInternalOrderSettledOnce`.
- **Not a report read**: the settlements are a plain listing of a
  table by its index, so they get no budget test (07 §7.7 item 7).
- **Permission** `controlling.order.settle` (not an approval
  permission), for settling and for closing.
- **Decided by default:**
  1. A settlement names a fiscal year and period, and its entry is dated
     the period's last day (as a depreciation run's, ADR-0057 §7).
  2. `:settle` takes `If-Match` (every custom method does, 04 §4.7): the
     order's version, which moves with its rule, so an order whose
     receiver was replaced since the caller read it is refused (412). A
     settlement does not move the version.
  3. Settling and closing are a person's (`x-agent: false`), as the
     other order writes (§1), under the one permission the plan adds.
     Settling, closing and reading settlements also need
     `ledger.entry.read`, since they show ledger amounts, as the budget
     does (a close's refusal names the balance left).
  4. The receiver is checked on the period's last day before anything is
     posted (409 `controlling.settlement_receiver_not_valid`: replace
     the rule); a receiver locked for actual costs, a closed period and
     every other refusal of the entry are the ledger's own problems.
  5. A credit balance is settled as one: the amount is negative.
  6. Only a released order is closed; one never released is not.
  7. The order's line keeps the profit center and segment of the order's
     responsible cost center and the receiver's line takes the
     receiver's, so a settlement to a cost center of another profit
     center moves the cost between profit centers.
  8. A settlement is not reversed (plan, Later): the ledger refuses to
     reverse an entry another context posted.
  9. An order is settled up to the period of its last settlement or a
     later one, never an earlier one (SAP's settlement likewise does not
     go back before the last settlement period; stated from memory, for
     the specialist to confirm). Once that period is closed in the
     ledger, a cost posted later is settled up to a later, open period,
     the next fiscal year's if need be.
- **Known limits, for a specialist** (question 2 below): the entry is in
  local currency and its group amount is converted at the settlement
  date's rate, so an order's balance in group currency need not be zero
  after settlement when rates moved since its costs were posted; and the
  balance settled is the leading ledger's, posted to every ledger the
  company keeps, so a cost posted to an order in one ledger alone is not
  settled ledger by ledger.
- **Walkthrough**: `step_order_settlement` extended: the unsettled order
  cannot be closed; Carla settles `FAIR-<year>` up to the current period
  (CAD 1,500.00 to CC-1100 on account 510000, the entry's two lines
  shown), the order's balance is zero and CC-1100's report shows the
  cost; settling again posts nothing; the order is closed and a posting
  to it is refused (`closed`).

### T60f: a distribution cycle run (migration 000276)
- **Tables.** `controlling.allocation_cycle`: a company's cycle with a
  code unique in it (`allocation_cycle_code_once`), a name, a `kind`
  (`distribution` alone) and its sender cost center; it keeps its company,
  code, kind and sender (`allocation_cycle_identity`).
  `controlling.allocation_receiver`: a cycle's receivers in their order,
  each cost center once (`allocation_receiver_once`) with its share in
  basis points (a percentage with two decimals); insert-only. Two deferred
  constraint triggers hold, when the transaction commits, that a cycle's
  shares add up to 10,000 basis points and that no receiver is the sender
  (`allocation_receiver_shares_whole`, `allocation_receiver_not_sender`).
  `controlling.allocation_run`: a run of a cycle for a fiscal period with
  its posting date, journal entry (once), document number, the amount
  moved and the number of accounts; one per cycle and period
  (`allocation_run_once`); insert-only. Row-level security on all three;
  the centers and the entry are the ledger's, without foreign keys.
  Schema test: `db/allocation_test.go` (`TestAllocationRules`).
- **Shaped for T60g.** An assessment is a further `kind` of the same
  cycle: T60g widens the kind's check and adds the assessment account to
  `allocation_cycle` (required exactly for that kind); receivers, runs,
  the operations and the events stay as they are, `kind` already on the
  cycle, the run and both events. The run's lines are built by
  `distributionLines` alone, so an assessment adds its own beside it.
- **API.** `POST /v1/allocation-cycles` (201; 409
  `controlling.allocation_cycle_exists`; 422
  `controlling.allocation_cycle_invalid`, naming every field that cannot
  stand: `kind` `unsupported`; `sender_cost_center_id` and
  `receivers[i].cost_center_id` `unknown`, `not_in_company`, `not_active`,
  `is_sender`, `duplicate`; `receivers[i].percentage` `invalid`;
  `receivers` `not_100`), `GET /v1/allocation-cycles` (filters
  `legal_entity_id`, `code`, `kind`, `sender_cost_center_id`; by `code`),
  `GET /v1/allocation-cycles/{id}`, `POST /v1/allocation-cycles/{id}:run`
  (body `fiscal_year`, `fiscal_period`; `If-Match` the cycle's version),
  `GET /v1/allocation-runs` (filters `legal_entity_id`,
  `allocation_cycle_id`, `fiscal_year`, `fiscal_period`; latest first by
  `posting_date`), `GET /v1/allocation-runs/{id}`. Cycles and runs show
  their centers' codes and the run its cycle's code, name and kind, its
  amount with its currency and its entry's id and number.
- **A run** holds every cycle of its sender `FOR UPDATE` (the cycle
  itself among them, locked in id order after a first read without a
  lock that names the sender, which a cycle keeps) before it looks for a
  run and reads the costs, so the runs of one sender follow one another:
  two runs of a cycle serialize, and two cycles of one sender never both
  read a cost and both move it (review round 1; proved by
  `TestDistributionRunsOfOneSender`). A period the cycle was already run
  for is refused (409
  `controlling.allocation_run_exists`, naming the run's entry), and the
  database holds it. The sender's costs of the period are the ledger's
  actuals of the center (`ledger.AssignmentBalances.CostCenterActuals`,
  what the cost center report shows, read page by page), less the
  accounts that are no primary cost accounts and those whose cost is zero
  (a new read of the port, `CostAccounts`: number and whether primary
  cost), in account number order. Per account the sender is credited
  with its cost and each receiver, in the cycle's order, debited with its
  share on the same account; the shares are the cost times each
  receiver's basis points over 10,000, divided by `money.Allocate`
  (largest remainder), so they add up to the cost to the minor unit and
  the entry balances per account; a share that rounds to nothing has no
  line. The entry is posted through `ledger.Postings.Post`
  (`source_context` `controlling`, document type `SA`, local currency,
  dated the period's last day, its source document the run). The run is
  kept, audited (`controlling.allocation_cycle.run`) and published
  (`AllocationRunPosted`). An Idempotency-Key replay answers the same.
- **Events** `AllocationCycleChanged` (`created`, with the receivers and
  their percentages) and `AllocationRunPosted`; audit actions
  `controlling.allocation_cycle.create` and `.run`.
- **Permissions** `controlling.allocation.read`, `.write`, `.run` (none
  an approval permission).
- **Not a report read**: the runs are a plain listing of a table, so
  they get no budget test (07 §7.7 item 7).
- **Decided by default:**
  1. A sender without primary costs in the period posts nothing and
     **keeps no run** (`posted: false`): a run is not reversed, so a kept
     empty run would close the period to the cycle for good.
  2. A percentage is above 0 and at most 100 with up to two decimals,
     kept as integer basis points; no float touches an amount.
  3. A cycle has 1 to 100 receivers, none of them the sender, each once.
  4. A cycle, once created, is not changed or deleted in the first
     version (the event is named for the changes to come); its `version`
     is the `If-Match` of `:run`, as every custom method takes one.
  5. Defining and running a cycle are a person's (`x-agent: false`), as
     the order writes are (§1); agents read cycles and runs. Running and
     reading runs also need `ledger.entry.read`, since they show ledger
     amounts.
  6. The centers are checked active when the cycle is defined and valid
     on the period's last day when it is run (409
     `controlling.allocation_center_not_valid`, naming the center); a
     center locked for actual costs, a closed period and every other
     refusal of the entry are the ledger's own problems.
  7. A credit balance on a cost account moves as one (negative shares).
     Revenue posted to the sender stays on it.
  8. A cost posted to the sender in a period after the cycle's run of
     that period stays on the sender (no second run, no reversal in the
     first version); it is not picked up by a later period's run.
  9. Two cycles may name the same sender; their runs follow one another
     (the lock above), and the second run of a period finds what the
     first left (nothing, unless costs were posted since). A receiver of
     one cycle that is the sender of another running at the same moment
     keeps the share it receives after its own run, as any cost posted
     after a run does (8).
  10. Each receiver's line takes the receiver's profit center and
      segment, the sender's line the sender's, as a settlement's do.
- **Known limits, for a specialist** (question 2): as for a settlement,
  the entry's group amounts are converted at the run date's rate, and the
  leading ledger's costs are moved in every ledger the company keeps.
- **Walkthrough**: `step_distribution` (Step 52): Sam sets up CC-1200
  (shared services) and CC-1300, Alex posts CAD 999.99 to CC-1200, Carla
  defines the cycle `SHARED` (CC-1100 60 %, CC-1300 40 %; percentages
  short of 100 are refused) and runs it for the current period: CAD
  599.99 and 400.00 on account 510000, the sender's cost gone, the
  centers' reports showing the shares; a second run is refused.

### T60g: an assessment cycle run (migration 000277)
- **The configuration** (`internal/config/controlling.go`) accepts
  `cost_element: secondary`, on an expense account alone
  (`config.CostElementSecondary`; `Account.CostElementProblem`, which
  country packs check their accounts with too). A secondary cost element
  is refused wherever the company names an account something other than
  an allocation posts to: the accounts of automatic postings
  (`Company.AutomaticAccounts`: account determination, tax codes and
  their components, withholding taxes, the revaluation's accounts, house
  banks, statement posting rules), an asset class's depreciation and loss
  accounts, a default assignment (which was already for primary cost
  accounts alone), and with `open_items`. Reconciliation accounts and
  revalued accounts are balance sheet accounts by their own rules, so
  none of them is one.
- **The ledger's account** (migration 000277; `internal/ledger`:
  `secondaryaccounts.go`, new; `setup.go`). The check
  `gl_account_cost_element` allows `secondary` on an expense account. An
  account is a secondary cost element **from its creation and stays
  one**: a trigger refuses an update that turns an account's cost element
  to or from `secondary` (`gl_account_secondary_kept`), as only a new
  account becomes an asset reconciliation account (ADR-0057 §3). An
  existing account may hold lines other contexts posted, or take one in
  flight while it is turned; a secondary one turned into a primary cost
  account would pass its assessments off as primary costs to the next
  run. `ledger.Setup.Check` reports both on submission
  (`checkSecondaryNew`); the activation is refused by the trigger (409
  `ledger.secondary_account_not_new`). A secondary cost element the chart
  no longer has is deactivated, as any account.
- **The ledger's posting rule** (`internal/ledger`: `secondaryaccounts.go`,
  `journal.go`, `assignment.go`). A line on a secondary cost element is
  posted only in an entry whose `source_context` is `controlling`:
  `ledger.check_line`, the trigger every journal line passes, refuses
  every other (`journal_line_secondary_account`), and the posting paths
  refuse it first, naming the line (422 `ledger.secondary_account`,
  `lines[i].account_id`, code `secondary_cost_element`; `refuseSecondary`
  in `prepare`, which manual entries, parked entries, entry templates and
  every caller of `ledger.Postings` pass). Controlling's own line on it
  names a cost center (422 `ledger.cost_center_required`: the account has
  no default one), which is validated as a primary cost line's is: valid
  on the posting date and **not locked for actual costs** (422
  `ledger.cost_center_invalid`, `locked_for_costs`). Its profit center
  and segment derive from that cost center. A line on it naming an
  internal order is refused as before (`not_a_cost_account`, §2). Schema
  test: `db/secondary_account_test.go` (`TestSecondaryCostElements`).
- **The ledger's port** (`assignmentbalances.go`). `CostAccounts` takes
  the legal entity and reads the accounts set up in it, each with whether
  it is a primary cost account, a secondary cost element and active.
- **The cycle** (migration 000277). `controlling.allocation_cycle.kind`
  may be `assessment`, and the cycle then names its
  `assessment_account_id` (the ledger's, no foreign key), a distribution
  none (`allocation_cycle_assessment_account`); the cycle keeps it
  (`allocation_cycle_identity`). Schema test: `db/allocation_test.go`.
  `POST /v1/allocation-cycles` takes `assessment_account_id`: required
  with `assessment` (`required`), an active secondary cost element set up
  in the cycle's company (`unknown`, `not_secondary`, `not_active`), and
  refused with `distribution` (`not_allowed`), each a field error of 422
  `controlling.allocation_cycle_invalid` beside the others. The cycle,
  its audit record and `AllocationCycleChanged` show the account's id and
  number. The account is checked when the cycle is defined; at a run, an
  account deactivated or blocked since is the ledger's refusal of the
  entry.
- **A run of an assessment** (`allocationrun.go`). The sender's costs are
  read as a distribution reads them: its primary costs of the period per
  account, from the leading ledger. Their total is what the run charges.
  One entry, all on the assessment account, credits the sender with the
  total and debits each receiver, in the cycle's order, with its share:
  the total times the receiver's basis points over 10,000, divided by
  `money.Allocate` (largest remainder), so the shares add up to the total
  to the minor unit and the entry balances on the account; a share that
  rounds to nothing has no line. The entry is posted as a distribution's
  (`source_context` `controlling`, document type `SA`, local currency,
  dated the period's last day, header text `Assessment <code>`). The
  sender's primary costs stay on their accounts. The run's `amount` is
  the total and its `account_count` the number of the sender's accounts
  whose costs it charged. Everything T60f decided for a run holds: the
  locks, one run per cycle and period, `If-Match`, the permissions,
  people only, the centers' validity on the posting date, the events.
- **A sender assessed for a period is not run for it again** (409
  `controlling.allocation_sender_assessed`, naming the assessment's cycle
  and entry), by the same cycle (409 `controlling.allocation_run_exists`
  first), another assessment or a distribution of that sender: the
  primary costs are still on the sender, and the next run would read and
  charge them again. T60f's "the second run finds what the first left"
  holds for distributions, which leave nothing; the check is made under
  the lock on the sender's cycles, so no such run is under way (proved by
  `TestAssessmentRunsOfOneSender`). A distribution first and an
  assessment after it is allowed: the assessment charges what was posted
  to the sender since, or nothing.
- **What a secondary cost element is refused for, and how it reads.**
  1. *Opening balances and migration loads*: refused; a migration run's
     entries (`integration`) are not controlling's, and the account has
     no balance to load. The SAP configuration draft does not take SAP's
     secondary cost elements over (unchanged: a note for the reviewer,
     `internal/delivery/sapconfig`).
  2. *Tax*: no tax code's account, and no line on it carries a tax code,
     since controlling's entries name none.
  3. *Foreign currency*: not revalued, and not an account a revaluation
     posts to. The entry is in local currency; its group amounts are
     converted at the run date's rate (the known limit of T60e and T60f).
  4. *Year-end carry-forward*: a profit and loss account, so its balance
     would go to the year's result; the balance is zero in the company,
     so it adds nothing and nothing is carried.
  5. *Financial statements*: in a company's trial balance it shows equal
     debits and credits and a balance of zero, and adds nothing to the
     result. In group reporting it is mapped to a group account like
     every account of a company in a group (ADR-0059 §3) and adds zero.
     By profit center or segment it need not net to zero: an assessment
     from a center of one profit center to a center of another moves the
     cost between them, as a settlement does (T60e, default 7).
  6. *Cost center reports* (`GET /v1/cost-center-balances`, plan against
     actual): the sender shows its primary costs on their accounts and
     the credit on the assessment account, the receivers their debits on
     it. The reports' queries are unchanged.
  7. *Internal orders and settlement*: no line on a secondary cost
     element names an order, and only cost centers receive in T60f and
     T60g, so an order never carries an assessment's line and a
     settlement, which moves an order's balance per account, never moves
     one.
  8. *Other contexts*: the ledger's reads of an account another context
     may be given to post to answer no for it, as they do for a
     reconciliation account and the GR/IR account
     (`ledger.Accounts.IsPostable`, `CloseAccounts.Accounts`,
     `AssetReads.OffsetAccount`; files `ports.go`, `closeparked.go`,
     `assetreads.go`; review round 1), so a requisition, order, supplier
     or customer invoice item, an accrual proposal and an asset's offset
     on it are refused when they are entered, by each context's own
     problem. Whatever still reaches a posting is refused by the ledger
     (422 `ledger.secondary_account`).
- **Decided by default:**
  1. A secondary cost element is an expense account (the chart's types
     have no "secondary costs"), new when it becomes one, and stays one.
  2. Only controlling posts to it; a migration run does not.
  3. A cost center's lock for actual costs refuses an assessment's line,
     on the sender and on a receiver.
  4. An assessment charges the sender's **primary** costs only: what
     other cycles charged to the sender on a secondary cost element is
     not passed on (iterative cycles are in the plan's "Later"), so a
     receiver's own distribution or assessment does not move what it
     received by assessment.
  5. Costs that add up to nothing (a debit on one account, a credit of
     the same amount on another) post nothing and keep no run, as a
     sender without costs (T60f, default 1). A credit total is charged
     as one (negative shares), as for a distribution (T60f, default 7).
  6. A sender assessed for a period is not run for it again (above). A
     cost posted to it in the period afterwards stays on it (T60f,
     default 8).
  7. One assessment account per cycle, entered when the cycle is
     defined; several cycles may name the same account.
- **Gaps for the product owner** (not in the plan's lists, not built):
  the accounts API does not show an account's cost element, so a client
  finds the assessment account in the configuration document; a run and
  its event name the cycle's kind but not the assessment account, which
  the cycle and the run's entry show (review round 1, a nit); the SAP
  configuration draft does not take over SAP's secondary cost elements
  or cycles.
- **Walkthrough**: `step_assessment` (Step 53): Ada adds account 6990, a
  secondary cost element, through a governed change; Sam sets up CC-1400
  (facilities), Alex posts CAD 1,000.01 of rent to it, and a manual entry
  on 6990 is refused; Carla defines the cycle `FACILITIES` (CC-1100 70 %,
  CC-1300 30 %; without its account it is refused) and runs it for the
  current period: CAD 700.01 and 300.00 on account 6990, the sender
  credited with 1,000.01 there and its rent still on account 510000, the
  centers' reports showing the assessment account's lines; a second run
  and a distribution of the same sender are refused.
