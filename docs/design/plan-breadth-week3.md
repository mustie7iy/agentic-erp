# Breadth-first plan, week 3 (25 November – 1 December 2026)

**Status:** plan (product owner, 2026-11-10: breadth first; 2026-11-12:
returns added, seven capabilities deferred). It details the tracker's
remaining placeholders of the required scope into slices (ADR-0053) and is
frozen once merged: each placeholder is split once, here, and this is the
last split of the breadth phase. `docs/tracker.json` stays the only plan
(07 §7.5); each leaf's `r` names its section below. It follows
`plan-breadth-week1.md` and `plan-breadth-week2.md`, whose rules it does
not repeat where they are unchanged.

The decision: every capability in the required scope gets a first usable
version end to end by 2026-12-01: the main path only, one or two slices
each, each sized for one agent in about half a day and fully gated (07
§7.7). Variants and depth come after 1 December; each placeholder's
"later: …" leaf names what was left out on purpose and is not picked up
before then.

Week 3 is the reverse sales chain (returns and credit memo requests),
dispute cases, expense reports, lease accounting, analytics (sales and
spend metrics, profitability and margin, report delivery, watchers,
forecasts), planning versions, governance, risk and compliance (controls,
risks, retention, archiving, personal data), process mining, an MCP server
and the thin agents. Fifty-three placeholders are split into 58 slices and
52 "later" leaves (the fifty-third, the bid comparison agent, was already
a leaf of its own since week 2 and keeps week 2's "later" leaf). The
target dates are the roadmap's; the lanes start as soon as this plan is
merged.

## The placeholders of the required scope, counted

The required scope is the tracker's areas Finance, Controlling, Sourcing
and procurement, Sales and service, Master data, GRC and security,
Analytics and planning, Platform, Non-functional and infrastructure,
Agentic delivery, Migration from SAP and Quality engineering (product
owner, 2026-11-10). On the base of this plan (`main` at 75197746) those
areas hold **303 open leaves**:

| Kind of open leaf | Count | In this plan |
|---|---|---|
| Unsplit placeholders: open leaves that sit directly under a module, carry the source `baseline` and have no step's detail | 139 | classified below |
| The bid comparison agent, a leaf week 2 set apart for week 3 | 1 | split (T105c) |
| "later: …" leaves of weeks 1 and 2 | 30 | not touched (after 1 December) |
| Leaves of steps already detailed: parts of split steps, review findings, sign-offs, operator tasks, week 2's slices not yet marked done on this base | 133 | not touched |

The 140 placeholders, by what this plan does with them:

| Area | Split | Left | Left, by reason (codes below) |
|---|---|---|---|
| Finance | 6 | 14 | PP 3 · DET 5 · POD 4 · OUT 1 · OTH 1 |
| Controlling | 4 | 3 | DET 1 · OUT 2 |
| Sourcing and procurement | 3 | 10 | PP 6 · W 1 · POD 1 · PO12 2 |
| Sales and service | 4 | 11 | PP 2 · DET 1 · PO12 8 |
| Master data | 2 | 2 | PP 2 |
| GRC and security | 6 | 1 | PP 1 |
| Analytics and planning | 12 | 5 | W 1 · WEB 1 · OUT 2 · POD 1 |
| Platform | 0 | 6 | PP 3 · DET 1 · WEB 1 · OTH 1 |
| Non-functional and infrastructure | 4 | 7 | PP 3 · W 2 · DET 1 · POD 1 |
| Agentic delivery | 12 | 19 | PP 6 · W 4 · SAP 6 · OTH 3 |
| Migration from SAP | 0 | 6 | PP 1 · W 3 · WEB 1 · POD 1 |
| Quality engineering | 0 | 3 | PP 3 |
| **Total** | **53** | **87** | PP 30 · W 11 · PO12 10 · DET 9 · POD 8 · SAP 6 · OUT 5 · OTH 5 · WEB 3 |

The reasons are spelled out, leaf by leaf, under "Open leaves left for
after 1 December". After the split the same areas hold **360 open
leaves**: 303 less the 52 placeholders that became summaries, plus 57 new
slice leaves and 52 "later" leaves. Of the 360, 82 are "later" leaves and
87 are the placeholders left, none of them to be picked up before
1 December.

## What every slice reuses

What `plan-breadth-week1.md` and `plan-breadth-week2.md` list under the
same heading applies unchanged: the unit of work and `reqctx.Request`,
RLS, `money.Minor`, `money.Allocate`, `money.Scale` and `money.Sum`, row
locks and state guards, `Idempotency-Key` and `If-Match`, financial
effects only through `ledger.Postings`, the context's outbox and inbox, an
audit record per state change, `problem.*` errors, contract first, real
PostgreSQL tests with `pgtest` and `pgtest.FailingTx`, a schema test per
database rule, the company's date in its time zone
(`config.Config.CompanyDay`), events naming a customer or supplier by id
and number only, and one new or extended step of `scripts/walkthrough.sh`.
Headless first: no web workspace work (ADR-0053 g).

Week 3 adds five things every slice here holds to (product owner,
2026-11-12, unless another source is named):

- **A report read gets a budget test of 3 s at p95** (`nfr.Budget`,
  measured as `scripts/budgets.sh` does), seeded at the volume 01 §1.3
  gives its data. A slice that adds a list or a report says so under
  "Acceptance".
- **No segregation-of-duties rule** is added: a release, an approval or a
  posting is by a permission of its own, and nothing sets the person apart
  from whoever created the document. The tenant's own SoD rules
  (`internal/identity`) apply as on every operation.
- **Events go to the context's outbox and `api/asyncapi.yaml`; none is
  offered as a webhook event** until a consumer asks.
- **No dunning charges or interest**, and no charge on a return or a
  dispute; the only interest computed is a lease liability's (T88b).
- **A run started by a person refuses a requester deactivated since**
  (product owner, 2026-11-03), where a slice adds a scheduled or queued
  run (T95b, T99b, T100c, T101a).

## Boundaries with the areas outside the required scope

Supply chain, Manufacturing, Assets/service/EHS, Projects, Human capital
and the industry packs stay outside. Week 2's boundaries hold (no material
master, no stock, no cost of goods sold), and week 3 adds:

- **A return moves no stock.** T85b is a confirmation in the sales
  context that returned quantities arrived, as T78c is for a delivery. The
  return delivery, inspection and stock posting are Supply chain's.
- **An employee is a business partner with the role supplier** (T87a).
  Human capital's employee master is outside the scope; question 6.
- **A right-of-use asset is not an asset of asset accounting** (T88b). The
  leases context posts through the ledger with accounts of its own.
  Whether it belongs in the asset history sheet is the accounting
  specialist's question 3.
- **Profitability has no cost side** (T92a): no cost of goods sold is
  posted, so a segment shows net revenue only. The margin report (T92b)
  takes cost of sales from the general ledger, where a person posts it at
  period end (planning default of 2026-11-12).
- **Inventory and HR metrics, product costing and budget control** depend
  on those areas and are left.

## Build order and lanes

Four lanes; a slice starts when the slices it depends on are in `main`, or
from their lane's tip as a stacked base. Week 2's T81c and T81d
(recognition schedule and run) are still to build and stay with week 2's
lane; nothing here depends on them.

- **Lane A, documents** (`internal/sales`: new returns and credit memo
  request files, a capture file; `internal/collections`: a new dispute
  file and the dunning proposal's and worklist's queries;
  `internal/payables`: a new expense report file; new `internal/leases`;
  `internal/partner`: the partner change's creation; `internal/config`:
  the expense types of T87a and the account determination keys of T88b
  and T88c): T85a → T85b → T85c →
  T85d → T86a → T86b → T87a → T89a → T88a → T88b → T88c → T90a, then three
  agents that need nothing of lanes B and C: T109a → T109b → T110a.
