# Depth plan 2: second variant slices (17 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants continue, pilot-relevant first). Each lane writes its own
`## <lane>` section in the form of `plan-breadth-week3.md` (Adds,
Acceptance, Later). `docs/tracker.json` stays the only plan (07 §7.5);
each leaf's `r` names its section below.

## rfq2

Sourcing, after `plan-depth-1.md` §T82e-§T82f: an RFQ that ends without
an award (`plan-breadth-week2.md` §T82b, Later; T82e's default 4 left it
later), and the two findings of the batch 78 review in the same code
(review:batch78): a cancelled award order left its requisition and RFQ
behind, and the purchase-to-pay event log did not show the requisition's
sourcing. ADR-0069. SAP S/4HANA is the reference: an RFQ that is no
longer needed is completed or deleted in Manage RFQs without a quotation
being awarded, and the requirement it came from is open again for
sourcing; a purchase order deleted after it was created from a quotation
opens its requisition again, and the quotation keeps its history.
Statements about SAP here are from memory and fix nothing beyond the
behaviour below.

### T82g RFQ cancelled or closed without an award

**Capability.** A buyer cancels an RFQ (a draft, or a published one no
longer needed) or closes a published RFQ without an award (the
quotations did not answer the need), with a reason. A requisition it was
created from leaves sourcing: it is approved again, available to a new
RFQ or an order, and its move is published as an event.

- **Reuses:** T82a's `getRFQ` under its row lock with If-Match and the
  status guard, `recorded` (audit, `RfqChanged`); T82d's quotation
  outcomes (`not_awarded`); the requisition read under its row lock
  (`getRequisition`) and moved by procurement's own requisition code
  (`setStatus`).
- **Adds:** `POST /v1/rfqs/{id}:cancel` and `POST /v1/rfqs/{id}:close`,
  each with a required `reason`; RFQ statuses `cancelled` and `closed` and
  `closing_reason` on the RFQ; `reason` in `RfqChanged`; event
  `RequisitionReopened` (the requisition approved again, with what
  reopened it); audit action `procurement.requisition.reopened`; a
  migration.
- **Walkthrough:** `step_rfq_requisition` extended: the RFQ from the
  requisition is published and one supplier quotes far above the
  estimate; Ben closes the RFQ without an award, with his reason: the RFQ
  reads `closed` with the reason, the quotation `not_awarded`, and the
  requisition `approved` again; closing it again is refused; Ben creates
  a new RFQ from the requisition, which goes on to the award as before.
- **Acceptance:** `:cancel` takes a `draft` or `published` RFQ and
  `:close` a `published` one (409 `procurement.invalid_status` otherwise:
  awarded, cancelled or closed, or a draft to close), at the version read
  (If-Match), once, under the RFQ's row lock; `reason` is required, 1 to
  500 characters (422 naming `reason`); the RFQ's recorded quotations all
  become `not_awarded`, and no quotation is recorded against it or award
  made after (409, the existing rules); the RFQ keeps `closing_reason`; a
  requisition it was created from moves `in_sourcing` → `approved` under
  its row lock in the same transaction (RFQ, then requisition, as the
  award takes them), audited as `procurement.requisition.reopened` and
  published as `RequisitionReopened` (`previous_status` `in_sourcing`,
  `cause` `rfq_cancelled` or `rfq_closed`, the RFQ by id and number), and
  can then be sourced by a new RFQ, ordered or cancelled; the RFQ's change
  is audited and published (`RfqChanged`, `cancelled` or `closed`, with
  the reason); the database holds a cancelled or closed RFQ with a
  reason, without an award, with no quotation still open, from the
  statuses above, and keeps it so; an RFQ from a requisition no longer
  counts as open once cancelled or closed.
- **Depends on:** T82e, T82f (in `main`).

### T82h An award's purchase order cancelled; the requisition's sourcing in the event log

**Capability.** When the buyer cancels a purchase order an RFQ's award
created, its requisition is approved again with an event, as for any
order from a requisition, and the RFQ shows that the order was cancelled
instead of naming it as its live result; the purchase-to-pay event log
shows a requisition's sourcing and its reopening (T103c).

- **Reuses:** `cancelOrder` (its checks: nothing received or invoiced);
  T82g's reopening of a requisition and its event; `recorded` for the
  RFQ's change; the event log's activities by audit action (ADR-0080 §3).
- **Adds:** the RFQ status `order_cancelled` (the award's order was
  cancelled; `purchase_order_id` still names it); `RequisitionReopened`
  with `cause` `purchase_order_cancelled` and the order; the activities
  "Source requisition" (`procurement.requisition.in_sourcing`) and
  "Reopen requisition" (`procurement.requisition.reopened`); a
  migration.
- **Walkthrough:** `step_rfq_requisition` extended: after the order's
  release, Tyrell cannot deliver after all and Ben cancels the order: the
  requisition reads `approved`, the RFQ `order_cancelled` naming the
  cancelled order; the order's case in the purchase-to-pay event log shows
  the requisition sourced and reopened.
- **Acceptance:** cancelling an order (any, from a requisition) moves
  its requisition `ordered` → `approved`, audited as
  `procurement.requisition.reopened` and published as
  `RequisitionReopened` (`cause` `purchase_order_cancelled`, the order by
  id and number); cancelling an order an award created moves its RFQ
  `awarded` → `order_cancelled` in the same transaction (order, then RFQ,
  then requisition: no path takes them the other way round), version
  bumped, audited and published (`RfqChanged`, `order_cancelled`), the
  quotations' outcomes and the award reason kept; the database lets an
  RFQ become `order_cancelled` only from `awarded`, with its order
  cancelled, and keeps it so; the event log gives "Source requisition"
  and "Reopen requisition" for the requisition's audit records of those
  actions.
- **Depends on:** T82g.

**Decided by default (rfq2):** (1) two methods, `:cancel` (a draft or a
published RFQ no longer needed) and `:close` (a published RFQ whose
quotations do not lead to an award), since a consumer branches on why
the sourcing ended; both take a reason, 1 to 500 characters, kept as
`closing_reason`; (2) the quotations recorded against a cancelled or
closed RFQ become `not_awarded`, T82d's outcome, so no quotation is left
open; (3) the requisition goes back to `approved` (not to a status of its
own): it is what it was before it was sourced, and its approval stands;
it still names the RFQ last created from it; (4) one event,
`RequisitionReopened`, for every move back to `approved` (an RFQ ended
or an order cancelled), with `cause`, offered to webhooks as the other
requisition events are; the RFQ's free-text reason is not in it (it is
in `RfqChanged`); (5) every order cancelled from a requisition publishes
it, not only an award's, since the defect is the same; its audit action
becomes `procurement.requisition.reopened` (it was `.approved`, which the
event log showed as a second approval); (6) an award's cancelled order
moves the RFQ to `order_cancelled` and keeps `purchase_order_id`, so the
history stays readable from the RFQ; the RFQ is not reopened, and the
requisition is sourced again by a new RFQ; (7) the event log's activities
stay per audit action; the RFQ's own records are not activities of the
order's case.

**Later for these variants:** reopening a cancelled or closed RFQ;
notifying the suppliers invited that the RFQ ended; an RFQ awarded to a
contract whose contract is cancelled; the RFQ's own activities in the
event log; the rest of the placeholders' lists.
