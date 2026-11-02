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

## As built (T72f): open GR/IR positions migrated from SAP

A migration run loaded every order valued directly, so an invoice after
cutover for goods SAP received booked the expense a second time, while
SAP's GR/IR credit stayed on the account with no order item to clear it
(03-data-model §3.7, §3.17; migration 000167). Decisions taken by default
(ADR-0053 §d), each settled by an existing rule:

- **Which orders load valued through GR/IR.** Those a line on SAP's GR/IR
  account names (ACDOCA EBELN/EBELP), whether or not a position is still
  open: SAP valued them through GR/IR, and the platform values an order
  through GR/IR whole (this ADR, valuation fixed at release). Others load
  directly, as before. The legal entity needs its `gr_ir` account (422
  `procurement.no_gr_ir_account`). Moving an order an earlier run loaded
  directly onto GR/IR is the optional tracker leaf, not built.
- **Receipts are not posted again.** The migrated journal carries their
  value, so the load records them without a WE entry
  (`procurement.PostLoadedReceipt`); a reversal on the platform posts its
  entry as any does.
- **The take-over, an entry of its own** (the pattern of T72c's down
  payments, ADR-0037): one per order, document type WE (`journal_we`, which
  a GR/IR account requires), source integration, a `GRIRItem.TakesOver` per
  open item: the item's GR/IR line with SAP's balance (HSL) and open
  quantity, against a line on the GR/IR account without an order item that
  offsets the lines SAP's documents, or the opening entry, loaded without
  theirs. It nets to zero on the account, so the trial balance is SAP's, and
  the total debits of the reconciliation leave it out. The ledger takes a
  position over once (409 `ledger.gr_ir_position_exists`); a later run
  tops it up (below).
- **The open quantity** is what SAP's posted invoice documents invoiced on
  the item (credit memos negative; the quantities T72b2 takes over for
  matching) less what SAP received (MSEG 101), the figures the load already
  takes over for receipts and matching, rather than a quantity field of
  ACDOCA. It is kept apart, as a clearing's is: `migrated` beside received,
  invoiced and cleared, which the GR/IR balances report
  (`migrated_quantity`); received and invoiced stay what the platform posted.
- **What the gate refuses** (T72b's never-guess rule): a GR/IR line without
  its order item or naming one outside the extract; SAP's GR/IR account
  (T030 WRX) with lines when it is not the legal entity's `gr_ir`; an order
  in another currency than the company's (refused when T72f was built;
  since taken over in its currency, below); a movement other than a
  receipt, or a receipt quantity that does not parse, on an item with a
  position; an open quantity without a balance; and an item received
  without a GR/IR line on an order whose other items post one, which an
  invoice after cutover would clear a position it never had.
- **Reconciliation.** Open GR/IR per order item, balance and quantity, in
  total and per item, shown when either side has a position.
- **Several companies and OData extracts.** A GR/IR line naming an order of
  another company of the scope is refused, so a position is taken over in
  its order's legal entity. An extract without T030 (the OData mapping does
  not carry it, nor EBELN/EBELP) cannot tell SAP's GR/IR account: lines on
  the legal entity's `gr_ir` account are refused without their order items,
  and a company with receipts is warned about, since lines on SAP's GR/IR
  account are recognised only when it is the legal entity's `gr_ir`.
- **Later runs** top the positions up (next section).

For an SAP specialist: that ACDOCA carries the order item under EBELN and
EBELP on every GR/IR line, and the OData properties for them and for T030
(not mapped).

## As built: later runs top GR/IR positions up (review:batch21)

The cutover protocol runs a final delta migration after the parallel run
(docs/migration.md), but a position was taken over only by the run that
loaded its order, so a receipt or invoice SAP posted on an order already
loaded failed the open GR/IR check, and no run could correct it. Decisions
taken by default (ADR-0053 §d), each settled by an existing rule:

