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
  it copies centers (ADR-0050 §5).
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
the rule. Primary costs stay on their accounts.

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

## As built

### T60a: an internal order created and released (migration 000265)
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

### T60b: costs posted to an internal order (migrations 000266-000268)
- **Column** `ledger.journal_line.internal_order_id`, nullable, no foreign
  key; the check `journal_line_one_cost_object` (a cost center or an
  order, never both), added `NOT VALID` (000266) and validated partition
  by partition (000267); the partial index
  `journal_line_by_internal_order` per partition (000268,
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
