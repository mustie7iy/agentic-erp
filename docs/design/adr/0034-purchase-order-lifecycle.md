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
then withdraws its request. An approval and a close of the same order at
the same moment therefore deadlock. PostgreSQL detects it and aborts one
(40P01, answered as a retryable 503 by the problem mapping); the other
completes, and the order, the change and the request agree either way
(`TestApproveAndCloseAtOnce`). This is accepted: the collision needs two
people acting on the same order within the deadlock timeout, and the
retry is safe. Taking the order's lock before the request's in the Decider
would remove it, at the cost of the approvals context knowing each object
type's row.

**Configuration keys are added before they are required.** The new keys
(`purchasing.order_release_approval`, `purchasing.contract_expiry_notice_days`)
are required by validation from their first release. That is safe during a
rolling upgrade only because the active document is read tolerantly by an
older replica (06 §6.4, `config.ParseActive`); a document activated before
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
- Approve-versus-close on the same order can answer 503 to one of the two
  people; their retry succeeds or is refused for the state the other left.
