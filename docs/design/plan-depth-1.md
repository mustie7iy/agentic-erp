# Depth plan, first variant slices (from 17 November 2026)

**Status:** plan (product owner, 2026-11-17: breadth is built; the
variants start, pilot-relevant first). Each lane splits the two variants
its brief names out of a "later: …" leaf of `docs/tracker.json` into
slices, in the form of `plan-breadth-week3.md`, and narrows that leaf to
what remains. `docs/tracker.json` stays the only plan (07 §7.5); each new
leaf's `r` names its section below. What every slice reuses is
`plan-breadth-week3.md`'s list, unchanged.

## rfq

Sourcing, "later: RFQ variants" (`plan-breadth-week2.md` §T82b, Later:
an RFQ from a requisition) and "later: award variants" (§T82d, Later: an
award to a purchase order in place of a contract, a recorded reason when
the lowest quotation does not win); ADR-0069. SAP S/4HANA is the
reference: an RFQ is created with reference to a purchase requisition
(ME41 / Manage RFQs, "create with reference"), taking over its items,
quantities and delivery dates, and the requisition shows the RFQ as its
follow-on document; the winning quotation is turned into a purchase order
with reference to it (ME21N / "Create Purchase Order from Quotation"),
and the order names the requisition the quotation's items came from. A
"reason for award" is kept with the award decision (Manage Supplier
Quotations' award reason). Statements about SAP here are from memory and
fix nothing beyond the behaviour below.

### T82e RFQ created from a requisition

**Capability.** A buyer creates an RFQ from an approved requisition: its
items, quantities, units, accounts and need date become the RFQ's, the
RFQ names the requisition, and the requisition is in sourcing until an
award orders it.

- **Reuses:** T82a's `createRFQ` (company, due date, suppliers invited,
  number, audit, outbox) and `rfqItems`; the requisition read under its
  row lock as `orderedRequisition` reads it (`getRequisition`), and moved
  on by `setStatus`, procurement's own requisition code.
- **Adds:** `requisition_id` on `POST /v1/rfqs` (the items then come from
  the requisition and are not sent; the currency defaults to the
  requisition's); `requisition_id` and `requisition_document_number` on
  the RFQ, `delivery_date` and `requisition_item_no` on its items
  (`delivery_date` also accepted on an item a buyer sends); the filter
  `requisition_id` on `GET /v1/rfqs`; the requisition status `in_sourcing`
  with `rfq_id` and `rfq_document_number` on the requisition; the
  requisition in `RfqChanged` and `RfqPublished`; a migration.
- **Walkthrough:** `step_rfq_requisition`: Ben raises a requisition for
  two items, Priya approves it; Ben creates an RFQ from it with two
  suppliers invited: its items, quantities and need date are the
  requisition's, which reads `in_sourcing` naming the RFQ; a second RFQ
  from it and items sent beside it are refused; the RFQ is published.
- **Acceptance:** only an approved requisition of the RFQ's company is
  sourced (409 otherwise; 422 for another company or currency); items
  sent beside a requisition are refused (422 `items`); every live item of
  the requisition becomes an RFQ item, in its order, with its quantity,
  unit and account, its catalog key as material key (or the requisition's
  number and the item's), and the requisition's need date as delivery
  date; the requisition moves `approved` → `in_sourcing` once under its
  row lock, in the RFQ's transaction (a second RFQ from it is 409); while
  in sourcing it is neither ordered directly nor cancelled (409, the
  existing rules); the database holds an RFQ's requisition to one in
  sourcing, of its company and currency, and each RFQ item's requisition
  item to one of that requisition; audited and published.
- **Depends on:** T82a (in `main`).

### T82f RFQ awarded to a purchase order

**Capability.** The buyer awards an RFQ to one quotation as a draft
purchase order in place of a contract: the order is created with that
supplier at the quoted prices, names the requisition the RFQ came from
(which becomes ordered), and the RFQ names the order. An award to any
quotation but the lowest needs a recorded reason, for a contract as for
an order.

- **Reuses:** T82d's award (row lock, `awardable`, the outcomes of the
  quotations, the RFQ's state guard), `createOrder` (the purchase order's
  own code path: its checks, number, audit and the requisition moved to
  `ordered`), the order's own release afterwards.
- **Adds:** `award_to` (`contract`, the default, or `purchase_order`) and
  `reason` on `POST /v1/rfqs/{id}:award`, `valid_from` and `valid_to`
  then required for a contract only; `purchase_order_id`,
  `purchase_order_document_number`, `award_reason` on the RFQ; event
  `RfqAwardedToOrder` (the supplier by id and number, the order by id and
  number, the requisition, the reason); a migration.
- **Walkthrough:** `step_rfq_requisition` extended: quotations are
  recorded from both suppliers; an award to the dearer one without a
  reason is refused, and with one it is accepted as a purchase order
  carrying the quoted prices, the requisition's need date and the
  requisition, which reads `ordered`; the order is released.
- **Acceptance:** the award's rules of T82d hold (published, once, a
  quotation the comparison ranks; row lock and state guards; 409 on a
  second award); a quotation not ranked first needs a `reason` (422
  `procurement.award_reason_required` naming `reason`), which is kept on
  the RFQ and in the audit record and event, and one ranked first may
  carry one; the order's items are the RFQ's at the quoted prices and
  price units, with their delivery dates and, from a requisition, the
  requisition items' account assignments; the order's date is the
  company's date today; an RFQ from a requisition is awarded to a
  purchase order only (409 `procurement.rfq_from_requisition` for a
  contract); RFQ, quotations, order and requisition change in one
  transaction; the database holds an awarded RFQ to exactly one of a
  contract or an order of its company, currency and winning supplier.
- **Depends on:** T82d, T82e.

**Decided by default (rfq):** (1) a requisition is sourced as a whole, as
it is ordered as a whole here: its status `in_sourcing` marks all its
items, and each RFQ item names its requisition item
(`requisition_item_no`); (2) an RFQ from a requisition is created through
`POST /v1/rfqs` with `requisition_id`, not a custom method of the
requisition, since the RFQ is what is created; (3) an item without a
catalog key takes `<requisition number>/<item no>` as its material key,
as does every item sharing a catalog key with another; (4) the requisition
in sourcing is not cancelled or ordered directly until its RFQ is awarded
(closing an RFQ without an award, which would free it, stays later); (5)
an RFQ from a requisition is awarded to a purchase order only: a
contract would leave the requisition in sourcing with nothing to order
it; (6) the order is a draft dated the company's date today, with the
supplier's payment terms, released afterwards as any order (the release
strategy then judges it against its requisition); (7) the reason is
required whenever the quotation awarded is not ranked first (equal
totals share rank 1, so any of them wins without one), 1 to 500
characters; (8) an award to an order publishes `RfqAwardedToOrder` beside
`RfqChanged`, since `RfqAwarded` requires the contract's fields; (9) the
link is kept once, on the RFQ (`purchase_order_id`), as T82d keeps the
contract's; the order names its requisition as any order does.

**Later for these variants:** a requisition's items sourced in part or
spread over several RFQs; several requisitions in one RFQ; an award of a
requisition's RFQ to a contract; the purchase order's representation
naming the RFQ; the rest of the two placeholders' lists.
