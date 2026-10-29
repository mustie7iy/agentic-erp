# ADR-0034: Purchase order lifecycle after release

**Status:** proposed

## Context
Until T47 a released purchase order could not change, could only be
released directly, and goods could only be received or the receipt
reversed. Real purchasing needs more, without weakening what payables and
the ledger rely on: the order price that GR/IR is valued at, the received
quantity invoices are matched against, the contract's consumption, and an
approval for anything that raises what the company has committed to. The
product owner decided the shape (T47, steps a to h):

- receipts are valued at the price fixed when an item is released;
- only a change that raises the order's total needs approval;
- agents may create, submit and release; approval decisions are people's;
- returns are allowed on closed orders, and their reversal (123) too;
- a rejected release goes back to draft;
- contract expiry alerts are events only.

## Decision

**The valuation price is fixed at release.** Each item of a released order
carries `valuation_price` and `valuation_price_unit`, set from its price as
it is released (with its order, or as a change adds it) and never changed
afterwards (database rule). Goods movements post GR/IR at it; a later price
reaches GR/IR only as the invoice's price difference. An item added back
under a removed item's number keeps the valuation price it was released at.

**Release strategy.** An order without an approved requisition, above its
requisition's total, or in another currency than it, is released by
`purchasing.order_release_approval` tiers on its total in the company's
currency: it waits in `pending_approval` for a `purchase_order` approval
request, whose approval releases it in the deciding transaction (checked
again: supplier, contract), and whose rejection or withdrawal returns it to
draft.

**A released order changes only through an order change.** A change is a
version of the order: its header fields (order date, payment terms,
extensions) and complete items, based on the order's `revision`, with the
totals before and after. One change is open per order. It is checked
against what a released order keeps (identity of the order and of each
item, never below what was received, no removal of an item with receipts)
when drafted, patched and submitted, and the database enforces the same
when it is applied. A change that does not raise the total is applied on
submission; one that raises it waits for an `order_change` request on the
release tiers by its new total. It is applied in one transaction, in the
order the triggers require (the order names the change, the items are
written, the change records what the order read and the revision it makes,
the order moves to that revision), consuming the difference of its total
from a contract and publishing `PurchaseOrderChanged`. Closing or
cancelling the order cancels its open change and withdraws its request.

**Returns** are movement types 122 (with a reason, bounded by what was
received) and 123 (its reversal), each moving the received quantity its
own way (database rules). On a GR/IR order a return posts at the
valuation price; an invoiced item's remaining GR/IR debit is what the
supplier's credit memo (T34) clears.

**Contracts are renewed and alerted.** `contracts/{id}:renew` drafts a
successor from the day after the contract ends, with its legal entity,
supplier, currency and items; one live renewal at a time. The contract
expirer alerts an active contract that no live renewal continues and that
ends within `purchasing.contract_expiry_notice_days`, once, by
`ContractExpiring`; the alert's update checks again that no renewal
committed meanwhile.

