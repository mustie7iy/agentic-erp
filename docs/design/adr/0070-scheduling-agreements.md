# ADR-0070: Scheduling agreements

**Status:** accepted (T83a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T83a's last commit). It decides
the slices T83a and T83b (`docs/design/plan-breadth-week2.md`, Lane C).
Each slice records what it built under "As built"; both are there now.
The defaults taken are listed at the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Sourcing and procurement › Contracts and scheduling
agreements has the placeholder Scheduling agreements.
`plan-breadth-week2.md` splits it into T83a (a scheduling agreement with
its delivery schedule) and T83b (a schedule line called off), takes as
its default 17 that a scheduling agreement is a contract of its own kind
with schedule lines, a line being called off as a purchase order against
the agreement, and asks T83a to write this ADR.

What the design already settles:

- A contract (03 §3.8, `internal/procurement/contracts.go`) belongs to a
  legal entity and a supplier, has a currency, a validity, a target value
  and items priced per material key; it is a draft, released by a person
  to `active`, and then terminated or expired; it is renewed by a draft
  successor (T47g). Its items change only while it is a draft.
- A purchase order names a contract and, per item, a contract item, whose
  price it takes (`applyContractPrice`); the contract must be active, of
  the order's legal entity, supplier and currency, and valid on the order
  date (`usableContract`). The order's release consumes the contract's
  target value (`consumeContract`), refused when the order exceeds what
  remains (`procurement.contract_exceeded`).
- Receipts, GR/IR and invoices work on purchase orders.
- ADR-0003 (RLS), ADR-0005 (money in minor units), ADR-0007 (outbox),
  ADR-0008 (row locks and state guards), ADR-0011 (audit), ADR-0053
  (slices, headless first). An item's value is quantity × unit price ÷
  price unit (`money.Scale`). Events name a business partner by id and
  number, never by name (product owner, 2026-11-05).

Nothing settles what a scheduling agreement holds beyond a contract, or
how a delivery is asked for under it. This ADR decides both at the level
the two slices need.

## Decision

### 1. A scheduling agreement is a contract of its own kind

A contract has a **kind**: `contract` (the default, and what every
contract was before) or `scheduling_agreement`. The kind is given when the
contract is created and never changes. A scheduling agreement is created,
changed while a draft, released, cancelled, terminated, renewed and
expired exactly as a contract is, by the same operations, permissions
(`procurement.contract.manage`, `.read`), number range (`contract`),
events and audit records; its items are priced as a contract's. A
renewal has the kind of the contract it renews, and starts with an empty
schedule. No new table holds agreements and no new context is added.

### 2. Only the kind differs: the schedule

What a scheduling agreement has and a contract has not is a **delivery
schedule**: schedule lines. A contract of the kind `contract` takes none
(409). Nothing else differs in the first version; in particular a
purchase order created directly against a scheduling agreement
(`contract_id` on the order) is not refused: it is priced and consumes
the agreement's target value as it would a contract's.

### 3. Schedule lines

A schedule line asks for a **quantity** of one of the agreement's
**items**, due on a **delivery date**. It is entered once the agreement
is `active`; a draft, cancelled, terminated or expired one takes none.
The quantity is positive, a plain decimal as an order's
(`numeric(20,6)`), in the agreement item's unit. The delivery date lies
within the agreement's validity; it may be in the past (a schedule
agreed by other means is recorded as agreed). Lines are numbered 1 to n
per agreement in the order entered and are listed by delivery date. A
line's **value** is quantity × the item's unit price ÷ price unit,
rounded half away from zero (`money.Scale`); it is computed on read and
not stored, since an active contract's items do not change. What a line
asks for is not changed and a line is not deleted in the first version;
it is called off once (§4).

Entering a line reads the contract under its row lock (ADR-0008), so the
lines of one agreement are entered, and later called off, one after
another and wait for a release, termination or expiry of the contract in
progress. It leaves the contract itself unchanged (its version stays).

### 4. A line is called off as a purchase order (T83b)

Calling off a schedule line creates a **draft purchase order** through
the existing order path (`createOrder`: its checks, validation rules,
number and audit record): of the agreement's legal entity, supplier and
currency, dated the company's date today in its time zone, with the
agreement as its contract and one item, the agreement's item at the
agreement's price in the line's quantity, due on the line's delivery
date. The line then names the order. From there the order is any order:
it is released (the release strategy and approval rules apply, and the
release consumes the agreement's target value, refused when the order
exceeds what remains), received and invoiced unchanged. There is no
second ordering path, and receipts, GR/IR and invoices know nothing of
schedule lines.

