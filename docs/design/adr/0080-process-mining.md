# ADR-0080: Process mining

**Status:** accepted (T103a, 2026-11-15; decided by default under ADR-0053 d,
accepted once `go-reviewer` had no blocking finding). It decides the
process mining context and its first slice, the purchase-to-pay event log
(`docs/design/plan-breadth-week3.md` §T103a); T103b (the process map)
records what it builds under "As built (T103b)".

## Context

A process analyst wants to see how purchase to pay actually runs: which
activities each order goes through, in which order, how long between them
and who (by kind) did them. Process mining tools read an event log: a case
id, an activity and a timestamp per event. Every state change in the
product already writes an audit event in its transaction (ADR-0011), and
the documents of purchase to pay already link to each other (a requisition
to its order, an order to its goods movements and invoices, an invoice's
journal entry to the payment run that cleared it). The log can therefore
be computed on read, without storing anything.

## Decision

1. **A leaf context, `processmining`, that stores nothing.** It reads
   tenancy's legal entities, procurement's orders (`procurement.ProcessLog`),
   payables' supplier documents (`payables.ProcessLog`), payments' released
   runs (`payments.ProcessLog`) and the audit trail (`audit.Trail`) through
   ports the composition root wires, by the types those contexts publish;
   no context imports it. It has no schema. Two indexes help the reads it
   asks of others (migrations 000340, 000341): orders by company, date and
   id; supplier documents on no order by company, date and id.
2. **Cases.** For purchase to pay a case is a purchase order dated in the
   period (order date from `from` to `to`, both included), or a supplier
   document (invoice, credit memo, subsequent debit or credit) on no order,
   by its header or any item, dated in the period by its invoice date: a
   case of its own. A case's documents are the order, the requisition it
   was made from, its goods movements (receipts, returns, reversals), the
   supplier documents on it (by header or item; a document on two orders
   is in both cases) and the released payment runs that paid those
   documents' posted entries; a document case's are the document and its
   runs. The case's events are every audit event of an activity about one
   of them, at any time up to the read, by time and then by the trail's
   sequence number.
3. **Activities and the audit actions they come from:**

   | Activity | Audit action | Object |
   |---|---|---|
   | Create requisition | `procurement.requisition.create` | requisition |
   | Submit requisition | `procurement.requisition.pending_approval` | requisition |
   | Approve requisition | `procurement.requisition.approved` | requisition |
   | Reject requisition | `procurement.requisition.rejected` | requisition |
   | Create purchase order | `procurement.purchase_order.create` | purchase order |
   | Submit purchase order | `procurement.purchase_order.submit` | purchase order |
   | Release purchase order | `procurement.purchase_order.release` | purchase order |
   | Close purchase order | `procurement.purchase_order.close` | purchase order |
   | Cancel purchase order | `procurement.purchase_order.cancel` | purchase order |
   | Receive goods | `procurement.goods_receipt.post` | goods movement |
   | Return goods | `procurement.goods_return.post` | goods movement |
   | Reverse goods receipt | `procurement.goods_receipt.reverse` | goods movement |
   | Enter invoice | `payables.supplier_invoice.create` | supplier document |
   | Post invoice | `payables.supplier_invoice.post` | supplier document |
   | Hold invoice | `payables.supplier_invoice.hold` | supplier document |
   | Release invoice | `payables.supplier_invoice.release` | supplier document |
   | Reverse invoice | `payables.supplier_invoice.reverse` | supplier document |
   | Pay invoice | `payments.payment_run.complete` | payment run |

   Any other action of those documents (an update, a match) is no activity.