- **What a later run moves.** Each item's take-over moves what the earlier
  take-overs took over of it to SAP's position now, balance and open
  quantity: a delta, so the take-overs together always hold SAP's
  position, and running the same extract again posts nothing, publishes no
  event and counts nothing. It is an entry of its own per order, as the
  first take-over (WE, source integration, against the GR/IR account's
  line without an order item that offsets the new SAP documents' lines);
  the report counts the items as `gr_ir_top_ups` and the entry's external
  reference is `GRIR/<EBELN>/<run id>`. An item SAP has since invoiced in
  full is topped up to nothing.
- **A position the platform moved after cutover.** What the platform posted
  on the item (a receipt, an invoice, a GR/IR clearing) stays as it is,
  beside what was taken over: the take-over is SAP's part of the position
  only, as T72f kept `migrated` apart from received, invoiced and cleared,
  and as every other migrated document is loaded once beside the
  platform's postings rather than netted against them. So the
  reconciliation shows such a posting as a difference from SAP, as it does
  for a GR/IR clearing after the load (`TestMigratedGRIRClearing`), and a
  later run does not undo it.
- **What the ledger checks.** The take-over item names what the caller read
  as taken over (`GRIRItem.TakenOver`, read with `Postings.TakenOverGRIR`
  from the integration entries' order item lines, reversals included); the
  item is refused unless the ledger still finds exactly that, under the
  order's GR/IR lock (409 `ledger.gr_ir_position_exists`), and an item
  listed twice in an entry is refused too. A take-over no longer requires
  the item to have no position of the platform's own. The lock is the
  ledger's guard on positions (lockOrders, ADR-0008): the load takes the
  GR/IR locks of all its orders at once, after the goods_receipt range and
  before any period or the journal's ranges, the order a goods movement,
  an invoice's posting and a GR/IR clearing take them in, since an order
  an earlier run loaded is visible to them.
- **Which orders.** Those an earlier run loaded valued through GR/IR on
  the platform, when the run that loaded the order and every take-over on
  it are runs of the later run's fiscal year. A run's extract carries its
  fiscal year's journal and invoices only (`sap.Source`, ACDOCA and RBKP),
  while receipts are not limited to it, so it reads SAP's position right
  only for an order whose history it holds: topping up a take-over of
  another year's run would move the position wrongly while the GR/IR
  account still nets to zero and the reconciliation, from the same
  extract, agrees (review:batch21, round 1). So an order loaded or taken
  over by another year's run is left with a warning in the load report and
  stays a difference in the reconciliation, as before. An order loaded
  with no position open is told by the run that loaded it (below,
  review:batch23). One loaded valued directly (SAP had not received it
  yet) is not moved onto GR/IR, which stays the optional tracker leaf: its
  position remains a difference in the reconciliation. In balances mode a
  later run does not load the opening entry again, so its trial balance
  check fails after SAP posted anything; the top-up still moves the GR/IR
  account's balance between its lines only.

### An order loaded with no position open (review:batch23)

The top-up told a take-over's run, and so its fiscal year, by the
take-over entry's source document, and an order loaded with nothing open
has none: a same-year order SAP received or invoiced on again before
cutover was left with a warning and a reconciliation difference. Decided
by default (ADR-0053 §d), from what the platform records:

- **The run that loaded a record.** `integration.external_ref`, the
  platform's record of what a run loaded for each SAP key, names the run
  that recorded it (`migration_run_id`, migration 000168, a foreign key to
  the run of the same tenant, validated by 000169), rather than a new table
  or a guess from timestamps: no other record ties a loaded order to its
  run (neither the order nor its receipts name it), and
  the take-over entries do so only when something was open. Every
  reference a run records names it, not only an order's.
- **The fiscal-year guard covers the loading run.** The order is topped up
  when the run that loaded it (its `EKKO/<EBELN>` reference) and every
  take-over on it are runs of the later run's fiscal year; another year's
  loading run is warned about (`was loaded by a run of fiscal year …`), as
  another year's take-over is. A reference recorded before 000168 names no
  run and is not backfilled (nothing tells its run): such an order with no
  take-over is left with a warning and stays a difference, as before.
- **Idempotent, locks, audit and outbox as any top-up.** The read of the
  references takes no lock and comes after the orders' GR/IR locks, so
  the lock order of batches 22 and 23 is unchanged; the top-up is the
  entry above, with its audit record and event, and the same extract
  again posts nothing.

## As built: open GR/IR positions in a foreign currency (product owner 2026-10-31)

T72f refused an order in another currency than its company's: the take-over
posted SAP's company code amount (HSL) alone, and a take-over item kept no
local amount of its own. The product owner decided on 2026-10-31 (decision
E, as recommended) that SAP's foreign-currency open items load in their own
document currency, which ADR-0005's As built of that date built for vendor
items and down payments; this builds it for GR/IR positions. Decided by
default (ADR-0053 §d), each settled by ADR-0005 As built and the ledger's
GR/IR rules as built (T33b):

- **The order keeps its currency** (EKKO WAERS), as every loaded order
  does, and is valued through GR/IR like any other.
- **The take-over is in the order's currency.** Each item's balance there is
  the sum of SAP's document amounts (WSL) on its GR/IR lines, and its local
  balance the sum of their company code amounts (HSL), kept as SAP booked
  them and not converted again (`GRIRItem.TakesOverLC`), the group amount
  converted from it at the posting date's rate, as for every document loaded
  in a foreign currency. The entry records the rate SAP's amounts imply
  (`Posting.Rate`): HSL over WSL of the order's GR/IR line with the largest
  document amount, the first by document number and line of equal ones
  (`sourceRate`). The ledger takes a rate and local balances together only,
  on every item of a take-over in a foreign currency
  (`checkForeignTakeOver`; a 500 otherwise, a caller's error), and a rate
  only on an entry in a foreign currency (`postingRates`, 422
  `ledger.rate_not_recordable` otherwise; ADR-0005 As built, batch 40), so
  an order in company currency is taken over with neither. A top-up moves both
  balances from what the earlier take-overs hold (`GRIRTakenOver.BalanceLC`,
  guarded as the quantity and balance are, 409
  `ledger.gr_ir_position_exists`). An order in company currency is taken
  over as before, its local balance its balance.
- **After cutover nothing new is needed.** The position is in the order's
  currency, so a goods receipt or invoice in it moves it as one the platform
  posted (`ledger.gr_ir_currency_mismatch` refuses another currency, as
  before): a receipt adds at the posting date's rate; an invoice brings the
  balance in that currency to the order price of what is left, keeps in
  local currency the share of the value the position was booked at, and
  books the difference between that and the invoice's own local amount as
  an exchange difference (`fx_gain`/`fx_loss`), the price difference in
  the order's currency on its own line, as T33b built for the platform's
  positions. So the rate a later invoice clears at and the exchange
  difference on clearing are those rules, not new ones.
- **What the gate refuses** (T72b's never-guess rule), for an order in a
  foreign currency: a GR/IR line naming it in another document currency
  (RWCUR) than the order's (an order's positions are taken over in its
  currency, from all their lines, so a later top-up to nothing reads them
  too); and, as for a line of a document posted in a foreign currency
  (`foreignLine`), a document amount that does not parse, a line with an
  amount in one currency and none in the other (no money rule gives either
  a value in the other) or the two of opposite signs; a position whose
  balance is zero in one currency and not in the other; document amounts
  that add up to more than an amount holds; and a rate an entry cannot
  record (`ledger.RecordableRate`). A line of an open position of an order
  in company currency that is in a foreign document currency stays refused,
  now saying the order's currency.
- **Reconciliation.** The open GR/IR comparison per order item shows the
  balance in the order's currency beside the local one for such an order,
  `-1350.00 CAD (-1000.00 USD), quantity -4`, so an item taken over with the
  wrong document amount differs, and the total in local currency is joined
  by one per foreign currency, `Open GR/IR in USD total`, as open items in
  a foreign currency are totalled (review round 1).
- **SAP's documents.** In documents mode a document holding a GR/IR line of
  such an order posts in the order's currency unless SAP cleared it together
  with documents in another currency (`clearingCurrency`). In balances mode
  SAP's documents of such a position move together (`grirForeignDocuments`,
  review round 2): all of them, with those of any position a shared document
  ties them to, post in the order's currency, so SAP's side of the GR/IR
  account nets in it, to zero for a closed position and against the
  take-over's offset for an open one, unless SAP cleared a vendor line of
  any of them, in whatever currency it paid; then all stay in the opening
  entry in company currency, where a closed position nets to zero too. An
  open position with a document SAP paid (an item invoiced in part by an
  invoice SAP paid) and a position SAP paid only in part leave the account
  netting in local currency alone: ADR-0005 As built states the cases (with
  documents mode's document SAP cleared together with documents in another
  currency, three in all). **Accepted by the product owner on 2026-11-02**
  as documented: in them the account nets in local currency alone, the
  order's and the company's currency amounts offset, nothing is revalued
  and no value moves.
- **Tests and walkthrough.** Both modes through the load with real
  PostgreSQL (`TestForeignGRIRPositions`): the take-over in USD at SAP's
  rate, a receipt at the platform's rate and the invoice clearing the
  position in both currencies with its exchange loss; a top-up and the same
  extract again (`TestForeignGRIRTopUp`); the gate (`TestForeignGRIRQuality`);
  the ledger's take-over (`TestGRIRTakeOverForeign`) and its refusals of a
  rate or local balance out of place (`TestGRIRTakeOverRefusals`); the
  GR/IR account netting to zero in USD once the position is cleared, in
  either mode; in balances mode, the GR/IR account by entry currency for a
  closed position paid and unpaid, and an open one invoiced in part, paid
  and unpaid (`TestForeignGRIRBalancesMode`), and the documents grouped
  through a receipt of two positions (`TestGRIRForeignDocumentGroups`); and
  walkthrough step 3 with the mock's USD order of the CAD company.

For an SAP specialist (from memory, not assumed by the load beyond what the
gate refuses): that SAP's GR/IR lines of an order carry the order's currency
as their document currency (RWCUR), including an invoice's in a currency
other than the order's, which the gate refuses; that SAP clears GR/IR in
company code currency at the receipt's value and posts the exchange
difference to an account of its own, so an open position's HSL is its
receipts' value at their rates; and whether SAP ever posts a GR/IR line of
such an order in company code currency alone.

## As built: later runs clear what SAP cleared across runs (review:batch22)

The final delta run at cutover loads what SAP posted since the earlier
runs, but the load cleared a vendor line SAP cleared only when this run
had posted every document of the clearing, so a payment the delta run
loaded left the invoice an earlier run loaded open, beside it. The open
payables per vendor net the two to zero, so no check showed it. Decided
by default (ADR-0053 §d), as the GR/IR top-up above decided the same
question for positions:

- **What a later run clears.** A clearing (the extract's vendor lines on
  a reconciliation account with one AUGBL in a company) whose documents
  are all on the platform, posted by this run or by an earlier one (its
  `ACDOCA/<BUKRS>/<GJAHR>/<BELNR>` reference), is cleared by the clearing
  document, or by the latest of them when the clearing document is not in
  the extract, as before (`Postings.Clear`). An earlier run's entry is
  named by its fiscal year (`Postings.EntryYears`) and its line numbers
  are the SAP lines' numbering again (`journalPosting`): a posted SAP
  document's lines do not change, only their clearing does. A document
  refused, by this run or earlier, still leaves its clearing undone.
- **Idempotent.** A clearing whose documents earlier runs posted and
  whose items one clearing cleared already (`Postings.ClearedTogether`)
  is left as it is, so the same extract again posts nothing, publishes no
  `items_cleared` and counts nothing. Items the platform has cleared or
  reversed since (a reversal clears the reversed entry's items), or that a
  payment proposal holds, are not cleared: `Postings.Clear` refuses them
  (409 `ledger.item_already_cleared`, `ledger.entry_reversed`,
  `ledger.item_reserved`), reported under
  `clearing <BUKRS>/<AUGBL>`, and the load is rolled back as on every
  refusal, rather than SAP's clearing guessed onto other items.
- **No fiscal-year guard.** Unlike a GR/IR position, a clearing is read
  from the extract's own lines: an extract holds its fiscal year's
  journal only (`sap.FilterScope`) and the references carry GJAHR, so
  every document of a clearing the run sees is one of its year's, posted
  by a run of the same year.
- **Locks.** The run reads under the shared entry bucket locks it takes
  first (`LockAllEntries`) and clears as before; it takes no new lock,
  so the lock order of batch 22 is unchanged.
- **Reconciliation.** Open payable items, the number of open ordinary
  vendor items per vendor (lines of amount 0 left out, as the load leaves
  them out), in total and per vendor (`open items of vendor <LIFNR>`), in
  both modes. A clearing SAP recorded and the platform did not leaves
  both items open, which the amounts net away and the count shows.

Not built, for the product owner: a clearing whose counter-item is
outside the extract, which is another fiscal year's (a prior-year invoice
paid this year). The extract has neither the item nor the clearing's date
and year, so the run cannot find the counter-item, and every such item
fails its run today: the clearing does not net to zero (422
`ledger.clearing_not_balanced`) in documents mode, and in balances mode
the cleared items of the year do not net to zero either. Which items a
fiscal year's migration holds open (open at the extract's date, as the
load reads AUGBL now, or at the year's end, SAP's key-date reading of
BSIK/BSAK by the clearing date AUGDT) is an accounting rule nothing here
settled; the options are in the tracker item that follows this one.

Decided by the product owner on 2026-10-31 (as recommended in batch 25,
sapclear case 2), not built yet: a fiscal year's migration holds open, at
the year's end, every vendor item SAP cleared in a later fiscal year, as
SAP's key-date reading of BSIK/BSAK by the clearing date does, so a
prior-year invoice paid this year is loaded open in its year's run and the
later year's run clears it with the payment it loads. This needs SAP's
clearing date and the clearing document's fiscal year in the extract
(AUGDT and AUGGJ, the field names stated from memory, for an SAP
specialist to confirm in the extract, which the build waits on); the load
then reads a line as cleared only when its clearing falls in the run's
fiscal year. Tracked as the item that follows this one.

## As built: a contract's expiry events name the company and the supplier (review:batch27)

- **Decision.** `ContractExpired` and `ContractExpiring` gave the company
  and the supplier by `legal_entity_id` and `supplier_id` alone, so a
  consumer alerting a buyer had to look both up (REVIEW.md, API-consumer
  classes: an identifier without the name its consumer needs). Both now
  give `legal_entity_code` (the company code, as `LegalEntityCreated`
  names it `code`), and `supplier_number` and `supplier_name` (as
  `BusinessPartnerCreated` gives them, and as the payments' and the
  release refusals' supplier names since batches 22 and 27). Each is named
  `<x>_code`, `<x>_number`, `<x>_name` beside `<x>_id`, as ADR-0038's
  payments name their house bank and documents. Decided by default from
  those precedents.
- **Optional.** The fields are optional in the schemas: asyncapi's v1
  rule (and `api/eventcompat.go`) admits new optional payload fields only,
  a newly required one being a breaking change. The job gives all three on
  every event it publishes now; an event published before them has none.
- **Where.** `eventNames` (`expiry_names.go`) names the company by the
  code the round read when it dated the legal entity (`companyDays`, through
  `config.EntityDay` and `tenancy.CompanyCode`, the one company-date helper
  since batch 28), so the round reads each legal entity once; and the
  supplier's number and name through a new `Partners` port
  (`SupplierNames`, partner's `Payees.Names`, the port payments reads them
  with; procurement already depends on partner, so no dependency direction
  changes). A legal entity the directory does not know (refused by
  `config.EntityDay` as the round dates it), a supplier the partner context
  does not hold, or either context failing, fails the tenant's round (it is
  retried the next round, idempotently) rather than publishing an event
  that leaves it unnamed, as `idref.Resolve` does for pages. Batch 28's
  integration moved the code read into `companyDays`: the lane had read the
  legal entity a second time, apart from the dates, while the helper was
  built in parallel.

## As built (apqty): a receipt's reversal is held to what is invoiced

- **Decision.** A goods receipt's reversal (102) is refused (409
  `procurement.reversal_below_invoiced`) when it would leave an order item
  received less than the supplier documents posted on it invoice, net of
  credit memos and with what SAP invoiced before migration: the match
  holds an invoice to what was received, so the reversal is held to what
  was invoiced (03 §3.9). The refusal names the receipt, the order item
  and both quantities, and says what comes first: the supplier's credit
  memo, or a return. Settled by the tracker leaf and 03 §3.9; decided by
  default.
- **Returns are not held to it.** A return (122) after the invoice is the
  flow this ADR's returns describe: the goods go back and the supplier's
  credit memo clears the remaining GR/IR debit. Holding the return to the
  invoiced quantity would refuse that flow, so only the reversal of a
  receipt, which says the goods never arrived, is checked. The reversal of
  a return (123) only adds to what is received.
- **Where and in which order.** Procurement reads the invoiced quantities
  through its `Invoices` port (`Invoiced`, implemented by payables'
  `Invoices`, wired in `internal/app`; procurement already depended on
  payables through that port for `HasInvoices`, so no dependency direction
  changes) after it locks the order items, which a document's posting and,
  since this step, its reversal lock too, in the order 03 §3.9 gives
  (ADR-0008: state-changing rows are read `FOR UPDATE`). Payables reverses a credit memo or an invoice under the same item
  locks and holds it to the same quantities
  (`payables.reversal_exceeds_received`, `payables.reversal_exceeds_invoiced`,
  `payables.reversal_leaves_revaluation`).