A line is called off **once**: the contract and the line are read under
their row locks, so of two call-offs one creates the order and the other
finds the line called off and is refused (409, naming the order), also
when both carry the version they read: a called-off line is refused as
called off before its version is compared. The same request with its
`Idempotency-Key` again answers the same line and order. The database
refuses a second call-off of a line whatever the writer. The agreement must be active and valid
on the company's date today. The caller needs `procurement.order.create`,
as for creating the order directly.

### 5. Events and audit

Every change of a schedule line is audited
(`procurement.schedule_line.create`, `.call_off`) and published in the
same transaction through the procurement outbox: `ScheduleLineChanged`
with the line as the change left it (`change`: `created`, `called_off`)
and, for a call-off, `ScheduleLineCalledOff` naming the order. Both name
the supplier by id and number and the company by id and code. The
contract's own events are unchanged. None is offered to webhooks in the
first version.

## Alternatives

- **A table of its own for scheduling agreements.** Rejected: an
  agreement has a contract's header, items, validity, target value,
  lifecycle, renewal and expiry; a second table would copy all of them
  and need a second path into the purchase order's contract checks.
- **Goods received against the schedule line itself**, without an order
  (SAP's scheduling agreement, where the receipt names the agreement
  item). Left for later (the plan's "later" leaf): it would change
  receipts, GR/IR and invoice matching, which the first version leaves
  untouched by calling a line off as an order.
- **One order per several lines, or a job that calls lines off as their
  dates near.** Later; one line gives one order, called off by a person.

## Consequences

- A called-off line's order is an order like any other: it can be
  changed while a draft, or cancelled. The line stays called off and
  names that order; it is not reopened, and what is still wanted is
  entered as a new line (changing a line is the "later" leaf's).
- The agreement's consumed value rises when the order is released, not
  when the line is called off, as for any contract.
- A call-off order's item has no tax code or cost center: the buyer adds
  what the order's release asks for on the draft order, as on any order
  created without them.
- Schedule lines hold no cumulative quantities, tolerances or firm zones
  and are not sent to the supplier; these are the "later" leaf's.

## Defaults chosen (for the product owner's digest)

1. A scheduling agreement is numbered from the company's `contract`
   range and shares the contract's permissions and events; only `kind`
   tells it from a contract.
2. A contract keeps its kind; a renewal has the kind of the contract it
   renews and an empty schedule.
3. Schedule lines are entered only while the agreement is active, on
   dates within its validity; a date before today is not refused.
4. A line's quantity is given in the agreement item's unit; another unit
   is refused, as an order item on a contract item is.
5. A line's value is computed on read and never stored.
6. Entering a line does not change the contract's version.
7. A purchase order created directly against a scheduling agreement is
   not refused in the first version.
8. `createScheduleLine` is an agent tool like the contract's other
   operations (no `x-agent: false`, no `x-autonomy`: it changes the
   schedule, so an agent needs autonomy level 2).
9. `callOffScheduleLine` is an agent tool too, as `createPurchaseOrder`
   is, but not marked `x-autonomy: propose`: beside creating a draft it
   reserves the line, so an agent needs autonomy level 2, as it does to
   order a requisition (ADR-0036). The order's release is where a person
   or the release strategy's approval commits the company.
10. A call-off takes `If-Match` with the line's version, as every custom
    method on a resource does (04 §4.7), beside the state guard.
    The method is spelled `:callOff`: the contract's rule for a custom
    verb is camel case (`:signOff`, `:acceptProposal`), where the plan
    wrote `:call-off`.
11. The call-off order is dated the company's date today, takes the
    supplier's payment terms and carries no tax code, cost center or
    purchasing organization; the buyer adds them on the draft.
12. An agreement not valid on the company's date today refuses the
    call-off with 409 `procurement.agreement_not_valid`, checked before
    the order is created, so the refusal names the agreement's validity
    and not a field of an order the caller did not send. The delivery
    date itself may lie after today, and after a call-off made late.
13. A line's `status` is derived: `called_off` once it names its order.
    What became of the order is the order's own status.

## As built

### T83a Scheduling agreement with its delivery schedule (migration 000301)

- **Schema.** `procurement.contract.kind` (`contract`, the default, or
  `scheduling_agreement`; trigger `contract_kind_kept`: a contract keeps
  its kind). `procurement.schedule_line` (its contract, `line_no` unique
  per contract, the contract item it asks for by a foreign key to
  `contract_item`, delivery date, quantity above zero, version), with RLS
  and `tenant_id` first. Trigger `schedule_line_checked`, which reads the
  contract `FOR SHARE`: only a scheduling agreement
  (`schedule_line_of_agreement`) that is active
  (`schedule_line_of_active_agreement`) takes a line, for a live item
  (`schedule_line_of_live_item`), on a date within its validity
  (`schedule_line_within_validity`), numbered after its last
  (`schedule_line_numbered_in_order`). A line is not updated or deleted.
  Schema tests in `db/schedule_line_test.go`.
- **API.** `kind` on `POST /v1/contracts` (422 `procurement.invalid_kind`
  for another), on the contract's representation and as a filter of
  `GET /v1/contracts`. `POST /v1/contracts/{id}/schedule-lines`
  (`procurement.contract.manage`, idempotent by its key): 409
  `procurement.not_a_scheduling_agreement` for a contract of the kind
  `contract`, 409 `procurement.invalid_status` unless active, 422
  `procurement.invalid_contract_item` naming `contract_item_no`, 422
  `procurement.invalid_quantity`, 422
  `procurement.schedule_line_outside_validity` naming `delivery_date`,
  422 `procurement.amount_too_large`. `GET
  /v1/contracts/{id}/schedule-lines` (`procurement.contract.read`;
  filters `contract_item_no` and `line_no`; cursor paging by
  `delivery_date`). A line is shown with the agreement's number, the
  item's material key and description, its quantity with the unit, the
  item's price and the line's value, each amount with its currency.
- **Code.** `internal/procurement/schedule.go` and `schedule_api.go`;
  `contracts.go` (the kind read, written and validated), `renewals.go`
  (the renewal's kind), `api.go` (the kind in the contract's
  representation, its creation and its list).
- **Events.** `ScheduleLineChanged` (`created`) in `api/asyncapi.yaml`,
  with the company's code and the supplier's id and number.
- **Walkthrough.** `step_scheduling_agreement` (step 69): a scheduling
  agreement with one item is created and released, a line refused on a
  contract of the kind `contract` and on a date outside the validity, and
  two lines entered for the coming two months.

### T83b Schedule line called off (migration 000302)

- **Schema.** `procurement.schedule_line.purchase_order_id` (a foreign
  key to `purchase_order`; one order calls off one line,
  `schedule_line_order_once`). Trigger `schedule_line_call_off`: a line
  is entered not called off (`schedule_line_starts_scheduled`), is called
  off once (`schedule_line_called_off_once`) and to a purchase order
  against its own scheduling agreement
  (`schedule_line_order_of_agreement`). The application role may update
  `purchase_order_id`, `version` and `updated_by` alone. Schema tests in
  `db/schedule_line_call_off_test.go`.
- **API.** `POST /v1/contracts/{id}/schedule-lines/{line_no}:callOff`
  (`procurement.order.create`, `If-Match` with the line's version,
  idempotent by its key) answers the line with `status` `called_off`,
  `purchase_order_id` and `purchase_order_document_number`. 409
  `procurement.schedule_line_called_off` for a line already called off,
  naming its order; 409 `procurement.not_a_scheduling_agreement`, 409
  `procurement.invalid_status` unless the agreement is active, 409
  `procurement.agreement_not_valid` unless it is valid on the company's
  date today; 404 for a line number it lacks; what the order's own
  checks refuse as `POST /v1/purchase-orders` refuses it. The list
  filters by `status` (`scheduled`, `called_off`).
- **Code.** `internal/procurement/schedule_calloff.go` (`callOff`, which
  calls `createOrder` with the agreement as the order's contract, the
  item's `ContractItemNo` and `DeliveryDate`) and `schedule_api.go`.
  `purchase_orders.go`, receipts, GR/IR and invoices are unchanged: the
  order's release checks and consumes the agreement through
  `coveringContract` and `consumeContract` as for any contract.
- **Events.** `ScheduleLineCalledOff` and `ScheduleLineChanged`
  (`called_off`, with `status` and the order) in `api/asyncapi.yaml`.
- **Walkthrough.** `step_scheduling_agreement` (step 69) extended: the
  first line is called off; the draft order has the agreement's price
  and the line's date and quantity; it is released, and the agreement's
  consumed value rises by the order's value.