4. **Actors by kind only.** An event names the kind of its actor:
   `person` (the trail's `human`), `agent`, or `system` (a service or the
   platform's own work); never the actor's id or name.
5. **The read.** `GET /v1/process-event-logs` (`process=purchase_to_pay`,
   `legal_entity_id`, `from`, `to`; permission `processmining.log.read`)
   answers one page of cases by date and id with their events, as JSON
   (the default) or CSV (a row per event, the next page's token in the
   `Next-Page-Token` header), and `case_count`, the cases of the whole
   log. The period is one day to 371 days, the longest fiscal
   year. A company the caller cannot act in is not found. Budget: 3 s at
   p95 for a page at 4,000 orders a month of one company (01 §1.3's 20
   million invoices a year over 500 companies), measured at about 0.2 s.

## Alternatives

- **An event log table written by every context.** Duplicates the audit
  trail and needs every context changed; the trail already holds the
  events in their transactions.
- **Reading SAP's document tables.** Later, with the SAP specialist.

## Consequences

- The log is only as complete as the trail: activities recorded before an
  action was audited, or by migration, do not appear.
- A read costs a few statements per page whatever the period's size; the
  case count is a count of the period's documents.

## Defaults chosen (for the product owner's digest)

- A case's period is its document's date; its events are all of its
  events up to the read, so a case dated in the period shows its payment
  made after it.
- Every supplier document on no order is a case of its own, whatever its
  kind; a credit memo on an invoice is not joined to the invoice's case.
- Payment is the completion of a released run (`payments.payment_run.complete`),
  when its payments are posted; proposals and bank files are no activity.
- The process is required and the first version knows only
  `purchase_to_pay`; the activity names are English labels, an open
  enumeration.

## As built (T103a)

- `internal/processmining` (`Endpoints.Read`, `GetProcessEventLog`),
  `procurement.ProcessLog`, `payables.ProcessLog`, `payments.ProcessLog`,
  `audit.Trail`; migrations 000339 (permission), 000340 and 000341
  (indexes); `TestEventLogBudget`; walkthrough `step_process_mining`.

## As built (T103b, the process map)

- `GET /v1/process-maps` (`getProcessMap`; `process`, `legal_entity_id`,
  `from`, `to`), `Endpoints.ReadMap`, `GetProcessMap`: the map of every
  case of the log of the company and period (§2, §5), the same cases,
  events and activities, read a thousand cases at a time with their events
  and summed; nothing is stored, no model is called, no migration.
- **Activities**: each with its events and the cases that have one or
  more, by event count, then by name.
- **Directly-follows pairs**: within a case, each event and the next, by
  the case's order (time, then sequence number); a pair's `count` is how
  many times its second activity directly follows its first, and
  `median_seconds` the median of the times between the two events, each
  cut to whole seconds, the lower middle for an even count (sorted, the
  `(n-1)/2`-th). Pairs by count, then by their activities.
- **Variants**: a case's variant is the sequence of its activities; every
  case is in exactly one, a case without events in the empty one. The ten
  most frequent are given, by case count, then by their activities (as
  text, in order), each with its share of the cases in percent, two
  decimals, rounded half away from zero, computed in integers;
  `variant_count` counts every variant, so the cases of those not given
  are `case_count` less theirs.
- **Open cases** (decided by default): a case is ended by its last
  activity: an order's by `Pay invoice`, `Close purchase order` or
  `Cancel purchase order`, a supplier document's on no order by `Pay
  invoice` or `Reverse invoice`; any other last activity, or none, leaves
  it open. Open cases are in the map like the others and counted,
  `open_case_count` on the map and on each variant (a count, not a flag,
  since an order's case and a document's could run the same activities).
  Process mining tools commonly mark a case complete by its end activity
  (stated from memory, not checked against a source); which activities
  end a purchase-to-pay case is this product's choice, listed for the
  product owner.
- **Permission** (decided by default): the event log's,
  `processmining.log.read`: the map is that log summed, and whoever may
  read the log can compute it. No new permission, so no migration.
- **Size**: the period is the log's, up to 371 days; a map reads every
  case of it, from one database state: the operation is `x-snapshot`
  (REPEATABLE READ), so a payment run completing between two chunks does
  not show some of its cases paid and others not, and `x-slo-class:
  report` (01 §1.3's row). `TestProcessMapBudget` (3 s at p95) measures a month of
  4,000 orders of one company, 01 §1.3's volume, at a median of 0.94 s and
  a p95 of 1.7 s on a loaded machine (load 13); the time grows with the
  cases, so a year of the largest company's 40,000 orders takes about
  twelve times as long, within the server's 60 s write timeout.
- Walkthrough `step_process_mining` (step 101) extended: Pia reads the
  month's map, which holds the variant of the order of step 4; Ben orders
  a month's office cleaning, which nobody receives, Alex's invoice on it is
  held for the missing receipt, Priya approves the exception, a payment run
  pays it, and the map then holds the service order's variant, without
  `Receive goods` and ended by `Pay invoice`, beside the first.