**Lock order, and the deadlock it leaves.** Every write of an order change
locks the order, then the change; the expiry alert locks the contract.
The approval Decider locks the request first and then calls the handler,
which locks the order; closing or cancelling an order locks the order and
then withdraws its request (its release request while it waits in
`pending_approval`, its open change's request once released). An approval,
of a release or of a change, and a close or cancel of the same order at
the same moment therefore deadlock. PostgreSQL detects it and aborts one
(40P01, answered as a retryable 503 by the problem mapping); the other
completes, and the order, the change and the request agree either way
(`TestApproveAndCloseAtOnce` shows it for a change's approval and a
close). This is accepted: the collision needs two people acting on the
same order within the deadlock timeout, and the retry is safe. Taking
the order's lock before the request's in the Decider would remove it, at
the cost of the approvals context knowing each object type's row.

**Account assignment (T56e, ADR-0050 §6).** An order item carries its
`account_assignment` (`none` or `cost_center`) and cost center. They are
checked against the ledger when the order is released, on its order date,
before approval is asked and again as the release is approved, and the
items an order change adds as it is submitted and approved; an order a
migration run loads is released unchecked. A released item keeps its
assignment as it keeps its account (`purchase_order_item_identity`, 422
`procurement.invalid_change`), so its receipts, their reversals, returns
and the invoice's price difference post to one cost center: the same
reasoning as the valuation price, which must not let a later change split
what one item posted.

**Configuration keys are added before they are required.** The new keys
(`purchasing.order_release_approval`, `purchasing.contract_expiry_notice_days`)
are required by validation from their first release. That is safe during a
rolling upgrade only because the active document is read tolerantly by an
older replica (06 §6.4, `config.ParseActive`, from the fix-config-compat
change, which must reach main before this one); a document activated before
a key existed reads its zero value, which means "no alerts" for the expiry
notice and "refused, `workflow.no_policy`" for the release tiers.

## Alternatives
- **Revalue GR/IR at the changed price** (no valuation price). Simpler
  rows, but an open GR/IR position would change value without a posting,
  and receipts already posted would disagree with the account. Rejected.
- **Edit released orders in place, with change documents** (SAP's
  CDHDR/CDPOS). Every writer would need the checks, and an approval of a
  change could not hold the content approved; the change row is the
  content, and the database lets the order change only as it is applied.
- **Approve every change.** Decreases and neutral changes do not raise the
  commitment; asking for approval would train approvers to click through.
- **Alert contracts by a separate job.** It would read the same rows on
  its own schedule; one round per tenant that expires and alerts keeps the
  work in one transaction.

## Consequences
- GR/IR stays valued at what was released; price differences are the
  invoice's (payables), never a revaluation.
- An order has versions: `revision` and its applied changes (with what the
  order read before each) are its history.
- Contract consumption is the sum of releases, change deltas (negative for
  a decrease) and cancellations; it is never updated in place.
- An approval (of a release or a change) at the same moment as a close or
  cancel of the same order can answer 503 to one of the two people; their
  retry succeeds or is refused for the state the other left.

## As built (T72e): GR/IR clearing of closed orders (SAP MR11)

A closed order takes no further receipts or invoices, so a GR/IR position
it is left with stays open until it is cleared (03-data-model §3.7, §3.8;
migration 000165). Decisions taken by default (ADR-0053 §d), each settled
by an existing rule:

- **What a clearing posts against.** The legal entity's `price_difference`
  account, with the order item's cost center when that is a primary cost
  account: the account the ledger already uses for what GR/IR leaves (an
  invoice's price difference; the residual a reversal leaves, T33b). SAP's
  documentation of GR/IR clearing account maintenance posts the offsetting
  entry to a price difference account for a material at standard price and
  to the stock account for one at moving average price with stock to cover
  it ([SAP Help, GR/IR Clearing Account Maintenance](https://help.sap.com/saphelp_46C/helpdata/EN/47/6100f249f011d1894c0000e829fbbd/content.htm));
  this system values no stock, so the price difference account is the one
  that applies. Account-assigned items in SAP are a specialist's question
  (below).
- **Which orders.** Closed orders valued through GR/IR, as the tracker's
  leaf and 03 §3.8 say ("a closed order's position waits for a GR/IR
  clearing"). An open order's position is still expected to clear by a
  receipt or an invoice.
- **Who owns it.** The procurement context, which owns the order, its status
  and its items' valuation prices and cost centers, and already posts GR/IR
  through `ledger.Postings`; the ledger keeps the positions and refuses a
  clearing whose position moved (`GRIRItem.Clears`, 409
  `ledger.gr_ir_clearing_outdated`), so what posts is what was approved. No
  new dependency between contexts.
- **Approval.** A `gr_ir_clearing` approval request decided by the fixed
  `controller` role, as period unlocks and center changes are
  (`approvals.DefaultGRIRClearingRole`): posting to P&L outside an invoice is
  a controller's decision, never the proposer's (`workflow.own_request`)
  and never an agent's (the permission is an approval permission). The
  request's amount is what the clearing posts to the price difference
  account.
- **The entry.** Document type WE, from the `journal_we` range every company
  with a GR/IR account already has (config validation), so no new number
  range is required of a configuration; source procurement, the clearing as
  its source document, so the ledger's `:reverse` refuses it.
- **Cleared quantities apart.** The quantity a clearing posts is neither
  received nor invoiced: the ledger records clearing entries
  (`ledger.gr_ir_clearing_entry`) and keeps `cleared` beside them, which the
  GR/IR balances report (`cleared_quantity`), so received and invoiced stay
  what the receipts and invoices say.
- **Idempotency.** The request's `Idempotency-Key` replays the answer; a new
  proposal while one is pending is refused (one pending per order), and one
  after the posting finds nothing to clear (409 `procurement.nothing_to_clear`).

Not built, for the product owner: reversing a posted clearing (no path
here yet), and clearing positions of orders that are fully invoiced but
not closed.

## As built: what GR/IR balances and approval requests refer to (review:T72e)

The API-consumer review of T72e (ADR-0053, REVIEW.md) found GR/IR balances
giving orders and accounts by id only and no totals, approval requests whose
object a consumer could not reach, and a change history that failed on page
2. Decided by default:

- **GR/IR balances name what they count.** Each position gives
  `purchase_order_number` beside `purchase_order_id`, read from the
  procurement context through a port the ledger declares
  (`ledger.OrderNumbers`, implemented by `procurement.Orders.Numbers`, one
  call per page), so the ledger still imports nothing of procurement; and
  `account_number` and `account_name` of the GR/IR account, the ledger's
  own. The number is absent only for postings naming an order procurement
  does not hold (none in practice; the ledger's own tests post such).
- **The report's totals.** `totals` sums every position the filters select,
  not only the page's, and is the same on every page: their count, the
  balances in local and group currency (the legal entity's, so present on
  an empty report too), and in the orders' currency only when every position
  is in one (always for one order). Absent when the legal entity is not one
  the caller may act in, as the positions are; that check is now made once
  on the legal entity (as the center balance reports make it) rather than
  in each query.
- **Approval requests lead to their object.** `object_resource` is the API
  path that reads the object, given by the context that owns the object
  type: an approvals `Locator`, an optional interface of the Handler beside
  `Barrer` and `Modifier`, asked once per object type per page. Objects read
  by their own id use `approvals.Paths`; an order change is read through its
  order (`/v1/purchase-orders/{id}/changes/{change_id}`), and a bank
  clearing's match through the statement whose line carries it. A Handler
  that is not a Locator leaves the path absent; every one the application
  wires is one (`TestApprovalObjectsLocated`). `object_type` and the list's
  filter now list `payment_run_release` and `gr_ir_clearing`.
- **A change history answers page 2.** `listPurchaseOrderChanges` joined the
  order, so the keyset's unqualified tie-breaker `id` was ambiguous on page
  2 (500). Its select is wrapped as the other joined listings are
  (`SELECT * FROM (…) ch WHERE true`). Every other keyset listing over a join
  was checked: none other was ambiguous, and each now has a page-2 test.