- **Lane B, analytics and planning** (`internal/analytics` with its
  `endpoint`, `extract` and `store`; `internal/controlling`: new
  profitability, margin and plan copy files; `internal/config` for the
  margin report's accounts): T91a → T91b → T92b → T93a → T92a → T94b →
  T94a → T95a → T95b → T95c → T96a → T96b → T97a → T97b → T95d. T92a needs
  T85d; T95d is last because it waits for a file (question 8). T91a reads
  orders and billing documents through the API, and the store's extract
  reads the sales tables as it reads the others, so it adds no file to
  `internal/sales`.
- **Lane C, governance, records and delivery tools** (new `internal/grc`,
  `internal/records`, `internal/processmining`; `internal/partner`: the
  partner's reads and block; `internal/audit` for the evidence export; `internal/config` for retention; `cmd/erp` and
  `internal/apitools` for the MCP server; `internal/delivery/sapconfig`):
  T98a → T98b → T98c → T99a → T99b → T100a → T100b → T100c → T101a → T103a
  → T103b → T104a → T102a.
- **Lane D, agents** (one new package per agent under `internal/`, its
  suite under `evals/`, its lines in `internal/platform/autonomy`,
  `cmd/erp`'s agent and eval commands): T105a → T105c → T106a → T106b →
  T106c → T107a → T107b → T105b → T108a → T108b → T108c → T105d → T105e →
  T105f → T105g. T105b needs T86a, T105d needs T91b, T105e needs T93a,
  T105f needs T98b, T105g needs T98c.

By build day, two slices a lane a day:

| Day | Lane A | Lane B | Lane C | Lane D |
|---|---|---|---|---|
| 1 | T85a, T85b | T91a, T91b | T98a, T98b | T105a, T105c |
| 2 | T85c, T85d | T92b, T93a | T98c, T99a | T106a, T106b |
| 3 | T86a, T86b | T92a, T94b | T99b, T100a | T106c, T107a |
| 4 | T87a, T89a | T94a, T95a | T100b, T100c | T107b, T105b |
| 5 | T88a, T88b | T95b, T95c | T101a, T103a | T108a, T108b |
| 6 | T88c, T90a | T96a, T96b | T103b, T104a | T108c, T105d |
| 7 | T109a, T109b | T97a, T97b | T102a | T105e, T105f |
| 8 | T110a | T95d | — | T105g |

Fifty-eight slices in eight build days of four lanes, one batch a day.
Day 1 is the first day after this plan is merged. With a start on
16 November, day 8 is 23 November, and the roadmap's week (25 November –
1 December) holds what the batches' gates and reviews send back, the
whole-codebase review (07 §7.7 step 9; the tracker's "Pre-pilot
whole-codebase review" leaf) and the release candidate. A start later
than 19 November leaves no such margin; the coordinator then drops the
last day's slices of each lane first (T110a, T95d, T105g), which nothing
depends on, and reports them as not started.

What merges first, where two lanes touch one package:

- `internal/partner`: lane A's T89a (the change's creation, day 4) before
  lane C's T100b (reads and block, day 4); different files.
- `internal/config` (`config.go`, `validate.go`): T92b (lane B, day 2)
  and T99a (lane C, day 2) each add a section of their own, lane B
  first; lane A adds T87a's expense types on day 4 and the account
  determination keys of T88b and T88c on days 5 and 6, after both.
- `internal/collections`: lane D's T105b only reads it through the API.
- `internal/platform/autonomy` (`Types`), `cmd/erp`'s agent and eval
  commands and `internal/layout`'s import table: every agent slice adds
  its lines; lane D before lane A's three agents on days 7 and 8.
- `api/openapi.yaml`, `api/gen`, migration numbers, `internal/app` wiring,
  `scripts/walkthrough.sh` and the tracker are shared by every slice and
  resolved at integration as always (`make generate`, renumbering).

Step numbers: T85–T110 are new; nothing on the base uses them (the highest
in use is T84b). Integration renames them if another plan took the same
numbers.

## New contexts

- **`leases`** (T88a): lease contracts, their schedules and runs. A leaf
  (ADR-0051 §2 is the pattern): it reads the ledger, tenancy, the partner
  and the configuration through ports the composition root wires, posts
  through `ledger.Postings`, and nothing imports it.
- **`grc`** (T98a): controls, their tests and links, risks. A leaf: it
  reads identity (permissions, SoD rules) and the configuration through
  ports; it posts nothing.
- **`records`** (T99a): retention status, archiving runs, the personal
  data inventory and deletion runs. A leaf: it reads the ledger and blanks a
  partner's data through a port the partner context publishes; the
  retention rules themselves are a section of the configuration document.
- **`processmining`** (T103a): event logs and process maps, computed on
  read. A leaf that reads procurement, payables, payments and the audit
  trail through ports and stores nothing.

Returns orders and credit memo requests are new tables of `sales`; dispute
cases of `collections`; expense reports of `payables`; saved reports,
schedules, shares, watchers and alerts of `analytics`. Each agent is a
package of its own, as `internal/cashagent` is.

## ADRs

New, each written by the first slice named and accepted once
`go-reviewer` has no blocking finding (0062 is held by a parked branch;
0068 and 0070 are week 2's; integration renumbers on a clash):

- **ADR-0072, recommending agents of the breadth phase**, written by
  T105a: the pattern of "The recommending agents" below, the agent types,
  what each may read and the kinds it recommends. Every other agent slice
  records its agent in the ADR's "As built".
- **ADR-0073, lease accounting**, written by T88a, with the IFRS 16
  statements of "Lease accounting: what the standard is taken to say",
  each marked as confirmed by the accounting specialist or not.
- **ADR-0074, expense reports**, written by T87a.
- **ADR-0075, margin and profitability reports**, written by T92b (the
  first of the two built).
- **ADR-0076, report delivery** (saved reports through the API,
  schedules, sharing, the Excel workbook), written by T95a.
- **ADR-0077, metric watchers, alerts, forecasts and scenarios**, written
  by T96a.
- **ADR-0078, controls and risks** (the `grc` context), written by T98a.
- **ADR-0079, retention, archiving and personal data**, written by T99a;
  T99b and T100a–T100c record what they build in it. It states the
  security posture of question 2 as the product owner answers it.
- **ADR-0080, process mining**, written by T103a.
- **ADR-0081, MCP server**, written by T102a.

No new ADR, recorded in an existing one's "As built":

- Returns orders and credit memo requests (T85a–T85d), order capture
  (T90a): ADR-0067 (sales); T85d also ADR-0063 §7 (the credit memo).
- Dispute cases (T86a, T86b): ADR-0065 (dunning and collections).
- Duplicate check (T89a): 03 §3.6, where T74a recorded the customer.
- Sales metrics, spend cube, drill-through, follow-up questions (T91a,
  T91b, T94a, T94b): ADR-0044 (analytical store) and the configuration's
  analytics section.
- Planning versions (T93a): ADR-0050 (plan data, T56g).
- Audit evidence export (T101a): ADR-0011 (audit trail) and ADR-0016
  (object storage).
- Fit-gap report (T104a): `docs/migration.md`.

---

## Lane A, documents

### T85a Returns order created and confirmed

**Capability.** A sales administrator creates a returns order for
quantities of a posted billing document's items, with a reason, and
confirms it; its items show what was billed and what is being returned.

- **Reuses:** the billing document and its items (`internal/sales`,
  T79a), the sales order's row lock under which billing counts quantities,
  the number ranges, `money.PositiveQuantity`, audit, outbox.
- **Adds:** tables `sales.returns_order`, `sales.returns_order_item`;
  `POST /v1/returns-orders` (billing document, reason, items with a
  quantity), `GET /v1/returns-orders`, `GET /v1/returns-orders/{id}`,
  `POST /v1/returns-orders/{id}:confirm`; event `ReturnsOrderChanged`;
  permissions `sales.return.read`, `.write`, `.confirm`; number range
  `returns_order`; `returned_quantity` per item in the sales order's
  representation.
- **Walkthrough:** `step_returns`: a returns order for part of the billed
  quantity of `step_billing`'s document is created and confirmed.
- **Acceptance:** only a posted billing document takes a returns order; an
  item's quantity is positive and at most its billed quantity less what
  the document's other returns orders took, under the sales order's lock,
  so two requests never return a quantity twice; the reason is one of an
  open enumeration (defective, wrong item, not wanted, other) with a free
  text; confirming moves `draft` to `confirmed` once; a replay answers the
  same order; nothing is posted and no stock moves.
- **Depends on:** T79a, T79b (week 2).

### T85b Returned quantities confirmed as received

**Capability.** The sales administrator confirms that returned quantities
arrived on a date; the returns order shows what was received and what is
still expected.

- **Reuses:** T85a's returns order under its row lock; T78c's delivery
  confirmation is the pattern (quantities against the open quantity,
  insert-only rows, the date not after the company's date today).
- **Adds:** tables `sales.return_receipt`, `sales.return_receipt_item`;
  `POST /v1/returns-orders/{id}/receipts`,
  `GET /v1/returns-orders/{id}/receipts`; event `ReturnReceived`;
  permission `sales.return.receive`; `received_quantity` per returns
  order item.
- **Walkthrough:** `step_returns` extended: the returned quantity is
  confirmed as received; the returns order reads as fully received.
- **Acceptance:** only a confirmed returns order takes a receipt; a
  quantity above the item's open quantity is refused naming the item
  (422); two receipts racing for the last quantity receive it once; a
  replay answers the same receipt; the order's delivered and billed
  quantities do not change; nothing is posted.
- **Depends on:** T85a.

**Later for Returns orders (after 1 December):** a return before billing
or without a billing document; the return delivery, goods receipt,
inspection and stock posting (Supply chain, outside the required scope);
replacement deliveries; changing or cancelling a returns order; approval
of a return; return reasons from configuration; the return in the revenue
contract (accounting specialist, question 4).

### T85c Credit memo request created and decided

**Capability.** A sales administrator creates a credit memo request
against a posted billing document, for a returns order's received
quantities or for an amount per item with a reason, and a sales manager
approves or rejects it.

- **Reuses:** T85b's received quantities, the billing document's items
  and amounts, T79a's sharing of an item's amount by quantity (the amount
  for a quantity is the item's amount × the quantity ÷ the billed
  quantity, the last request taking the remainder), the number ranges.
- **Adds:** tables `sales.credit_memo_request`,
  `sales.credit_memo_request_item`; `POST /v1/credit-memo-requests`
  (billing document, and either a returns order or items with an amount),
  `GET /v1/credit-memo-requests`, `GET /v1/credit-memo-requests/{id}`,
  `POST /v1/credit-memo-requests/{id}:approve`, `:reject` (with a reason);
  event `CreditMemoRequestChanged`; permissions
  `sales.credit_memo_request.read`, `.write`, `.approve`; number range
  `credit_memo_request`.
- **Walkthrough:** `step_credit_memo_request`: a request is created from
  the returns order of `step_returns`, read back with the amount its
  quantity gives, and approved.
- **Acceptance:** a request from a returns order takes its received
  quantities not yet requested and computes the amounts; a request by
  amount names billing items and positive net amounts; per billing item,
  the requests not rejected never exceed the item's billed amount (under
  the sales order's lock; 422 naming the item); approve and reject move
  `open` once (row lock, version; a replay answers the same) and are
  audited; no rule sets the approver apart from the creator (question 5);
  nothing is posted.
- **Depends on:** T85b.

### T85d Credit memo posted through receivables

**Capability.** The billing clerk posts an approved credit memo request: a
customer credit memo naming the billing document's invoice is entered and
posted in receivables in the same transaction and clears against it.

- **Reuses:** `receivables.Endpoints.CreateInvoice` and `PostInvoice` with
  the kind `credit_memo` and `reference_invoice_id` (T75d, ADR-0063 §7:
  its tax, its limit against the invoice, 422
  `receivables.credit_exceeds_invoice`, and its clearing against the
  invoice), called in process through the port T79b gave the sales
  context; the source document fields T79b added to the customer invoice.
- **Adds:** `POST /v1/credit-memo-requests/{id}:post`; event
  `CreditMemoRequestPosted`; `customer_invoice_id` and the credit memo's
  number on the request; `credited_amount` per billing document item; in
  `sales.Orders` each item's posted credit (net amount and posting date).
  No new table.
- **Walkthrough:** `step_credit_memo_request` extended: the request is
  posted; the credit memo names the request and the invoice; receivables,
  revenue and output tax moved back by its amounts; the customer's open
  amount fell by its gross.
- **Acceptance:** posting moves `approved` to `posted` once (row lock and
  state guard); the credit memo's items are the request's, one for one,
  on the billing items' revenue accounts and output tax codes; its date is
  the company's date today unless given, and its tax follows T75d as
  built; a refusal by receivables (a closed period, a blocked customer,
  the credit above the invoice's gross) leaves the request unposted and is
  returned naming the request's own fields.
- **Depends on:** T85c, T75d (in `main`), T79b (week 2).

**Later for Credit memo requests (after 1 December):** debit memo
requests; a request without a billing document; approval tiers by amount
and a rule that the approver is another person; restocking and handling
charges; a credit paid out to the customer in place of clearing;
cancelling a posted request; the credit memo as a document and as an
e-invoice; the credited amounts in the revenue contract and in rebates.

### T86a Dispute case opened and closed

**Capability.** A collections specialist opens a dispute case on a
customer's open invoice with a reason and the disputed amount, assigns it
to a person, and closes it as accepted or rejected.

- **Reuses:** the collections context and its reader of customer open
  items (T76a, ADR-0065), `partner.Payees.Names`, the principals' names
  for the assignee, `etag`, audit, outbox.
- **Adds:** table `collections.dispute_case`; `POST /v1/dispute-cases`,
  `GET /v1/dispute-cases` (customer, status, assignee),
  `GET /v1/dispute-cases/{id}`, `PATCH /v1/dispute-cases/{id}` (assignee,
  note), `POST /v1/dispute-cases/{id}:close` (outcome, reason); event
  `DisputeCaseChanged` (customer id and number); permissions
  `collections.dispute.read`, `.write`.
- **Walkthrough:** `step_dispute`: a case is opened on the overdue invoice
  of `step_dunning`'s customer for part of its amount, assigned, and read
  back.
- **Acceptance:** the invoice must be an open customer item of a company
  the caller may access; the disputed amount is positive and at most the
  item's open amount; an item has one open case (a second is refused,
  409); closing moves `open` to `accepted` or `rejected` once; an item
  cleared meanwhile leaves the case open for a person to close; closing a
  case posts nothing (an accepted case is credited by a person, T85c or
  T75d).
- **Depends on:** T76a, T75b (in `main`).

### T86b Disputed invoices in dunning and the worklist

**Capability.** A dunning run leaves an invoice with an open dispute case
out of its proposal, and the collection worklist shows each customer's
disputed amount beside its overdue amount.

- **Reuses:** the dunning proposal (`internal/collections/propose.go`) and
  the worklist (`worklist.go`, `GET /v1/collection-worklist`), with their
  tests and the worklist's budget test.
- **Adds:** the exclusion in the proposal's query, with the excluded
  items listed on the run as `disputed`; `disputed_amount` and
  `open_dispute_cases` per customer on the worklist. No new endpoint or
  table.
- **Walkthrough:** `step_dispute` extended: a dunning run at the same key
  date leaves the disputed invoice out and names it as disputed; the
  worklist shows the disputed amount; after the case is closed as
  rejected a new run proposes the invoice.
- **Acceptance:** an invoice with an open case is in no notice whatever
  the disputed part (default 4); a run already proposed before the case
  was opened is confirmed as proposed; the worklist's order and totals
  stay as T76c built them, the disputed amount beside them; the
  worklist's budget (3 s at p95) holds with cases seeded.
- **Depends on:** T86a.

**Later for Dispute cases (after 1 December):** a case over several
invoices or on a short payment at the incoming payment; the credit memo
or write-off created from an accepted case; correspondence and
attachments; escalation and deadlines; dunning the undisputed part of an
invoice; disputed amounts in the credit exposure and the cash forecast;
cases opened by the customer.

### T87a Expense report entered and posted

**Capability.** An accountant enters an employee's expense report (the
employee as a business partner with the role supplier; lines with a date,
an expense type, an amount, an input tax code and a cost center) and
posts it: the employee's open item, which the payment run pays.

- **Reuses:** the posting of a supplier invoice without an order
  (`internal/payables/post.go`, `tax.go`: gross lines, input tax at the
  document date, the supplier's reconciliation account and payment term,
  `ledger.Postings.Post` with `source_context` `payables`), the supplier
  check payables makes at entry, the ledger's cost center assignment, the
  payment run as it is.
- **Adds:** the configuration section `companies[].expense_types[]` (code,
  name, expense account; none shipped); tables `payables.expense_report`,
  `payables.expense_report_line`; `POST /v1/expense-reports`,
  `GET /v1/expense-reports`, `GET /v1/expense-reports/{id}`,
  `POST /v1/expense-reports/{id}:post`; event `ExpenseReportChanged`;
  permissions `payables.expense.read`, `.write`, `.post`; number range
  `expense_report`; ADR-0074.
- **Walkthrough:** `step_expense_report`: an employee is created as a
  supplier, a report with two lines of different expense types is entered
  and posted, and the open item appears in a payment run's proposal.
- **Acceptance:** each line's account is its expense type's; amounts are
  gross in the company's currency and the tax is taken out of them by the
  line's code as payables does for a gross invoice; an unknown expense
  type, tax code or cost center is refused naming the line's field; a
  line's date is not after the company's date today; posting moves
  `entered` to `posted` once and needs an open period; the open item is
  due by the partner's payment term; a list read by employee and status.
- **Depends on:** nothing (payables and the payment run are in `main`).

**Later for Expense postings (after 1 December):** submission by the
employee and approval by a manager; receipts attached and read by
capture; per diems and mileage rates; corporate cards and their
statements; expenses in another currency; advances and their clearing;
policy checks; employees from human capital (outside the required scope);
expense reports loaded from an expense system.

### T89a Duplicate check when a business partner is created

**Capability.** When a business partner is created, the partner change
lists the existing partners with the same tax registration number or the
same normalized name and country as possible duplicates, and one whose
tax registration number is already in use is refused unless the creator
confirms it.

- **Reuses:** the partner change and its approval (`internal/partner`
  `changes.go`, through which T74a creates a customer), the duplicate
  rules of the migration's quality gate (`internal/integration`
  `quality.go`: duplicate vendor and tax-number warnings); the
  normalization they use moves to `internal/partner` and is used by both,
  not copied.
- **Adds:** `possible_duplicates[]` (partner id, number, what matched) on
  the partner change's representation; `confirm_duplicate` on its create
  input; 409 `partner.duplicate_tax_number`. No new endpoint; one index on
  the normalized name.
- **Walkthrough:** `step_customer` extended: a second customer with the
  first one's tax registration number is refused, then created with the
  confirmation, its change listing the first as a possible duplicate for
  the approver.
- **Acceptance:** the check runs for every role, since the partner is one
  model; a match on name and country is listed and never refused; a match
  on the tax registration number is refused without the confirmation and
  listed with it; a blocked partner still counts; the check sees only the
  caller's tenant (RLS); the migration's gate gives the same results as
  before (its tests unchanged).
- **Depends on:** T74a (in `main`).

**Later for Duplicate checks (after 1 December):** fuzzy matching of names
and addresses; matching on bank details; a duplicate check over the
existing partners as a run with its worklist; merging two partners;
duplicate rules in configuration; the check on a change of an existing
partner.

### T88a Lease contract with its payment schedule

**Capability.** A lease accountant creates a lease contract as lessee
(company, lessor, commencement date, term in months, payment amount and
frequency, in advance or in arrears, annual discount rate, cost center)
and reads it back with its payment schedule.

- **Reuses:** tenancy's legal entities, the supplier check procurement
  and payables make for the lessor, the ledger's cost centers,
  `currency.ParseAmount`, the number ranges, `etag`, audit, outbox.
- **Adds:** the leaf context `leases` (schema, RLS, outbox, its row in
  `internal/layout`); tables `leases.contract`, `leases.payment` (the
  schedule's dates and amounts, written at creation);
  `POST /v1/lease-contracts`, `GET /v1/lease-contracts`,
  `GET /v1/lease-contracts/{id}`; event `LeaseContractChanged`;
  permissions `leases.contract.read`, `.write`; number range
  `lease_contract`; ADR-0073.
- **Walkthrough:** `step_lease`: a three-year lease of office space with
  monthly payments in advance is created and read back with its 36
  payments.
- **Acceptance:** the term is at least one month and the payment
  frequency monthly, quarterly or yearly, dividing the term; the discount
  rate is a decimal percentage above zero, kept as a decimal string and
  never as a float; payments are in the company's local currency (422
  otherwise); the schedule's dates follow the commencement date, at the
  start or the end of each interval; the contract is a draft and nothing
  is posted.
- **Depends on:** nothing.

**Later for Lease contracts (after 1 December):** changing a contract;
modifications and reassessments; extension and purchase options; variable
and index-linked payments; initial direct costs, incentives and
restoration costs; the short-term and low-value exemptions; non-lease
components; leases in a foreign currency; lessor accounting; taking
contracts over from SAP RE-FX.

### T88b Lease schedule read and the contract activated

**Capability.** The lease accountant reads the contract's schedule: the
opening lease liability as the present value of its payments, and per
period the interest, the payment, the closing liability and the
right-of-use asset's depreciation on a straight line. Activating the
contract posts the right-of-use asset against the lease liability.

- **Reuses:** `ledger.Calendar.Periods` (the company's fiscal periods),
  `ledger.Postings.Post` with `source_context` `leases`,
  `ledger.Accounts.Determination` and
  `companies[].account_determination`, `money.Allocate` for the
  depreciation's shares; exact arithmetic with `math/big.Rat`, rounded
  half away from zero to the minor unit, as `money.Scale` rounds.
- **Adds:** `GET /v1/lease-contracts/{id}/schedule`,
  `POST /v1/lease-contracts/{id}:activate`; table
  `leases.schedule_line` (written at activation, so the amounts posted
  later are the ones shown); account determination keys
  `right_of_use_asset` and `lease_liability`; permission
  `leases.contract.activate`.
- **Walkthrough:** `step_lease` extended: the schedule is read (its
  opening balance below the sum of the payments, no interest on the
  first payment, its last closing liability zero); the contract is
  activated and the two accounts moved by the right-of-use amount.
- **Acceptance:** the lease liability at commencement is the sum of the
  payments not paid on the commencement date, each discounted from its
  date at the contract's rate (how an annual rate becomes a period's
  rate is default 6 and the accounting specialist's question 2); a
  payment due on the commencement date is not in that liability and is
  added to the right-of-use asset; activation credits the liability
  account with both, so the schedule's first opening balance holds the
  commencement payment until the lessor's invoice for it is posted that
  day; each period's interest is the period's rate × the balance
  outstanding during it: the opening balance for payments in arrears,
  the opening balance less the period's payment for payments in advance;
  the last period takes only the rounding remainder so the liability
  ends at exactly zero, and a test bounds that remainder by one minor
  unit per period; the depreciation's
  shares add up to the opening right-of-use amount exactly; a draft's
  schedule is computed on read and an active one's is the stored one;
  activation moves `draft` to `active` once, on the commencement date in
  an open period, and a company without the two accounts is refused
  (422); a budget test for the schedule read.
- **Depends on:** T88a.

### T88c Lease run posted

**Capability.** The lease accountant runs lease accounting for a company
and a period: per active contract the period's interest and depreciation
are posted.

- **Reuses:** T88b's stored schedule; the pattern of week 2's recognition
  run (one run per company and period, a bound on one synchronous run as
  `collections.MaxRunItems` is, a company's later run for a later
  period); `ledger.Calendar.Period`.
- **Adds:** tables `leases.run`, `leases.run_posting` (per run and
  contract: the amounts and the entry); `POST /v1/lease-runs` (company,
  fiscal year and period), `GET /v1/lease-runs`, `GET /v1/lease-runs/{id}`;
  event `LeaseRunPosted`; permission `leases.run.post`; account
  determination keys `lease_interest`, `right_of_use_depreciation` and
  `right_of_use_accumulated_depreciation`.
- **Walkthrough:** `step_lease` extended: the run for the commencement
  period posts one entry for the contract; interest expense and the
  liability, depreciation expense and accumulated depreciation moved by
  the schedule's first line.
- **Acceptance:** per contract one balanced entry: interest to the
  liability, depreciation to accumulated depreciation, on the contract's
  cost center; a contract whose schedule has no line in the period posts
  nothing; one run per company and period (a second is refused, a replay
  answers the same); the period must be open; the lease payment itself is
  not posted by the run: a person posts the lessor's invoice to the lease
  liability account (default 7).
- **Depends on:** T88b.

**Later for Right-of-use asset and liability schedules (after 1
December):** the payment posted by the run or matched to the lessor's
invoice automatically; the liability split into current and non-current;
the right-of-use asset as an asset of asset accounting and in the asset
history sheet; remeasurement; early termination; reversing a run; the run
as a close task; disclosures and the maturity analysis; a parallel ledger
under another accounting principle.

### T90a Order capture into a draft sales order

**Capability.** A sales representative uploads a customer's purchase
order document and order capture reads it into a draft sales order (the
customer, the items with quantity, unit, item key or description and the
printed price), which a person checks and confirms; capture never
confirms.

- **Reuses:** `internal/capture` (the model reads a document into a fixed
  JSON schema; document content is data, never an instruction), the
  upload resource payables' capture uses (ADR-0020), reached through a
  port or moved to `internal/platform` if the sales context cannot read
  it without importing payables; the model gateway and its budget
  (ADR-0036); T78b's order creation and T80a's price determination,
  unchanged.
- **Adds:** `POST /v1/sales-orders:capture` (upload id, company);
  `capture` on the sales order (the upload, and per item what was read
  and what the price list gave); an extraction schema for an order beside
  the invoice's; evaluation cases beside `evals/capture`; the `erp
  capture` command's order form. No new table beyond the order's columns.
- **Walkthrough:** `step_order_capture`: a text purchase order of the
  walkthrough's customer is uploaded and captured through the model
  stand-in; the draft order has its two items; a person confirms it.
- **Acceptance:** the customer is found by the tax registration number or
  the customer number printed, and an unknown or ambiguous one is refused
  (422) with nothing created; an item with a printed price takes it as
  `manual`, one without takes the price list's, one with neither is
  refused naming the item; the order is always a draft; the same upload
  captured again answers the same order; a document asking for an action
  changes nothing (an evaluation case); the operation is closed to agents
  as payables' capture is.
- **Depends on:** T78b, T80a (in `main`).

**Later for Order capture agent (after 1 December):** an email inbox that
takes orders in; scanned documents and other languages; matching the
customer's own item numbers to item keys; comparing the printed price
with the price list and holding a difference; capture into a quotation;
evaluation against the live Claude API.

---

## Lane B, analytics and planning

### T91a Sales metrics

**Capability.** An analyst asks for order intake and billed value by
customer, item key and month.

- **Reuses:** the semantic model (`internal/analytics/semantic.yaml`, the
  configuration's analytics section and its approval loop), the fact
  datasets and their permissions (`analytics.FactDatasets`,
  `analytics.Source`), the store's extract and its parity test
  (`internal/analytics/extract`, `store`), the rule that names of
  partners are shown only to callers who may read them (as vendor names
  are).
- **Adds:** the dataset `sales_lines` (confirmed sales order items by
  order date, posted billing document items by billing date), read
  through the API and extracted to the store; metrics `order_intake`,
  `billed_value` and `billing_document_count`; dimensions `customer`,
  `customer_name`, `item_key`; "orders" and "billing" among the everyday
  words.
- **Walkthrough:** `step_sales_analytics`: order intake and billed value
  by customer for the current month equal the orders and billing
  documents the earlier steps created.
- **Acceptance:** amounts are net, in the company's currency, and a query
  across currencies is refused as the currency guard refuses it; a caller
  without the sales read permission gets no row of the dataset; the store
  and the API path give the same figures (parity test); draft orders and
  unposted billing documents are not counted; the model change is in the
  seed and reaches a tenant as a metric change does.
- **Depends on:** T78b, T79b (week 2).

**Later for Sales metrics (after 1 December):** open order value and
backlog; delivery performance; discounts and price realisation; quotation
conversion; credit memos and returns rates; revenue recognized against
billed; metrics by sales organization or representative.

### T91b Spend cube

**Capability.** A buyer analyses spend by supplier, account, cost center
and month, under a contract and outside one.

- **Reuses:** the invoices dataset and its metrics (`invoice_value`,
  `purchase_order_value`), the dimensions `vendor`, `vendor_name`,
  `account`; the purchase order's contract reference
  (`internal/procurement/purchase_orders.go`).
- **Adds:** the dataset `spend_lines` (posted supplier invoice items with
  their account, cost center, order and the order's contract); metrics
  `spend`, `spend_under_contract`, `spend_off_contract` and
  `off_contract_share`; dimensions `cost_center` and `contract`; "spend"
  among the everyday words now means `spend`.
- **Walkthrough:** `step_spend`: spend by supplier equals the posted
  invoices of the earlier steps and reads as off contract, since no
  order of the walkthrough names a contract; the under-contract case is
  the package's tests'.
- **Acceptance:** spend is the net amount of posted invoices less posted
  credit memos, by posting date; an invoice without an order is off
  contract; parity between the store and the API path; the caller's
  company access and the payables read permission bound the rows.
- **Depends on:** nothing.

**Later for Spend cube (after 1 December):** spend categories and their
taxonomy; spend by material group and catalog; price variance against
contract and catalog prices; payment terms and discount capture; maverick
buying rules; spend in the group's currency across companies; supplier
consolidation views.

### T92b Margin report from the general ledger

**Capability.** A controller reads the margin report of a company and a
period: per profit center the revenue, the cost of sales on the accounts
the configuration names, and the margin.

- **Reuses:** the profit center balances (`internal/ledger`
  `centerbalances.go`, `listProfitCenterBalances`), the chart's
  account types, the configuration's validation and tiers.
- **Adds:** the configuration section `controlling.margin_report`
  (`cost_of_sales_accounts[]` per chart, as account numbers or ranges);
  `GET /v1/margin-report` (company, fiscal year, period from and to);
  permission `controlling.margin.read`; ADR-0075.
- **Walkthrough:** `step_margin`: with a cost of sales account named, the
  report shows the billed revenue of the earlier steps, a manually posted
  cost of sales and their difference for the walkthrough's profit center.
- **Acceptance:** revenue is the balance of the accounts of type revenue,
  cost of sales that of the named accounts, margin their difference, each
  exact from the balances; lines without a profit center are one row
  named as unassigned, so the rows add up to the company's totals; a
  chart without named accounts shows revenue and a zero cost of sales and
  says so; a budget test (3 s at p95).
- **Depends on:** nothing (T56's profit centers are in `main`).

**Later for Margin reporting (after 1 December):** several margin levels
and their account groups; the report by segment and by hierarchy node;
comparison with the plan and the previous year; the report in the group's
currency; drill-down to journal entries; export.

### T93a Planning version copied

**Capability.** A controller copies a plan category's lines of a fiscal
year into another plan category as a new version, scaled by a percentage,
and reads it in the plan and actual reports.

- **Reuses:** plan categories, plan batches and their lock
  (`internal/controlling` `categories.go`, `batches.go`,
  `applyPlanBatch`, the lock the product owner
  had enforced on plan batches, 2026-10-27), `money.Scale`, the plan and
  actual reads (`planactual.go`).
- **Adds:** `POST /v1/plan-categories/{id}:copy` (target category, fiscal
  year, percentage); the copy recorded as one plan batch of the target
  with its origin. No new table.
- **Walkthrough:** `step_plan_version`: a plan category with lines for the
  walkthrough's cost centers is created (the walkthrough has none yet) and
  copied into a new one at 105 percent; the cost center plan and actual
  report reads the new category's figures.
- **Acceptance:** each line is copied to the same center, account and
  period, its amount scaled and rounded half away from zero; a locked
  target is refused as a batch into it is; a target that already has
  lines for the year is refused (409) unless the request says to replace
  them; a replay answers the same batch; the source is not changed.
- **Depends on:** nothing (T56g is in `main`).

**Later for Planning versions (after 1 December):** planning on accounts
without a center; top-down distribution and seasonal spreading; planning
in quantities and drivers; approval of a version; comparison of two
versions line by line; rolling forecasts; plan data loaded from a
planning tool.

### T92a Net revenue by profitability segment

**Capability.** A controller reads net revenue by profitability segment
(customer and item key) per period from posted billing documents: gross
value, discounts, credit memos and net revenue.

- **Reuses:** `sales.Orders` with the posted billing T79b put there and
  the posted credits T85d adds, the items' conditions (T80b: price and
  discounts), through a port the controlling context declares and the
  composition root wires; `ledger.Calendar.Periods`;
  `partner.Payees.Names`.
- **Adds:** `GET /v1/profitability-report` (company, fiscal year, period
  from and to, optionally a customer); permission
  `controlling.profitability.read`; recorded in ADR-0075.
- **Walkthrough:** `step_profitability`: the report shows, for the
  customer and item key of the earlier steps, the billed gross value, the
  discount of `step_price_list`, the credit memo of
  `step_credit_memo_request` and the net revenue.
- **Acceptance:** per segment, gross value less discounts less credit
  memos is net revenue, exact in minor units, and the rows add up to the
  net of the period's posted billing documents less its posted credit
  memos; an item without an item key is grouped under its customer alone;
  amounts are in the company's currency; no cost is shown, and the
  representation says so; a budget test (3 s at p95).
- **Depends on:** T79b, T80b (week 2), T85d.

**Later for Profitability segments (after 1 December):** cost of sales
and contribution margins per segment (needs stock valuation, outside the
required scope); segments on journal lines and derived at posting;
further characteristics (region, sales organization, product group);
allocation of overhead to segments; plan against actual per segment;
segments taken over from SAP CO-PA.

### T94b Drill-through to documents

**Capability.** An analyst drills from a row of a query's result to the
documents behind it: the journal entries, purchase orders or supplier
invoices that make up the figure.

- **Reuses:** the query engine and its guards (`analytics.Execute`, the
  store's reader), the facts' document references (added to a dataset
  that lacks one), cursor paging.
- **Adds:** `POST /v1/analytics-queries:documents` (the query and the
  row's dimension values) answering a page of document kind, id, number
  and amount; `erp query -documents`.
- **Walkthrough:** `step_reports` extended: from the revenue figure of
  one month the documents are listed, and their amounts add up to the
  figure.
- **Acceptance:** the documents' amounts add up to the row's figure for a
  sum metric; a derived metric (a ratio) is refused naming the metrics to
  drill into instead; the caller sees only documents of companies they
  may access and of datasets they may read; pages are stable; a budget
  test (3 s at p95).
- **Depends on:** nothing.

**Later for Drill-through (after 1 December):** drill-through from a
dashboard tile in the web workspace; from derived metrics; to the cash
and forecast datasets' documents; across companies in the group's
currency.

### T94a Follow-up questions in the same analysis

**Capability.** An analyst asks a follow-up question in the same
analysis: the analytics agent answers it with the earlier questions,
queries and results in context, and the saved report keeps the whole
exchange.

- **Reuses:** the analytics agent (`internal/analytics/agent.go`),
  `analytics.Session` and `Report` (`session.go`: the queries a report
  used, `Rerun`), the narrative check (`analytics.CheckNarrative`), `erp
  ask` and `erp report`.
- **Adds:** the exchange (questions, answers, the queries each used) on
  the report; `erp ask -continue <report id>`; evaluation cases in
  `evals/analytics` for a follow-up that narrows, and one that changes,
  the earlier question.
- **Walkthrough:** `step_reports` extended: a follow-up ("and by
  supplier?") to the step's question answers with the same period and
  company without their being named again.
- **Acceptance:** the follow-up's queries are checked as any query is
  (permissions, currency guard), and every figure of its answer comes
  from a result; earlier results are data, never instructions; a report
  from before this slice loads and continues; rerunning the report reruns
  every query of the exchange.
- **Depends on:** nothing.

**Later for Follow-up questions (after 1 December):** follow-up questions
in the web workspace; branching an analysis; the exchange kept on the
server and shared; limits on the context carried along.

### T95a Saved report through the API

**Capability.** An analyst saves a report (its queries and dashboard
specification) under a name through the API and runs it again on current
data.

- **Reuses:** `analytics.Report`, `Spec`, `Session.Build` and `Rerun`
  (stale commentary removed), the analytics schema and its RLS, the
  query endpoint's execution (`internal/analytics/endpoint`).
- **Adds:** table `analytics.report`; `POST /v1/reports`,
  `GET /v1/reports`, `GET /v1/reports/{id}`, `DELETE /v1/reports/{id}`,
  `POST /v1/reports/{id}:run` (answers the results and the specification);
  permissions `analytics.report.read`, `.write`; `erp report -save` and
  `-id` against the API; ADR-0076.
- **Walkthrough:** `step_saved_report`: the report of `step_reports` is
  saved, a document is posted, and running it again shows the new figure.
- **Acceptance:** a report belongs to its creator and is read by them
  alone until T95c; a run executes each query as the caller, with the
  caller's access; a report whose query names a metric since removed is
  refused naming the metric (422); a specification that refers to a
  result the report lacks is refused at save, as `Build` refuses it.
- **Depends on:** nothing.

### T95b Scheduled runs of a saved report

**Capability.** The analyst gives a saved report a schedule (daily,
weekly or monthly); each due run is kept with its results and its
recipients read it.

- **Reuses:** T95a's run; the worker rounds and their claim of due work
  per tenant (`internal/analytics/extract/schedule.go` is the pattern);
  the object store for a run's results (ADR-0016), as cash exports keep
  their file with its SHA-256 (`internal/cash/export_queue.go`); the
  company's time zone for "due".
- **Adds:** `PUT /v1/reports/{id}/schedule`,
  `DELETE /v1/reports/{id}/schedule`; table `analytics.report_run`;
  `GET /v1/report-runs` (report), `GET /v1/report-runs/{id}` (the results
  as JSON or CSV); event `ReportRunCompleted`; the round in the worker.
- **Walkthrough:** `step_saved_report` extended: a daily schedule is set,
  the worker's round is run once, and the run is listed and read as CSV.
- **Acceptance:** a due run executes as the report's owner with the
  owner's access on that day; an owner deactivated since is refused and
  the run recorded as failed with that reason; a schedule's run for a day
  is made once however many workers claim it (run-twice test); a failing
  query fails the run and leaves the schedule in place; without object
  storage the schedule is refused (503), as an export is.
- **Depends on:** T95a.

**Later for Scheduled delivery (after 1 December):** delivery by email
and to a file share (needs email transmission, not built); delivery only
when a condition holds; bursting one report per company or cost center;
retention of old runs; a schedule in the web workspace.

### T95c Report shared, each viewer with their own access

**Capability.** The owner shares a saved report with people or roles;
each viewer runs it with their own company access, and a kept run is read
only by a viewer whose access covers its companies.

- **Reuses:** T95a's report and T95b's runs; principals and roles
  (`internal/identity`) through a port; company access as the query
  endpoint checks it.
- **Adds:** table `analytics.report_share`;
  `POST /v1/reports/{id}/shares`, `GET /v1/reports/{id}/shares`,
  `DELETE /v1/reports/{id}/shares/{share_id}`; `shared` among the report
  list's filters.
- **Walkthrough:** `step_saved_report` extended: the report is shared
  with a person who may access one of two companies; their run shows that
  company's rows only, and the owner's kept run over both is refused to
  them.
- **Acceptance:** a viewer reads and runs but does not change or share
  the report; a run by a viewer is computed with the viewer's access and
  dataset permissions; a kept run names the companies it covered and is
  refused (403) to a viewer who lacks one of them; ending a share ends
  the access at once; a share to an agent is refused.
- **Depends on:** T95b.

**Later for Sharing (after 1 December):** sharing with edit rights; links
for people outside the tenant; folders and favourites; comments on a
shared report; sharing in the web workspace.

### T95d Excel workbook

**Capability.** An analyst downloads a query's result or a kept report
run as an Excel workbook, amounts as numbers with their currency's
decimals.

- **Reuses:** the CSV export (`analytics.WriteCSV`, `erp query -csv`) for
  the columns and their order; the standard library's `archive/zip` and
  `encoding/xml`: no new dependency (default 9).
- **Adds:** the workbook media type on `POST /v1/analytics-queries` and
  `GET /v1/report-runs/{id}`; `erp query -xlsx`; a writer in
  `internal/analytics`.
- **Walkthrough:** `step_reports` extended: a query is downloaded as a
  workbook, which the step unzips and whose sheet holds the query's rows.
- **Acceptance:** one sheet, a header row, a row per result row; an
  amount is written as a number from its minor units and exponent with no
  float in between; text that starts with `=`, `+`, `-` or `@` is written
  as text, never as a formula; a test reads the workbook back and
  compares it with the CSV; the file format's details are checked against
  the standard's text, which the coordinator supplies (question 8).
- **Depends on:** T95b for the run's download, and the format's text as
  a file (question 8), for which it is the lane's last slice.

**Later for Excel export (after 1 December):** several sheets with
formatting and charts; a workbook that refreshes from the API; export of
operational lists (open items, aging, worklists); templates per report.
PDF export stays left (product owner, 2026-11-04).

### T96a Metric watcher

**Capability.** An analyst sets a watcher on a governed metric (its
filter, its period, and a condition: above or below a value, or changed
against the previous period by more than a percentage) and evaluates it;
a condition that holds raises an alert.

- **Reuses:** the query endpoint's execution and its guards; the semantic
  model's metrics and dimensions; the analytics schema.
- **Adds:** tables `analytics.metric_watcher`, `analytics.metric_alert`;
  `POST /v1/metric-watchers`, `GET /v1/metric-watchers`,
  `GET /v1/metric-watchers/{id}`, `DELETE /v1/metric-watchers/{id}`,
  `POST /v1/metric-watchers/{id}:evaluate`; event `MetricAlertRaised`;
  permissions `analytics.watcher.read`, `.write`; ADR-0077.
- **Walkthrough:** `step_metric_watch`: a watcher on spend above a value
  is set and evaluated; the alert names the figure and the threshold.
- **Acceptance:** a watcher names one metric, a period grain and one
  company or all the creator may access; evaluation runs the query as the
  watcher's creator; a condition that holds raises one alert per watcher
  and period (a second evaluation raises none); a metric since removed
  makes the watcher fail with that reason; percentages are decimal
  strings compared exactly.
- **Depends on:** nothing.

**Later for Metric watchers (after 1 December):** conditions on ratios
and on several metrics; watchers per dimension value; a watcher proposed
by the analytics agent; pausing and snoozing; watchers as configuration
with approval.

### T96b Alerts raised after each refresh, listed and acknowledged

**Capability.** Watchers are evaluated after each refresh of the
analytical store, a deviation from the average of the earlier periods
among their conditions; a controller lists the open alerts of the
companies they may access and acknowledges one.

- **Reuses:** T96a's evaluation; the store's refresh rounds
  (`extract.Scheduler`) to trigger it; company access.
- **Adds:** the condition `deviation` (the figure differs from the
  average of the previous n periods by more than a percentage);
  `GET /v1/metric-alerts` (status, watcher),
  `POST /v1/metric-alerts/{id}:acknowledge`; permission
  `analytics.alert.read`, `.acknowledge`; the evaluation in the worker.
- **Walkthrough:** `step_metric_watch` extended: a deviation watcher is
  set, a large invoice is posted, the store is refreshed and the alert is
  listed and acknowledged.
- **Acceptance:** an evaluation after a refresh is idempotent (run-twice
  test: no second alert); a deviation needs n earlier periods with data
  and raises nothing without them; an alert is listed only to a caller
  whose access covers the watcher's companies; acknowledging moves open
  to acknowledged once and is audited; a budget test for the list.
- **Depends on:** T96a.

**Later for Anomaly alerts (after 1 December):** alerts shown in the web
workspace; notification by email or webhook; statistical anomaly
detection beyond a deviation from the average; the analytics agent
explaining an alert; alert routing and escalation.

### T97a Forecast of a governed metric

**Capability.** An analyst forecasts a governed metric for the coming
periods from its history by a named method (the average of the last
periods, or the same period a year before) and reads history and forecast
together.

- **Reuses:** the query endpoint's execution for the history; the
  calendar of the semantic model; integer arithmetic on minor units.
- **Adds:** `POST /v1/analytics-forecasts` (metric, filter, grain,
  periods ahead, method `moving_average` with n or `same_period_last_year`);
  recorded in ADR-0077. Nothing is stored.
- **Walkthrough:** `step_metric_forecast`: revenue is forecast for the
  next three months by the average of the months with data; the forecast
  equals that average.
- **Acceptance:** the forecast is computed from the history the caller
  may read, exactly, rounded half away from zero; too little history for
  the method is refused naming what is missing (422); a count metric is
  forecast as a whole number; a ratio is refused; the answer names the
  method and the periods it used, and says it is an arithmetic
  projection.
- **Depends on:** nothing.

**Later for Forecasts on governed metrics (after 1 December):** trend and
seasonal models and their accuracy measured against actuals; confidence
ranges; forecasts kept and compared over time; forecasts per dimension
value; forecasts written into a plan category.

### T97b Scenario on a forecast

**Capability.** The analyst applies a scenario to a forecast (percentage
changes per dimension value, such as a company or an account) and reads
baseline and scenario side by side.

- **Reuses:** T97a's forecast and its arithmetic.
- **Adds:** `scenario` on `POST /v1/analytics-forecasts` (`adjustments[]`:
  a dimension, a value, a percentage); baseline, scenario and their
  difference per period in the answer.
- **Walkthrough:** `step_metric_forecast` extended: revenue of one
  account raised by ten percent; the scenario's total exceeds the
  baseline by ten percent of that account's forecast.
- **Acceptance:** an adjustment applies to the rows of its dimension
  value, forecast separately and summed, so baseline and scenario each
  add up; two adjustments on one row are refused (422); a dimension the
  metric lacks is refused; nothing is stored.
- **Depends on:** T97a.

**Later for Scenario modeling (after 1 December):** scenarios kept under
a name and shared; drivers and formulas between metrics; scenarios on
plan versions and on the liquidity forecast; several scenarios compared;
the planning agent proposing scenarios.

---

## Lane C, governance, records and delivery tools

### T98a Control library with test results

**Capability.** A compliance officer records a control (code, objective,
process, owner, frequency, manual or automated) and the result of testing
it (passed or failed, with a note of the evidence), and reads the library
with each control's last test.

- **Reuses:** principals' names for the owner, `etag`, audit, outbox,
  cursor paging.
- **Adds:** the leaf context `grc` (schema, RLS, outbox, its row in
  `internal/layout`); tables `grc.control`, `grc.control_test`
  (insert-only); `POST /v1/controls`, `GET /v1/controls`,
  `GET /v1/controls/{id}`, `PATCH /v1/controls/{id}`,
  `POST /v1/controls/{id}:retire`, `POST /v1/controls/{id}/tests`,
  `GET /v1/controls/{id}/tests`; event `ControlChanged`; permissions
  `grc.control.read`, `.write`, `.test`; ADR-0078.
- **Walkthrough:** `step_controls`: a control on payment release is
  recorded, tested as passed with a note, and the library shows its last
  test.
- **Acceptance:** a code is unique per tenant; a test names its date, its
  result and who tested, and is never changed; recording a test is closed
  to agents; the library lists by process and by last result; none is
  shipped.
- **Depends on:** nothing.

**Later for Control library (after 1 December):** test plans and their
schedule; sign-off of a control by its owner; issues and remediation
plans; evidence attached as files; controls per company; a library
shipped with a country pack; controls changed through approval.

### T98b Control matrix

**Capability.** The compliance officer links a control to what enforces
it in the product (a permission, a segregation-of-duties rule, a path of
the configuration) and to the references of a framework the tenant names,
and reads the control matrix with whether each link still holds.

- **Reuses:** T98a's controls; identity's permissions and SoD rules
  (`listSodRules`) and the active configuration version
  (`getConfigVersion`) through ports.
- **Adds:** table `grc.control_link`; `POST /v1/controls/{id}/links`,
  `DELETE /v1/controls/{id}/links/{link_id}`; `GET /v1/control-matrix`;
  `framework_references[]` (free text) on the control.
- **Walkthrough:** `step_controls` extended: the control is linked to the
  payment release permission and to the SoD rule of the walkthrough's
  tenant; the matrix shows both links as holding and the last test.
- **Acceptance:** a link to a permission holds while the permission
  exists in the contract, one to an SoD rule while the rule is in force,
  one to a configuration path while the active version has a value
  there; a link that no longer holds is shown as broken, never removed; no
  framework's text or numbering is shipped: the references are the
  tenant's (default 11); a budget test (3 s at p95).
- **Depends on:** T98a.

**Later for SOX control mapping (after 1 December):** a shipped mapping
to a named framework (each reference needs a compliance specialist);
general IT controls of the hosting evidenced from the delivery pipeline;
the matrix as an auditor's export; coverage reports per process and
assertion.

### T98c Risk register

**Capability.** A risk owner records a risk (title, category, owner,
likelihood and impact from 1 to 5, the controls that mitigate it), rates
it again and closes it; the register lists the risks by score.

- **Reuses:** the `grc` context, T98a's controls, principals' names.
- **Adds:** tables `grc.risk`, `grc.risk_rating` (insert-only history),
  `grc.risk_control`; `POST /v1/risks`, `GET /v1/risks`,
  `GET /v1/risks/{id}`, `PATCH /v1/risks/{id}`,
  `POST /v1/risks/{id}/ratings`, `POST /v1/risks/{id}:close`; event
  `RiskChanged`; permissions `grc.risk.read`, `.write`.
- **Walkthrough:** `step_risks`: a risk of duplicate payments is recorded
  with the control of `step_controls`, rated 2 by 4, rated again 1 by 4
  and listed with its score.
- **Acceptance:** the score is likelihood × impact of the latest rating;
  each rating keeps who rated and when; a linked control must exist and
  not be retired; closing moves open to closed once with a reason; the
  list is by score, then by title.
- **Depends on:** T98a.

**Later for Risk register (after 1 December):** risk assessments on a
schedule; inherent and residual ratings; key risk indicators from
governed metrics; treatment plans and their tasks; risk appetite and
thresholds; a heat map; risks per company.

### T99a Retention rules and status

**Capability.** A records administrator sets retention rules in the
configuration (per company and document kind, the years a document is
kept after its fiscal year ends; none is shipped) and reads which fiscal
years of a document kind are past retention.

- **Reuses:** the configuration document, its validation, tiers and
  approval (`internal/config`), `ledger.Calendar.Periods` for the fiscal
  years' ends, the company's date.
- **Adds:** the configuration section `companies[].retention[]` (document
  kind, years); the leaf context `records`; `GET /v1/retention-status`
  (company, document kind): per fiscal year its end, the date its
  retention ends and whether it is past; permission
  `records.retention.read`; ADR-0079. Document kinds of the first
  version: `journal_entry` and `personal_data`.
- **Walkthrough:** `step_retention`: a rule of a number of years for
  journal entries is activated through a change request; the status shows
  the migrated prior year as not past retention, and with a rule of zero
  years as past.
- **Acceptance:** years are a whole number from 0 to 99; a kind has one
  rule per company; a company without a rule for a kind has nothing past
  retention; retention ends the stated years after the fiscal year's last
  day; the rule changes only through the configuration's approval; no
  period of any jurisdiction is shipped or defaulted (question 3).
- **Depends on:** nothing.

**Later for Retention rules (after 1 December):** rules shipped with a
country pack (each period needs the legal source); legal holds; retention
counted from another date than the fiscal year's end; rules per document
type and per business partner; retention of object storage evidence and
of audit events.

### T99b Archiving run

**Capability.** The records administrator starts an archiving run for a
company, a document kind and a fiscal year past retention: its documents
are written to an archive file in object storage with a manifest and its
SHA-256, and the year reads as archived; nothing is deleted.

- **Reuses:** T99a's status; the queue-and-worker pattern of cash exports
  (`internal/cash/export_queue.go`, `exporter.go`: queued in the request's
  transaction, written by a worker, the file's SHA-256 kept); the object
  store (ADR-0016); the ledger's journal read through a port.
- **Adds:** tables `records.archiving_run`; `POST /v1/archiving-runs`,
  `GET /v1/archiving-runs`, `GET /v1/archiving-runs/{id}`,
  `GET /v1/archiving-runs/{id}/file`; `archived` per fiscal year in the
  retention status; event `ArchivingRunCompleted`; permission
  `records.archive.run`. The first version archives journal entries.
- **Walkthrough:** `step_retention` extended: with the zero-year rule a
  run for the prior year is started, the worker's round is run, and the
  file's manifest counts the year's entries and matches its SHA-256.
- **Acceptance:** a year not past retention, or a year not closed, is
  refused (422); one completed run per company, kind and year (a second
  answers the first); the worker's round is idempotent (run-twice test:
  one file); the file holds every entry of the year with its lines in a
  documented JSON Lines form, and the manifest their count and the sum of
  their debits; no row of the ledger is changed or removed (default 12,
  question 2); a requester deactivated since is refused.
- **Depends on:** T99a.

**Later for Archiving runs (after 1 December):** deleting archived
documents from the database (needs the product owner: the ledger and the
audit trail are append-only, ADR-0006, ADR-0011); reading a document back
from the archive; archiving on a schedule; document kinds beyond journal
entries; archive formats an auditor's tools read; destroying an archive
at the end of its retention.

### T100a Personal data inventory

**Capability.** A privacy officer reads the personal data inventory (each
table and field holding personal data, with its category and purpose, the
audit trail's client addresses among them) and, for one business partner
or principal, where data about them is held.

- **Reuses:** the data model's table list and its test
  (`docs/tables_test.go` checks named tables exist); the rule that audit
  details carry no personal data (ADR-0026); the `records` context.
- **Adds:** the inventory as a declared list in `internal/records`
  (schema, table, column, category, purpose), with a test that every
  entry exists in the database and that every column of a list of
  known personal columns (names, addresses, bank details, subjects,
  client addresses) is in it; `GET /v1/personal-data-inventory`;
  `GET /v1/personal-data-inventory/subjects/{kind}/{id}` (per entry, how
  many rows hold data of that partner or principal); permission
  `records.privacy.read`; 03-data-model names the inventory.
- **Walkthrough:** `step_privacy`: the inventory lists the business
  partner's name and bank details and `audit.audit_event.client_address`;
  for the walkthrough's supplier it counts the rows that hold its data.
- **Acceptance:** the inventory covers every context's schema; a new
  migration adding a column of a known personal kind fails the test until
  the inventory names it; the subject read counts rows and returns no
  personal data itself; closed to agents; a budget test for the subject
  read.
- **Depends on:** T99a (the context).

**Later for the personal data inventory (after 1 December):** a subject
access export in a portable format; the inventory extended by custom
fields; legal bases and consent records; processors and transfers; the
inventory reviewed and signed off on a schedule.

### T100b Personal data blocked

**Capability.** The privacy officer blocks a business partner's personal
data once nothing is open for it: the partner's own reads and the names
other contexts take from the partner then answer its name, address and
bank details masked, except to holders of a permission, and new documents
for it are refused.

- **Reuses:** the partner's block (`internal/partner` `Block`,
  `partner.Postings.PostingBlock`, which every posting context already
  checks), the masking of bank details shown to agents
  (`internal/platform/redact`, ADR-0047), the ledger's open items read
  through the port partner has.
- **Adds:** `personal_data_blocked_at` on the business partner;
  `POST /v1/business-partners/{id}:block-personal-data`,
  `:unblock-personal-data`; permissions `partner.privacy.block` and
  `partner.privacy.read_blocked`; event `BusinessPartnerUpdated` with the
  changed field; recorded in ADR-0079.
- **Walkthrough:** `step_privacy` extended: a supplier with no open item
  is blocked; a clerk reads it masked, the privacy officer in full; an
  invoice for it is refused.
- **Acceptance:** a partner with an open item in the ledger is refused
  (409) naming the company; open orders and unposted documents are the
  person's to check in the first version, and the ADR says so; blocking
  also sets the partner's block, so entry and
  posting are refused by the checks in place; masking is at the source
  only: the partner's own reads, its bank accounts and the names other
  contexts take from the partner (`partner.Payees.Names`) answer masked
  for a caller without the permission, each with a test; a name a
  context stored on its own document, and events and exports written
  before, stay as they are and the ADR lists them as not masked; closed
  to agents.
- **Depends on:** T100a.

**Later for Personal data blocking (after 1 December):** blocking
proposed by a run at the end of purpose; blocking of principals and of
contact persons; unblocking with approval; masking of names a context
stored on its own documents; masked data in events, exports and
analytics already written; blocking per company.

### T100c Deletion run

**Capability.** The privacy officer previews and starts a deletion run:
for each business partner blocked longer than the retention rule for
personal data, its name, address and bank details are blanked; ledger
documents and audit events stay.

- **Reuses:** T99a's rule of the kind `personal_data`, T100b's block,
  the queue-and-worker pattern of T99b.
- **Adds:** table `records.deletion_run`, `records.deletion_run_subject`;
  `POST /v1/personal-data-deletion-runs:preview`,
  `POST /v1/personal-data-deletion-runs`,
  `GET /v1/personal-data-deletion-runs/{id}`; the port
  `partner.Privacy.Blank`; event `PersonalDataDeleted` (ids only);
  permission `records.privacy.delete`.
- **Walkthrough:** `step_privacy` extended: with a personal data rule of
  zero years the preview lists the blocked supplier; the run blanks its
  name and bank details; its posted invoice still reads, naming the
  partner by number.
- **Acceptance:** without a `personal_data` rule nothing is eligible; the
  preview and the run list the same subjects; blanking replaces each
  inventory field of the partner by a fixed marker and ends its bank
  accounts, in one transaction per subject, and is not reversible; no
  audit event is touched, so the audit schema and `audit.event_immutable`
  do not change; journal entries, invoices and audit events are not removed; the run is
  closed to agents, audited per subject, idempotent (run-twice test) and
  refuses a requester deactivated since.
- **Depends on:** T99a, T100b.

**Later for Deletion runs (after 1 December):** a person's erasure
blanking the client addresses of their audit events as the tenant's
erasure does (`audit.event_immutable` allows that alone; migration
000151), which the Compliance leaf names and which changes a
security-sensitive trigger; deletion on a schedule;
deleting rows in place of blanking them; personal data in object storage
files and in archives, in free-text fields and attachments; deletion
requested by the data subject with its deadline; a certificate of
deletion.

### T101a Audit evidence export

**Capability.** An auditor requests an evidence export for a company and
a period: the audit events, the journal entries with their approval
decisions and the result of verifying the audit chain, written as files
with a manifest and SHA-256 to object storage and downloaded.

- **Reuses:** the audit trail's read and the chain's verification
  (`internal/audit`, `GET /v1/audit-events`, `verifyAuditChain`), the
  ledger's journal and the approval requests read through ports, the
  queue-and-worker pattern of T99b, the object store.
- **Adds:** table `audit.evidence_export`;
  `POST /v1/audit-evidence-exports`, `GET /v1/audit-evidence-exports`,
  `GET /v1/audit-evidence-exports/{id}`,
  `GET /v1/audit-evidence-exports/{id}/files/{name}`; permission
  `audit.evidence.export`; event `AuditEvidenceExported`.
- **Walkthrough:** `step_audit` extended: an export for the current
  period is requested, the worker's round is run, and the manifest's
  counts equal the period's audit events and journal entries and name the
  chain as verified.
- **Acceptance:** the files are JSON Lines (audit events; journal entries
  with lines; approval decisions with who decided) and a manifest with
  each file's count and SHA-256 and the verification's result; the export
  covers the companies the requester may access among those asked; audit
  details are exported as stored (they hold no personal data, ADR-0026)
  and client addresses only to a holder of the permission that reads them
  today; the round is idempotent; a requester deactivated since is
  refused; closed to agents.
- **Depends on:** T99b (the pattern; no data of it).

**Later for Audit evidence export (after 1 December):** samples drawn by
the auditor's criteria; evidence per control of the library; subledger
documents and their attachments; a read-only auditor role with a time
limit; export in an audit tool's format; a signed manifest.

### T103a Event log of purchase to pay

**Capability.** A process analyst reads the event log of the
purchase-to-pay flow for a company and a period: per purchase order each
activity with its time and whether a person, an agent or the system did
it, from requisition to payment, as JSON or CSV.

- **Reuses:** the audit trail (each state change's action, object, time
  and the actor's kind), the document links procurement, payables and
  payments already hold (requisition to order, order to receipt and
  invoice, invoice to payment), each read through a port.
- **Adds:** the leaf context `processmining` (no schema of its own);
  `GET /v1/process-event-logs` (process `purchase_to_pay`, company, from,
  to; JSON or CSV); permission `processmining.log.read`; ADR-0080 with
  the list of activities and the audit actions each comes from.
- **Walkthrough:** `step_process_mining`: the log for the walkthrough's
  company holds, for the order of `step_procure`, its activities in
  order, from the requisition's creation to the payment.
- **Acceptance:** a case is a purchase order; an invoice without an order
  is a case of its own; events are ordered by time, then by the audit
  sequence; actors are given as a kind (person, agent, system), never by
  name or id; the caller's company access bounds the cases; the log is
  paged by case; a budget test (3 s at p95) at 01 §1.3's order volume.
- **Depends on:** nothing.

**Later for Event logs (after 1 December):** event logs from SAP's
document tables (needs the SAP specialist for the tables and fields); the
order-to-cash and record-to-report flows; the log in XES; activities
below the document level (changes, approvals per step); the log in the
analytical store.

### T103b Process map

**Capability.** The analyst reads the process map of a flow for a period:
its activities, how often each follows another, the median time between
them and the variants by frequency.

- **Reuses:** T103a's event log.
- **Adds:** `GET /v1/process-maps` (process, company, from, to): the
  activities with their counts, the directly-follows pairs with their
  counts and median durations, the ten most frequent variants with their
  share of cases; recorded in ADR-0080. Nothing is stored and no model is
  called.
- **Walkthrough:** `step_process_mining` extended: the map shows the
  order's variant and, after a second order paid without a goods receipt
  (a service), two variants.
- **Acceptance:** counts add up: every case is in exactly one variant; a
  pair's count is the number of times its second activity directly
  follows its first; the median is of whole seconds, the lower middle for
  an even count; cases still open in the period are counted and marked as
  open; the figures are computed, never written by a model (default 13);
  a budget test (3 s at p95).
- **Depends on:** T103a.

**Later for Discovery agent process maps (after 1 December):** the
discovery agent describing a map and proposing changes; conformance
against a reference flow; bottleneck and rework analysis; filters by
supplier, amount or user; a drawn map in the web workspace; comparison of
SAP's flow with the product's.

### T104a Fit-gap report of a SAP customizing extract

**Capability.** A delivery consultant reads the fit-gap report of a SAP
customizing extract: per customizing object the configuration generator
reads, whether it was carried over, carried over with a note or left out,
with the notes.

- **Reuses:** the configuration generator and the notes it already
  returns (`internal/delivery/sapconfig`, `sapconfig.Generate`, called by
  `erp config`); only the SAP tables it already reads, nothing new about
  SAP.
- **Adds:** a structured result beside the notes (object, key, outcome
  `carried_over`, `carried_with_note` or `left_out`, note); `erp config
  -fit-gap` writing the report as text or JSON; its section in
  `docs/migration.md`.
- **Walkthrough:** `step_migrate` extended: the report for the mock SAP
  extract counts the objects carried over and lists those with a note.
- **Acceptance:** every note the generator returns today appears in the
  report under its object; the counts per outcome add up to the objects
  read; the generated configuration is byte for byte what it was (its
  golden tests unchanged); the report states that it covers customizing
  only, not usage, changes or custom code.
- **Depends on:** nothing.

**Later for Fit-gap report (after 1 December):** gaps from usage analysis
and change-log mining (their own leaves); custom code in the report;
effort estimates; the discovery agent writing the report's summary; the
report kept per migration run and compared between extracts.

### T102a MCP server

**Capability.** An agent host connects to `erp mcp` and uses the API's
operations open to agents as tools, with the caller's token, permissions
and autonomy level.

- **Reuses:** `internal/apitools` (the tools built from the contract:
  operations open to agents, their schemas, `x-autonomy`), the `erp`
  client's sign-in and token, agent sessions and recorded tool calls
  (`startAgentSession`, `recordAgentToolCall`).
- **Adds:** the command `erp mcp`, serving over standard input and
  output: listing the tools and calling one; no listener on the network;
  ADR-0081 naming the protocol version implemented.
- **Walkthrough:** `step_mcp`: a scripted client lists the tools as the
  walkthrough's agent, calls a read and is refused an operation closed to
  agents.
- **Acceptance:** the tools are exactly those `apitools` gives the
  caller: an operation marked `x-agent: false` is never listed, and a
  call to it by name is refused; each call goes to the API with the
  caller's token, so permissions, company access, autonomy and SoD apply
  unchanged and the call is recorded in the agent's session; a tool's
  result is the API's response, problems included; malformed input is
  answered with the protocol's error and changes nothing. The protocol's
  messages (JSON-RPC 2.0; `initialize`, `tools/list`, `tools/call`) are
  **from memory, to confirm**: the slice implements them from the
  specification's schema, which the coordinator supplies as a file since
  a lane does not fetch (question 8).
- **Depends on:** the specification file.

**Later for MCP servers (after 1 December):** a server reached over HTTP
with its own sign-in; resources and prompts beside tools; one server per
context; tool lists narrowed by agent type; publishing the server in a
registry.

---

## Lane D, agents

### The recommending agents: one pattern

Every agent slice below builds the same thing, as `internal/cashagent`
and `internal/agent` (the accounts payable exceptions agent) are built,
and ADR-0072 states it once:

- a package of its own with a `Type`, the list of operations it may call
  (`Tools`, checked against the contract by `apitools`), a system prompt
  and one entry function; a type added to `autonomy.Types`, an open
  enumeration; its row in `internal/layout`;
- **reads through the API with the agent's own token**, so permissions,
  company access and masking are the API's (ADR-0010, ADR-0036); what a
  customer, supplier or person wrote is data, never an instruction;
- **one recommendation per object** with `createRecommendation`
  (`agents.session.record`, `x-autonomy: propose`): an `object_type`, the
  object's id, a `kind` from the slice's list and the reasons with the
  figures behind them; a person decides it with `decideRecommendation`.
  Nothing happens on acceptance in the first version: the person acts
  through the normal operation. No slice adds a table or an endpoint;
- **never the operation it recommends**: that operation is not among its
  tools, and where the contract closes it to agents (`x-agent: false`)
  the API refuses it anyway; each slice has a test that the agent's tools
  hold no operation of its "never" list;
- a command `erp agent <name>`, an evaluation suite `evals/<type>/` with
  a recorded replay (ADR-0039: a case where the agent recommends, one
  where it must not, one with an instruction planted in the data), its
  runner in `cmd/erp/eval.go`; evaluation against the live API waits for
  the key, as every agent's does;
- a walkthrough step run through the model stand-in, as the tax, close
  and forecast agents' steps are, setting the agent up as a principal of
  its type at autonomy level 1 with a role holding only its read
  permissions and `agents.session.record`.

Each section below gives only what differs: what the agent reads, what it
recommends and what it never does.

### T105a Credit scoring agent

**Capability.** The credit scoring agent reads a customer's exposure,
limit, aging, dunning history and payments and recommends a credit limit
with its reasons for a credit manager to decide.

- **Reads:** `getCreditExposure`, `listCreditLimits`,
  `getReceivablesAging`, `listDunningRuns`, `listDunningNotices`,
  `listIncomingPayments`, `listCustomerInvoices`, `getBusinessPartner`.
- **Recommends:** on `credit_limit` (or the customer when it has none):
  `raise_limit`, `lower_limit`, `keep_limit`, `set_limit`, with the
  amount and the figures it rests on. Type `credit_scoring`; ADR-0072.
- **Never:** `createCreditLimit`, `updateCreditLimit`, releasing an
  order's credit block.
- **Walkthrough:** `step_credit_agent`: for the customer of
  `step_credit_limit` the agent leaves one recommendation naming the
  limit, the exposure and the overdue amount; the credit manager rejects
  it.
- **Acceptance:** every amount in the recommendation is one the API
  returned (checked as the forecast agent's citations are); a customer
  with no history gets `keep_limit` or none and a reply saying so; the
  agent gives no numeric score (default 13).
- **Depends on:** T77a, T76a (in `main`).

**Later for Credit scoring agent (after 1 December):** a numeric score
from a stated model and its back-testing; outside credit ratings and
agency data; scoring on a schedule and at each order; the recommended
limit applied on acceptance; scoring groups of customers; evaluation
against the live Claude API.

### T105c Bid comparison agent

**Capability.** The bid comparison agent reads an RFQ's comparison and
recommends an award with its reasons; it never awards.

- **Reads:** the RFQ, its quotations and their comparison (T82a–T82c:
  `getRfq`, `listSupplierQuotations`, the comparison read),
  `getBusinessPartner`, `listContracts`, `listPurchaseOrders`.
- **Recommends:** on `rfq`: `award` naming one quotation, or `no_award`,
  with the totals, the items where another supplier is cheaper and the
  quotations left out as incomplete or expired. Type `bid_comparison`.
- **Never:** the award (T82d), publishing an RFQ, recording a quotation.
- **Walkthrough:** `step_rfq` extended before the award: the agent
  recommends the cheaper complete quotation; the buyer accepts the
  recommendation and awards as before.
- **Acceptance:** the recommended quotation is one the comparison ranks
  (a test refuses a recommendation naming an unranked one in the agent's
  own guard); an RFQ without a complete, valid quotation gets `no_award`.
- **Depends on:** T82c (week 2). The leaf is week 2's; its "later" leaf
  is week 2's "bid comparison variants".

### T106a Access agent: roles from actual usage

**Capability.** The access agent reads a role's permissions, who holds it
and which operations they called in a period, and recommends the
permissions the role needs with those never used, for an identity
administrator to decide.

- **Reads:** `listRoles`, `getRole`, `listRolePermissions`,
  `listRoleAssignments`, `listPrincipals`, `listAuditEvents`.
- **Recommends:** on `role`: `remove_permissions` (listing those no
  holder used in the period) or `keep`. Type `access`.
- **Never:** any identity write; they are people's alone.
- **Walkthrough:** `step_access_agent`: for a role holding a permission
  nobody used, the agent recommends removing it and names it.
- **Acceptance:** "used" is an audit event of an operation that needs the
  permission, by a holder, in the period; read operations leave no audit
  event, so read permissions are listed as "use not recorded", never as
  unused (the ADR says so); the agent's role holds
  `identity.access.read` and `audit.event.read` (question 7).
- **Depends on:** nothing.

**Later for Roles from actual usage (after 1 December):** roles drafted
as change requests; usage read from SAP (needs the SAP specialist); role
mining across all principals; the agent run on a schedule; evaluation
against the live Claude API.

### T106b Access agent: SoD analysis of a role

**Capability.** The access agent analyses a role as it stands or with
permissions to be added: which segregation-of-duties rules its holders
would break and how to avoid it, as a recommendation on the role.

- **Reads:** T106a's, and `listSodRules`.
- **Recommends:** on `role`: `sod_conflict` (the rules, the holders and
  the permissions that collide, and a split of the role) or
  `no_conflict`. A second entry function of the `access` agent, as the
  close agent has two.
- **Never:** granting a permission or changing a rule.
- **Walkthrough:** `step_access_agent` extended: asked about adding the
  payment release permission to the role that proposes payment runs, the
  agent names the tenant's SoD rule and the holders.
- **Acceptance:** the conflicts named are those the API's own check would
  refuse at grant time (a test grants the permission afterwards and
  compares); the analysis is started by a person; a role change does not
  start it by itself in the first version.
- **Depends on:** T106a.

**Later for SoD analysis (after 1 December):** the analysis started by
every role change event; mitigating controls from the control library;
proposed new SoD rules; analysis across companies.

### T106c Access agent: access review prepared

**Capability.** The access agent prepares an access review: per principal
its roles, when each was last used, permissions never used and
segregation-of-duties conflicts, with a recommendation to keep or remove
each role, for the reviewer to decide.

- **Reads:** T106b's.
- **Recommends:** on `principal`: `keep_roles`, `remove_roles` (naming
  them) or `review` where it cannot tell. A third entry function.
- **Never:** ending a role assignment or blocking a principal.
- **Walkthrough:** `step_access_agent` extended: for the walkthrough's
  people the agent leaves one recommendation each; the one who never used
  a role is recommended its removal.
- **Acceptance:** one recommendation per principal and run (a second run
  of the same period adds none); agents and services are reviewed as
  principals too; the review's sign-off is not built (the "Periodic
  access reviews" leaf, post-pilot).
- **Depends on:** T106b.

**Later for Access review preparation (after 1 December):** the periodic
access review itself with its sign-off; reviews per manager; removing
access on acceptance; reminders and escalation.

### T107a Regulatory agent: a notice read against the configuration

**Capability.** A tax accountant gives the regulatory agent a notice of a
regulatory change as text; it reads the notice against the tenant's
country packs, tax codes and registrations and recommends whether and
where the configuration is affected, quoting the notice.

- **Reads:** `listCountryPacks`, `listConfigVersions`,
  `getConfigVersion`, `listTaxRegistrations`, `listTaxPeriods`; the
  notice is passed with the task (`erp agent regulatory-notice -file`).
- **Recommends:** on `config_version`: `change_needed` (the paths
  affected, the passage of the notice, the date it takes effect as the
  notice states it) or `no_change`. Type `regulatory`.
- **Never:** any configuration write; fetching anything (it has no tool
  that reaches outside the API).
- **Walkthrough:** `step_regulatory_agent`: given a made-up notice of a
  rate change for a made-up code, the agent names the tax code's path
  and quotes the passage.
- **Acceptance:** every passage it quotes is in the notice, character for
  character (checked in code); the notice is data: one that asks for an
  action changes nothing (an evaluation case); the product ships no
  regulatory content and the walkthrough's notice is marked as invented;
  the agent states that a person must check the notice against its
  source.
- **Depends on:** nothing.

**Later for Regulatory change monitoring (after 1 December):** the agent
fetching the authorities' publications on a schedule (needs outbound
access and a list of sources); notices in other languages; a register of
notices with their status; country pack releases triggered by a notice;
evaluation against the live Claude API.

### T107b Regulatory agent: the change request drafted

**Capability.** For a regulatory recommendation a person accepted, the
regulatory agent drafts the configuration change request and has it
simulated, for a person to submit and approve.

- **Reads:** T107a's, `getRecommendation`, `getChangeRequest`.
- **Writes:** `createChangeRequest`, `updateChangeRequest`,
  `simulateChangeRequest`, as the configuration agent does
  (`internal/delivery/configagent`); then a recommendation on the change
  request, `ready_to_submit` or `simulation_failed`.
- **Never:** `submitChangeRequest`, `activateChangeRequest`, a decision
  on an approval.
- **Walkthrough:** `step_regulatory_agent` extended: the tax accountant
  accepts the recommendation; the agent drafts the change request with
  the new rate from the notice's date and simulates it; a person submits
  it.
- **Acceptance:** only an accepted recommendation of T107a's kind is
  drafted from (an open or rejected one is refused by the agent's guard);
  the draft changes only the paths the recommendation named; the change
  takes effect through the configuration's own approval.
- **Depends on:** T107a.

**Later for Configuration and report updates (after 1 December):**
changes to a tax return's layout and to statutory reports; a new country
pack version drafted; golden tests drafted with a change; effective dates
checked against open periods.

### T105b Collections agent

**Capability.** The collections agent reads a company's collection
worklist, a customer's overdue items, dunning notices, payments and
dispute cases and recommends the next action per customer with a draft
message for a collections specialist to decide.

- **Reads:** `getCollectionWorklist`, `getReceivablesAging`,
  `listDunningNotices`, `listIncomingPayments`, `listCustomerInvoices`,
  the dispute cases' list (T86a), `getBusinessPartner`.
- **Recommends:** on the customer: `call`, `send_reminder`, `escalate`,
  `wait_for_dispute`, `no_action`, with the overdue figures and a short
  draft message. Type `collections`.
- **Never:** starting or confirming a dunning run, opening or closing a
  dispute case, contacting a customer (it has no tool that sends).
- **Walkthrough:** `step_collections_agent`: for the worklist of
  `step_collections` the agent leaves one recommendation per customer;
  the customer with the open case of `step_dispute` gets
  `wait_for_dispute`.
- **Acceptance:** one recommendation per customer and run; a customer
  whose only overdue items are disputed is never recommended a reminder;
  the draft message holds no figure the API did not return.
- **Depends on:** T76c (in `main`), T86a.

**Later for Collections agent (after 1 December):** promises to pay and
their follow-up; payment plans; messages sent on acceptance (needs email
transmission); prioritising by predicted payment behaviour; the agent run
on a schedule; evaluation against the live Claude API.

### T108a Support agent: how-to answers

**Capability.** A user asks the support agent how to do something; it
answers from the API contract and the tenant's active configuration,
naming the operations and the settings that apply.

- **Reads:** the contract's operations, their descriptions and problem
  codes (through `apitools`), `listConfigVersions`, `getConfigVersion`,
  `getSchema`, `getCurrentPrincipal`.
- **Answers:** a reply only (no recommendation), as `erp ask` replies.
  Type `support`; `erp agent support-ask`.
- **Never:** any write.
- **Walkthrough:** `step_support_agent`: asked how an invoice above the
  tolerance is released, the agent names the operation and the tenant's
  tolerance.
- **Acceptance:** every operation the reply names exists in the contract
  and every setting it cites is in the active configuration with that
  value (checked in code, as the analytics narrative is); a question the
  contract does not answer gets a reply saying so; the question is data.
- **Depends on:** nothing.

**Later for How-to answers (after 1 December):** answers in the web
workspace; answers from the tenant's own process documents; feedback on
answers; answers in French and Arabic; evaluation against the live Claude
API.

### T108b Support agent: root cause of a refusal or a stuck document

**Capability.** A user gives the support agent a refused request or a
stuck document; it reads the document, its audit events, its operation
and the configuration and recommends the root cause with the events that
show it.

- **Reads:** T108a's, `listAuditEvents`, `getOperation`,
  `listOperations`, `listApprovalRequests`, and the document's own read
  operation by its kind.
- **Recommends:** on the document: `root_cause` with a class
  (`configuration`, `master_data`, `missing_approval`, `closed_period`,
  `permission`, `unknown`) and the events and settings that show it. A
  second entry function; `erp agent support-diagnose`.
- **Never:** any write.
- **Walkthrough:** `step_support_agent` extended: for an invoice held
  because its period is closed the agent names the closed period and the
  audit event of the refusal.
- **Acceptance:** every event it cites is one the API returned for that
  object; `unknown` when the events do not show a cause; the service's
  logs are not read (they are not behind the API).
- **Depends on:** T108a.

**Later for Root cause (after 1 December):** reading the service's logs
and traces (needs the observability stack's query access); incidents
across tenants for operators; known-error records; diagnosis started from
an alert.

### T108c Support agent: a fix drafted as a change request

**Capability.** When the root cause is a setting, the support agent
drafts the configuration change request that fixes it and has it
simulated, for a person to submit and approve.

- **Reads and writes:** as T107b (`createChangeRequest`,
  `updateChangeRequest`, `simulateChangeRequest`), from an accepted
  `root_cause` recommendation of the class `configuration`; `erp agent
  support-fix`.
- **Never:** submitting or activating, a decision on an approval, any
  change to master data or documents.
- **Walkthrough:** `step_support_agent` extended: for an invoice held by
  a tolerance the user accepts the root cause; the agent drafts a change
  request raising the tolerance and simulates it; nobody submits it and
  the step withdraws it.
- **Acceptance:** only an accepted recommendation of the class
  `configuration` is drafted from; the draft changes one path; the
  drafting helper is the one T107b wrote, moved to a shared package, not
  copied.
- **Depends on:** T108b, T107b.

**Later for Fixes as configuration change requests (after 1 December):**
fixes to master data as partner changes; fixes proposed for a stuck
workflow; a regression suite drafted with the fix; ticketing (its own
leaf, post-pilot).

### T105d Savings opportunity agent

**Capability.** The savings opportunity agent reads the spend cube,
contracts and catalog items and recommends savings opportunities with the
figures behind each for a buyer to decide.

- **Reads:** `runAnalyticsQuery` (T91b's spend metrics),
  `getAnalyticsModel`, `listContracts`, `listCatalogItems`,
  `listPurchaseOrders`, `getBusinessPartner`.
- **Recommends:** on the company: `bring_under_contract` (spend with a
  supplier outside any contract), `consolidate_suppliers` (several
  suppliers on one account), `use_contract_price` (orders above a
  contract's price), each with its spend figures. Type `savings`.
- **Never:** creating an RFQ, a contract or an order.
- **Walkthrough:** `step_savings_agent`: with the spend of `step_spend`
  the agent recommends bringing the off-contract supplier under a
  contract and names its spend.
- **Acceptance:** every figure comes from a query result or a document
  the API returned; it states no saving in money, only the spend an
  opportunity covers (default 13); at most a stated number of
  recommendations a run.
- **Depends on:** T91b.

**Later for Savings opportunity agent (after 1 December):** opportunities
tracked to realised savings; RFQs drafted from an opportunity; market
price benchmarks; the agent run on a schedule; evaluation against the
live Claude API.

### T105e Planning agent

**Capability.** The planning agent reads a company's plan and actual
figures per center and account and recommends next year's plan lines with
its reasons for a controller, who applies them as a plan batch.

- **Reads:** `listPlanCategories`, `listPlanLines`,
  `listCostCenterPlanActual`, `listProfitCenterPlanActual`,
  `listCostCenters`, `runAnalyticsQuery`.
- **Recommends:** on a `plan_category`: `plan_proposal` with the lines
  (center, account, period, amount) and, per center, why it departs from
  the year before. Type `planning`.
- **Never:** `applyPlanBatch`, copying a category (T93a), locking one.
- **Walkthrough:** `step_planning_agent`: for the category copied in
  `step_plan_version` the agent proposes lines; the controller accepts
  and applies them as a batch.
- **Acceptance:** each proposed line names a center and an account that
  exist and may be planned; amounts are decimal strings in the company's
  currency; the reasons cite actual figures the API returned.
- **Depends on:** T93a.

**Later for Planning agent (after 1 December):** proposals written into a
plan version directly as proposals to accept line by line; drivers and
scenarios; explanations of plan and actual variances for centers (T56j);
planning across companies; evaluation against the live Claude API.

### T105f Control monitoring agent

**Capability.** The control monitoring agent reads the control library,
the control matrix, the audit trail and the access data and recommends,
per control, whether it found an exception, with the events it found, for
the compliance officer to decide.

- **Reads:** T98a's and T98b's reads, `listAuditEvents`, `listSodRules`,
  `listRoleAssignments`, `listApprovalRequests`.
- **Recommends:** on a `control`: `exception_found` (the events, and the
  link that is broken if one is) or `no_exception`. Type
  `control_monitoring`.
- **Never:** recording a test, changing a control or a link.
- **Walkthrough:** `step_control_agent`: the step creates an SoD rule
  and a control of its own linked to it, then ends the rule; the matrix
  shows the link as broken and the agent reports an exception on that
  control, naming the audit event of the rule's end. The walkthrough's
  own propose-and-release rule is not touched.
- **Acceptance:** every event it cites is one the API returned; a control
  with no link and no automated evidence gets `no_exception` only with
  the statement that nothing could be checked; one recommendation per
  control and run.
- **Depends on:** T98b.

**Later for Continuous control monitoring agent (after 1 December):**
monitoring on a schedule and on events; test results recorded on
acceptance; monitoring rules written as CEL; exceptions as issues with
remediation; evaluation against the live Claude API.

### T105g Risk agent

**Capability.** The risk agent reads the risk register, the controls and
their tests and the audit trail and recommends new ratings and risks not
yet in the register with its reasons for a risk owner to decide.

- **Reads:** T98c's and T98a's reads, the control matrix,
  `listAuditEvents`, `listRecommendations` (the control monitoring
  agent's exceptions).
- **Recommends:** on a `risk`: `rate_again` (likelihood and impact, why)
  or `keep`; on the tenant: `new_risk` (title, category, the evidence).
  Type `risk`.
- **Never:** any write to the register.
- **Walkthrough:** `step_risk_agent`: with the failed control test of
  `step_controls` the agent recommends rating the linked risk higher.
- **Acceptance:** a rating it recommends is 1 to 5 on each scale; a risk
  whose controls all passed their last test and has no exception gets
  `keep`; it cites tests and events the API returned.
- **Depends on:** T98c, T105f for the exceptions it reads.

**Later for Risk agent (after 1 December):** key risk indicators watched
through metric watchers; risks from outside sources; scenario analysis of
a risk; the agent run on a schedule; evaluation against the live Claude
API.

### T109a Enablement agent: a process described from the configuration

**Capability.** A process owner has the enablement agent write the
description of a process (purchase to pay, or order to cash) from the
tenant's active configuration and roles: who does what, with the limits
and tolerances in force, every figure taken from the configuration.

- **Reads:** `listConfigVersions`, `getConfigVersion`, `listRoles`,
  `listRolePermissions`, `listSodRules`, the contract's operations
  through `apitools`.
- **Writes:** a recommendation on the `config_version` of the kind
  `process_document`, its content the description in Markdown; `erp
  agent enablement-docs -process` also writes it to a file. Type
  `enablement`. Built in lane A.
- **Never:** any other write.
- **Walkthrough:** `step_enablement_agent`: the purchase-to-pay
  description names the tenant's approval limit and invoice tolerance
  with their configured values.
- **Acceptance:** every amount, percentage and role the document names is
  in the active configuration or the identity data with that value
  (checked in code); it names the configuration version it describes; a
  process the first version does not know is refused.
- **Depends on:** T105a (ADR-0072 and the pattern in `main`).

**Later for Process docs from live configuration (after 1 December):**
documents kept and compared between configuration versions; documents
regenerated when a change request is activated; diagrams; translation;
training material and quizzes.

### T109b Enablement agent: in-context help

**Capability.** A user asks the enablement agent what a field or a
problem code of an operation means; it explains it from the API contract
and the tenant's configuration.

- **Reads:** T109a's and `getSchema`.
- **Answers:** a reply only; `erp agent enablement-help -operation
  -field` or `-problem`. A second entry function.
- **Never:** any write.
- **Walkthrough:** `step_enablement_agent` extended: asked about the
  problem code a refused invoice returned, the agent explains it and
  names the tolerance in force.
- **Acceptance:** the operation, field or problem code must exist in the
  contract (422 otherwise, before any model call); the explanation quotes
  the contract's description and cites only settings of the active
  configuration; help beside a field in the web workspace is not built.
- **Depends on:** T109a.

**Later for In-context help (after 1 December):** help shown beside the
field in the web workspace; help through the API for other clients; help
texts kept and reviewed by the tenant; guided walkthroughs of a task.

### T110a Integration agent: failing integrations

**Capability.** The integration agent reads the tenant's connectors,
webhook subscriptions and their deliveries, bank files and long-running
operations and recommends what to do about each failing one with its
evidence, for an integration administrator to decide.

- **Reads:** `listConnectors`, `getConnector`,
  `listWebhookSubscriptions`, `listWebhookDeliveries`,
  `getWebhookDelivery`, `listBankFiles`, `listOperations`,
  `getOperation`, `listMigrationRuns`.
- **Recommends:** on a `connector`, a `webhook_subscription` or a
  `bank_file`: `redeliver`, `pause`, `check_endpoint`, `retransmit`,
  `investigate`, with the failing deliveries or statuses. Type
  `integration`. Built in lane A.
- **Never:** enabling or disabling a connector, pausing, resuming,
  redelivering, rotating a secret, transmitting a bank file.
- **Walkthrough:** `step_integration_agent`: for a webhook subscription
  whose endpoint refuses deliveries the agent recommends checking the
  endpoint and names the failed deliveries.
- **Acceptance:** secrets never reach it (the API returns none); one
  recommendation per failing object and run; a tenant with nothing
  failing gets none and a reply saying so; it generates no connector.
- **Depends on:** T105a.

**Later for Connector generation and monitoring (after 1 December):**
generating a connector from another system's interface description;
monitoring on a schedule and from alerts; redelivery on acceptance;
mapping proposals for a new interface; evaluation against the live Claude
API.

---

## Lease accounting: what the standard is taken to say

The primary source is IFRS 16 *Leases* (IASB). **Every statement below is
from memory**: the planning lane has no network and the repository holds
no copy of the standard, so none was checked against its text. Each names
the paragraph for the accounting specialist to confirm (specialist
question 1), and T88a's ADR-0073 marks each as confirmed or not. Nothing
else about IFRS 16 is assumed by the slices. The product owner approved
building revenue recognition on statements of this kind as planning
defaults (2026-11-12); whether the same holds for leases is the product
owner's question 10.

1. At the commencement date a lessee recognizes a right-of-use asset and
   a lease liability (paragraph 22).
2. The lease liability is measured at the present value of the lease
   payments not paid at that date, discounted at the interest rate
   implicit in the lease if it can be readily determined, otherwise at
   the lessee's incremental borrowing rate (paragraph 26).
3. The right-of-use asset is measured at cost: the initial amount of the
   lease liability, plus payments made at or before commencement less
   incentives received, initial direct costs and an estimate of
   restoration costs (paragraphs 23 and 24).
4. After commencement the liability is increased by interest on it and
   reduced by the payments made (paragraph 36).
5. The right-of-use asset is carried at cost less accumulated
   depreciation and impairment (paragraphs 29 and 30) and depreciated
   from commencement to the earlier of the end of its useful life and
   the end of the lease term, unless ownership transfers (paragraphs 31
   and 32).
6. A lessee may elect not to apply this to short-term leases and leases
   of low-value assets, expensing their payments on a straight line
   (paragraphs 5 and 6).

How the slices use them, and no further:

- The **person** decides that a contract is a lease, its term and its
  discount rate, and enters the payments (T88a). The system judges none
  of it and applies no exemption of statement 6: a contract entered is
  capitalized.
- The **system** does the arithmetic: the present value of statement 2
  (T88b); the right-of-use asset at the liability's initial amount, the
  other parts of statement 3 being left to "later"; the interest of
  statement 4 and the straight-line depreciation over the lease term of
  statement 5 (T88b's schedule, T88c's run).
- The **postings** are the plan's reading of those statements, not a
  quotation: at activation the right-of-use asset against the lease
  liability; in each run interest expense against the liability and
  depreciation expense against accumulated depreciation; the lessor's
  invoice, posted by a person, against the liability.

---

## Open leaves left for after 1 December

Not split here; each stays as it is in the tracker. The codes are the
table's at the top.

**PP, post-pilot by the product owner's triage of 2026-10-21 (30).** Each
leaf's reference says so. The breadth decision did not name them, but
week 2 split one such leaf (scheduling agreements), so the triage alone
does not settle it: by default they wait, and question 1 lists the nine
that are capabilities rather than refinements of something built. Finance: capture of subsequent debits
and credits; cash discounts granted to customers; budget availability
check. Sourcing and procurement: the lock order of approving and closing
an order; GR/IR regrouping; `erp invoices` settling every invoice; email
inbox intake; supplier self-service portal; supplier risk screening
agent. Sales and service: governance for customers; "CRM, payroll and T&E
connectors (later phases)". Master data: cleansing proposals with owners;
continuous data-quality monitoring. GRC: periodic access reviews.
Platform: escalation and substitution rules (substitution); purchase
order output (X12 850); form templates. Non-functional: load tests at
production volumes; caching; lock-contention relief. Agentic delivery:
Git-backed repository; CUE constraints; release strategies (T16FS);
number ranges (NRIV); replay of payments and requisitions; ticketing for
the support agent. Migration: change data capture. Quality engineering:
the three contract and index steps.

**W, waits on someone outside the lanes (11).** Evaluation against the
live Claude API for capture, the analytics reports, and the
configuration, migration, test and analytics agents (six: the key and
its spend, deferred by the product owner on 2026-11-04); the design
review sign-off (the product owner's review); the external penetration
test (an outside firm); OData V2 extraction (the pilot's SAP system); the
pilot company code's cutover and the consultant hours (a client).

**PO12, deferred past 1 December by the product owner on 2026-11-12
(10).** Rebate agreements and settlement runs; EDI 850 inbound; the CRM
connector; service orders and field service dispatch; subscriptions and
usage rating; cXML order and invoice exchange (supplier network); the
contingent workforce connector.

**DET, not a placeholder any more: its work is held by detailed leaves or
is built, and the leaf is closed by hand as 07 §7.5 says (9).**
- Forecast agent with variance explanations (T53i: `internal/cashagent`),
  accrual proposals by agent and variance commentary by agent (T64e–T64j:
  `internal/closeagent`): built; the coordinator closes them.
- Eliminations (T68f, T68h), tax returns (T61g–T61k) and "Cost center
  master and hierarchy; then lift…" (T56a, T56b, T56k): their open work
  is in their steps' own leaves (T68h's seven, the returns' sign-offs,
  T56k).
- Governed changes (Customer master): a customer is created and changed
  through the partner change and its approval since T74a.
- Platform › Regulatory agent: the same capability as Agentic delivery ›
  Regulatory agent, split here (T107a, T107b); closed by hand when they
  are done.
- Compliance › Data retention policies: the same capability as Master
  data › Retention rules, split here (T99a); closed by hand with it.

**SAP, needs facts about SAP that are not in the repository, or the
pilot's system (6).** Discovery agent: usage analysis (ST03N/STAD) and
change-log mining (CDHDR/CDPOS). Custom-code agent: Z-object inventory
(TADIR, abapGit), unused-code retirement, mapping to configuration or Go
extensions. Integration agent: interface inventory (WE20, SM59). Their
table structures and export forms would have to be stated from memory;
07 §7.6 has T69 wait for SAP data access at the pilot. See the SAP
specialist's question and product owner question 1.

**POD, held by an earlier product owner decision (8).** Tax engine
integration (no provider now, ADR-0049 P1); Peppol network connector (no
provider appointed, 2026-11-09); country formats (one more format when a
country pack needs it, ADR-0061); SAF-T and statutory reports (the FTA
Audit File waits for its specification, Canada for Q4; ADR-0061 §10);
PDF export (no PDF for the pilot, 2026-11-04). And three refinements of
built behaviour, held by "features first, polishing to the backlog"
(2026-10-31, restated 2026-11-10): moving open direct-valued orders onto
GR/IR (optional, T72f); the override rate's counting of payment runs
(T54); the removal of `payments.release_approvals` (a contract step, once
no stored document needs it).

**OUT, depends on an area outside the required scope (5).** Budget
control (industry pack); cost estimates and actual costing (product
costing needs materials, bills of material and stock valuation:
Manufacturing and Supply chain); inventory metrics and HR metrics ("as
that module lands").

**WEB, the web workspace, deferred since ADR-0053 (3).** Dashboards in
the web workspace; server-rendered web workspaces; the web UI for pilot
finance users. Headless first; their built parts keep passing.

**OTH, left by this plan's own default, for the product owner to overrule
(5; question 1).**
- Expense system connector: a connector to a named outside system whose
  interface is not known here; T87a's API is what any connector calls.
- Go extensions generated by the custom-code agent: 07 §7.6 (T70) allows
  no tenant code before an ADR on a sandbox, a security posture decision.
- Operations agent (health monitoring, incident triage, continuous
  releases): it acts on a deployed cell's Prometheus, Alertmanager and
  Argo CD, and AWS is deferred to the end (product owner, 2026-11-02).

Also untouched: the 30 "later" leaves of weeks 1 and 2, and the 133 open
leaves of detailed steps (review findings, sign-offs, operator tasks,
T81c and T81d).

---

## Defaults chosen (for the product owner's digest)

No design document or ADR settles these; each is the simplest behaviour
that makes the main path work, and the slice records it in its ADR.

1. A returns order is for a posted billing document's items, up to the
   billed quantity; a return is confirmed as received by a person, in the
   sales context: no stock, no posting.
2. A credit memo request credits at the billing document's prices: for
   returned goods the billed amount of the quantity received, in full.
   It is approved by a permission of its own, without tiers.
3. A posted credit memo request is one customer credit memo in
   receivables naming the billing document's invoice; its tax and its
   limit against the invoice are T75d's, as built.
4. An invoice with an open dispute case is left out of dunning whole,
   whatever part is disputed; it still counts in the customer's credit
   exposure and in the aging. Closing a case posts nothing.
5. An expense report is entered and posted by an accountant, in the
   company's currency, gross, each line's tax code chosen by the person;
   the employee is a business partner with the role supplier and is paid
   by the payment run.
6. A lease's period rate is its annual rate divided by the payments a
   year. A payment due on the commencement date is not part of the
   measured lease liability; it is added to the right-of-use asset and
   bears no interest. Interest is on the balance outstanding during the
   period. Amounts are rounded half away from zero and the last period
   takes the rounding remainder only.
7. A lease posts through the ledger with five accounts from the account
   determination; the right-of-use asset is not an asset record; the run
   posts interest and depreciation and never the payment.
8. Profitability is net revenue by customer and item key from posted
   billing and credit memos; the margin report takes revenue and the
   named cost of sales accounts from the ledger by profit center.
9. A workbook is written with the standard library; no third-party
   module is added.
10. A scheduled report runs as its owner; a kept run is read only by
    viewers whose access covers its companies; delivery is a kept run,
    not an email.
11. The control library, the control matrix and the risk register ship
    empty; framework references are the tenant's free text; scores are
    likelihood × impact on scales of 1 to 5.
12. Retention rules ship empty. Archiving writes a file and deletes
    nothing. A deletion run blanks a blocked partner's master data, touches
    no audit event and removes no row; it finds nothing without a rule
    the tenant set.
13. Process maps, the fit-gap report and forecasts are computed, never
    written by a model. An agent gives no score and no saving in money:
    it recommends, with the API's figures.
14. Every new agent recommends through `createRecommendation`; accepting
    a recommendation changes nothing by itself. Agents are started by a
    person; none runs on a schedule.
15. The regulatory agent reads a notice a person gives it; it fetches
    nothing.
16. The MCP server runs over standard input and output with the caller's
    own token; it opens no port.
17. Order capture creates a draft order at the printed price; a person
    confirms.

## Questions for the product owner

Only new scope, money or tax rules, legal or regulatory facts and security
posture; none blocks a slice. Each gives what the product does by default
and the recommended answer.

1. **Placeholders left without a decision of yours (new scope).** Eleven
   in-scope placeholders are left though no decision defers them: the six
   that need SAP facts (usage analysis, change-log mining, Z-object
   inventory, unused-code retirement, mapping to extensions, interface
   inventory), the operations agent's three, the expense system connector
   and Go extensions. Product costing's two and the three "outside area"
   leaves are left as outside the scope. Nine more are capabilities your
   triage of 2026-10-21 marked post-pilot, which the breadth decision
   neither named nor excluded: the supplier self-service portal, the
   supplier risk screening agent, periodic access reviews, cash discounts
   granted to customers, the budget availability check, governance for
   customers, continuous data-quality monitoring (with cleansing
   proposals), form templates, and substitution rules. *Default: all wait
   until after 1 December. Recommended: confirm; if any is wanted before,
   it is split as week 2's scheduling agreements were, and the first to
   take would be periodic access reviews, which T106c prepares.*
2. **Archiving and deletion (security posture, legal).** Archiving
   deletes nothing from the database; a deletion run blanks master data
   of blocked partners, keeps every ledger document and touches no audit
   event, and is irreversible.
   *Default: built as described, behind a retention rule the tenant must
   set. Recommended: confirm, and say whether deletion may be built
   before a privacy lawyer has read ADR-0079; if not, T100c waits and
   T100a and T100b are built.*
3. **Retention periods (legal fact).** No period is shipped for Canada or
   the UAE; the tenant enters its own. *Default: none shipped.
   Recommended: confirm; periods join the country packs only with the
   legal source and a sign-off, after 1 December.*
4. **Credit memo for a return (money rule).** Returned goods are credited
   at the billed amount in full, with no restocking charge, and the
   request is approved by one permission with no amount tier.
   *Recommended: confirm.*
5. **Who approves a credit memo request (security posture).** No rule
   sets the approver apart from the creator, as for the credit block
   release (2026-11-12). *Recommended: confirm for the pilot.*
6. **Employees as business partners (scope, privacy).** An expense
   report's employee is a business partner with the role supplier, so
   employees' names and bank details enter the partner master while Human
   capital is outside the scope. *Default: as described; T100a's
   inventory lists them. Recommended: confirm.*
7. **What agents may read (security posture).** The access, control
   monitoring, risk and support agents read identity data (roles,
   assignments, SoD rules) and the audit trail; the contract already
   opens these reads to an agent holding `identity.access.read` and
   `audit.event.read`. *Default: each agent's role is granted them in the
   tenant by an administrator; nothing is granted by default.
   Recommended: confirm.*
8. **Two specifications the lanes cannot fetch (process).** T102a needs
   the Model Context Protocol specification's schema and T95d the
   workbook format's relevant part as files in the repository or the
   scratchpad. *Default: the slices wait for the files and are the last
   of their lanes (T95d, T102a). Recommended: the coordinator supplies them; no
   new module is added for either.*
9. **Disputed invoices (money rule).** A disputed invoice is not dunned
   at all, even when only part is disputed, and still counts in the
   credit exposure. *Recommended: confirm.*
10. **Lease accounting on unconfirmed statements (accounting).** T88a to
    T88c are built on six IFRS 16 statements from memory, each marked
    "not yet confirmed" in ADR-0073, as revenue recognition was
    (2026-11-12). *Default: built now as planning defaults. Recommended:
    confirm; the specialist's answers may overrule them.*

## Questions for the accounting specialist

1. **IFRS 16 paragraphs**: confirm the six statements of "Lease
   accounting: what the standard is taken to say" against the standard's
   text, paragraph by paragraph; they are from memory.
2. **The period's discount rate**: the first version divides the annual
   rate by the payments a year (default 6). Should it be the effective
   rate, (1 + annual rate) to the power of the period's share of a year,
   less 1? And confirm that a payment on the commencement date is
   counted in the right-of-use asset and not in the measured liability
   (statements 2 and 3).
3. **The right-of-use asset and asset accounting**: is it to appear in
   the asset history sheet and its depreciation in the depreciation run,
   or is a separate account and schedule acceptable for the pilot?
4. **Returns and revenue recognition**: a posted credit memo reduces
   what was billed, and the recognition run (T81d) reads billing. How is
   a return to enter a revenue contract (a right of return, a refund
   liability; IFRS 15's application guidance on sales with a right of
   return, from memory paragraphs B20 to B27, to confirm)? Until answered
   the run does not read credit memos.
5. **Short-term and low-value leases**: the first version capitalizes
   every contract entered. Does the pilot use the exemptions, and if so
   by what thresholds?

## Questions for the SAP specialist

1. For the six leaves left as needing SAP facts: which tables, fields and
   export forms carry workload statistics (ST03N/STAD), change documents
   (CDHDR/CDPOS), the repository's custom objects (TADIR) and the
   interface partners and destinations (WE20, SM59), and can the pilot's
   system export them? Nothing about them is assumed here.
