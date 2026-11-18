# Depth plan 3: third variant slices (18 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants go on, pilot-relevant first). It follows `plan-depth-1.md`
and `plan-depth-2.md`, whose rules it does not repeat: each lane splits
two named variants of its placeholder into slices in the form of
`plan-breadth-week3.md` (Adds, Acceptance, Later) and builds them.
`docs/tracker.json` stays the only plan (07 §7.5); each leaf's `r` names
its section below. Each lane writes its own `## <lane>` section.

## cm

Placeholder: "later: customer credit memo variants"
(`plan-breadth-week1.md` §T75d, Later; ADR-0063 §7). Two variants are
split here: debit memos, and reversing a credit memo or a debit memo.
Paying a credit out, credit memos from returns and credit memo requests
(the rest of week 2's sales leaves) stay in the placeholder. SAP S/4HANA
is the reference, stated from memory and not checked against a source in
the lane: a customer debit memo is an invoice-like document charging the
customer more (FB70/FB75 in FI, billing type L2 in SD), posted Dr customer
and Cr revenue and output tax; a document is reversed by FB08, which posts
a reversal document with the signs turned and clears the original's open
item against it, refused once the item is cleared; the reversal posts
into an open period.

### T75j Customer debit memo

**Capability.** An AR accountant enters and posts a debit memo to a
customer, charging what an invoice did not (a price correction, freight,
a late fee): it posts as an invoice does, with its own journal document
type, and is the customer's open item, with its output tax.

- **Reuses:** T75a's document with a kind (`kinds.go`), T75b's posting
  (Dr customer, Cr items and output tax at the document's own date), the
  payment terms (the term entered, else the customer's from the
  document's date), the events and audit records.
- **Adds:** the kind `debit_memo` on `POST /v1/customer-invoices` (and
  as a filter of `GET /v1/customer-invoices`); posting it as journal
  document type `DD` with its own number range `journal_dd` and the
  header text "Customer debit memo <number>"; the document number from
  the `customer_invoice` range, as a credit memo's is. A debit memo names
  no invoice (422 `receivables.reference_not_allowed`, as an invoice).
  Migration: the kind check widened.
- **Walkthrough:** `step_debit_memo`: Ruth enters and posts a debit memo
  to Northwind for freight charged after its invoice, CAD 150.00 and 5%
  GST; the DD entry debits Northwind on 1200 and credits revenue and
  2300; the memo is Northwind's open item with the customer's term; a
  debit memo naming an invoice is refused.
- **Acceptance:** a debit memo is entered and checked as an invoice is
  (items more than zero, tax per item at its own date); it posts Dr the
  customer's reconciliation account with the gross, Cr each item's
  account and output tax, as a `DD` entry of receivables drawing from
  `journal_dd` (a company without that range is refused, 422
  `config.no_number_range`, and nothing posts); its open item is a debit
  with its terms; the events carry its amounts positive and the kind;
  it names no invoice and is not counted against any invoice's credit
  limit.
- **Depends on:** T75a–T75d (in `main`).

### T75k Reversing a customer credit memo or debit memo

**Capability.** An AR accountant reverses a credit memo or a debit memo
posted in error: a reversal document turns its entry's signs, its open
item is cleared against the reversal, and a reversed credit memo no
longer counts against its invoice's credit limit.

- **Reuses:** the ledger's reversal port (`ledger.Postings.Reverse`: the
  reversal entry with its tax items negated on the original's tax date,
  the original's open item cleared against the reversal's, refused when a
  line is cleared or the period is closed), T75d's limit (only posted
  memos count), the row lock and `If-Match`.
- **Adds:** `POST /v1/customer-invoices/{id}:reverse`
  (`receivables.invoice.post`, `If-Match`, `Idempotency-Key`,
  human-only) with a `reason` (1 to 200 characters) and an optional
  `posting_date` (absent: the document's own posting date, as the
  ledger's reversal and T75i date theirs). The document becomes
  `reversed` and shows the reversal entry (id, year, number), its
  posting date and the reason; `CustomerInvoiceReversed` (`x-webhook`,
  without the reason) and the audit record
  `receivables.customer_invoice.reverse` (without the reason, ADR-0079:
  free text stays out of audit details). Refusals: a document not
  posted (409 `receivables.invalid_status`); an invoice, corrected by a
  credit memo instead (409 `receivables.invoice_not_reversible`); a
  memo another context entered from its document (sales' billing
  cancellation or credit memo request; 409
  `receivables.entered_from_source`); a reversal dated before the
  document posted (422 `receivables.reversal_before_posting`); an open
  item cleared, netted in a payment or cleared against its invoice (409
  `receivables.document_cleared`; the ledger's `ledger.entry_cleared`
  under the entry's lock); a closed period (409
  `ledger.period_not_open`, the period's problem). The ledger's
  `reverseJournalEntry` names this path for receivables' entries.
  Migration: the status `reversed`, the reversal columns, the trigger
  admitting posted → reversed for memos alone.
- **Walkthrough:** `step_memo_reversal`: Ruth reverses step_debit_memo's
  debit memo; its open item and the reversal's are cleared, Northwind
  owes what it did before; a credit memo naming an invoice is posted and
  reversed, and the credit it took is free again (a memo of the full
  remaining amount posts); a reversal of the reversed memo, of an
  invoice, and into a closed period are refused.
- **Acceptance:** only a posted credit or debit memo entered by a person
  is reversed, once, at the version read; the reversal entry is the
  original's with the signs turned, document type and source kept, tax
  taken back on the original's tax date, on the date asked or the
  document's posting date; the memo's item and the reversal's are
  cleared together and no longer open; a memo whose item is cleared is
  refused and nothing posts; a closed period refuses with the ledger's
  problem; a reversed credit memo frees its amount under its invoice's
  limit; a replay answers the reversed document; two reversals at once
  post one.
- **Depends on:** T75j.

**Later:** a debit memo naming the invoice it adds to; an invoice's
reversal (it is corrected by a credit memo); reversing a memo whose item
is cleared by resetting the clearing first in one step; reversal reason
codes from configuration; reversing documents sales entered (through
sales' own paths); new country pack versions bringing `journal_dd`.

## price

Placeholder: "later: price determination variants"
(`plan-breadth-week2.md` §T80a, Later; ADR-0067 §10, §11). Two variants
are split here; the rest stay in the placeholder. SAP S/4HANA's condition
technique is the reference (from memory, not a primary source: condition
records with scales, scale basis quantity or value, scale type "from";
the customer price group of the customer's sales area data, an access
sequence that reads the most specific key first); nothing below depends
on a fact of SAP's beyond that shape.

### T80e Scales on a condition record, by quantity or by value

**Capability.** A pricing analyst records a price or a discount whose
rate falls or rises with what is bought: a price per each that is lower
from 1000 pieces and lower again from 5000, or a customer's discount that
is larger from an order value of 1000.00; a quotation or order item priced
from the record takes the rate of the scale level its quantity or value
reaches, and shows the level.

- **Reuses:** T80a's record and its determination (`validRecord`, the
  customer's before the general one, the latest `valid_from`), T80b's
  procedure (`conditioned`), `money.Scale`, `money.PositiveQuantity`,
  `currency.ParseAmount`, the `ref` units and currencies.
- **Adds:** on `POST /v1/sales-prices` `scale_basis` (`quantity` or
  `value`) and `scale[]`, at most 20 levels, each `from_quantity` (a
  `Quantity`) or `from_amount` (a `Money`) by the basis, more than zero and
  strictly rising, with its rate as the record's kind gives one:
  `unit_price` (per the record's `price_unit`, in its currency) for a
  price, `percent` for a discount. The record's own rate applies below
  the first level (level 0); an item takes the rate of the highest level
  whose `from` its basis reaches ("from" scale). A price's scale is by
  quantity, in the record's unit; a discount's by quantity in the unit
  its levels state, which an item in another unit does not take, or by
  value in the currency they state, the value before the discount, which
  a document in another currency does not take (nothing is converted, as
  §10 converts no price). `SalesPrice` shows `scale_basis` and `scale[]`
  with each level's number; a condition on an item shows its `scale`
  (basis, the level reached, its `from`). `SalesPriceChanged` carries the
  basis and the levels. Migration: `scale_basis`, `scale_uom_code`,
  `scale_currency` on `sales.price`; `sales.price_scale` (insert-only).
- **Walkthrough:** `step_price_scales`: Paula records BOLT-M10 at 0.80 CAD
  each, 0.70 from 1000 and 0.60 from 5000, and Northwind's discount on
  it, one percent, two from an order value of 1000.00 CAD and three from
  5000.00; Sally's quotation of 6000 bolts takes 0.60 (level 2), 3600.00,
  and two percent off it (level 1), 72.00, net 3528.00; one of 500 takes
  0.80 (level 0) and one percent; a scale whose levels do not rise, and a
  price with a value scale, are refused naming the level or the basis.
- **Acceptance:** the level reached is exact (a quantity compared as a
  decimal, a value in minor units); a scaled record's rate below the first
  level is its own; a scale with no basis, a basis with no levels, a level
  with the other basis's `from`, a `from` of zero or less or not above
  the one before, a level's rate of the other kind, a quantity level in
  another unit than the price's, levels in two units or currencies are
  each refused naming the field (422 `sales.invalid_scale`); a level's
  price not more than zero (`sales.amount_not_positive`), in another
  currency than the record's (`sales.currency_mismatch`), a level's
  percent out of range (`sales.invalid_percent`), an unknown unit or
  currency of a discount's scale (`sales.unknown_unit`,
  `sales.unknown_currency`), each naming the level's field; a price with a
  value scale is refused (`sales.invalid_scale`, `scale_basis`); a record
  without a scale prices as before; an item priced once keeps its rate
  and level, as §10 fixes a price.

### T80f Prices per customer price group

**Capability.** A configuration administrator defines customer price
groups (wholesale, retail); a pricing analyst puts a customer in one in
its sales data for a company and records prices and discounts for the
group; a quotation or order item for the customer takes the customer's
own record first, then its group's, then the general one, and shows which
step of the access found it.

- **Reuses:** T80b's configuration section `pricing`, its validation and
  transitions (`internal/config/pricing.go`, `diff.go`), T80a's records
  and determination, `partner.Postings.PostingBlock` (the customer, as a
  price record's customer is checked), `etag`, audit, the outbox, cursor
  paging.
- **Adds:** `pricing.price_groups[]` (a code and a name; never removed
  once activated; tier 2, a name tier 1); table `sales.customer_sales_data`
  (a customer's sales data in a company: its `price_group`, versioned);
  `POST /v1/customer-sales-data`, `GET /v1/customer-sales-data`,
  `GET /v1/customer-sales-data/{id}`, `PATCH /v1/customer-sales-data/{id}`
  (`If-Match`); event `CustomerSalesDataChanged`; `price_group` on a price
  record (`sales.price.price_group`, not with `customer_id`) and a filter
  of `GET /v1/sales-prices`; the access sequence customer → price group →
  general for prices and discounts alike; `access` (`customer`,
  `price_group`, `general`) and `price_group` on an item's conditions.
  Permissions `sales.price.read` and `.write` (no new ones: the sales data
  holds the pricing group alone, which only decides prices).
- **Walkthrough:** `step_price_groups`: Ada adds the price groups 01
  Wholesale and 02 Retail through a governed change; Paula puts Contoso in
  01 and records 01's price for BOLT-M10, 0.75; Sally's quotation to
  Contoso takes it (`access` `price_group`), Northwind's the general one;
  Paula records Contoso's own price, which then wins; she moves Contoso to
  02, which has no record, and the next item takes the general price; a
  second sales data for Contoso, an unknown group, a record with both a
  customer and a group and a stale `If-Match` are refused.
- **Acceptance:** the customer's record before its group's before the
  general one, then the latest `valid_from`, then the one recorded last;
  a customer without sales data, or without a group, has the general
  record; a group the configuration does not have is refused naming the
  field (422 `sales.unknown_price_group`); a record with a customer and a
  group (422 `sales.customer_or_group`, `price_group`); a second sales
  data of the customer in the company (409
  `sales.customer_sales_data_exists`); a change that changes nothing (422
  `sales.nothing_to_change`); a stale `If-Match` 412; two changes racing
  end with one; the sales data's customer is named by id, number and
  name, its company by code and name; items priced before keep their
  price when the group changes.

**Later (still in the placeholder):** prices per price list type or sales
organization; surcharges and freight; minimum prices and manual price
approval; currency conversion of a price; repricing a document; mass
changes and price uploads; prices taken over from SAP. Configurable access
sequences stay in "later: pricing configuration variants".

## alloc

Placeholder: "later: allocation cycle variants" (`plan-breadth-week1.md`
§T60g, Later; ADR-0064 §5). Two variants are split here: shares from
statistical key figures, and fixed amounts with the reversal of a run.
SAP S/4HANA's assessment and distribution cycles (KSU1/KSV1 with their
segments' sender and receiver rules, statistical key figures KK01 with
their values posted by KB31N, cycle reversal from KSU5/KSV5) are the
reference, stated from memory and marked so in ADR-0064's "As built".
A cycle stays one segment (one sender, its receivers): several segments
and senders in a cycle stay in the placeholder.

### T60j Receiver shares from statistical key figures

**Capability.** A controller defines a statistical key figure of a
company (employees, floor space), posts its values per cost center and
period, and defines a cycle whose receivers' shares are the key figure's
values of the period run: a run splits the sender's costs in proportion
to them.

- **Reuses:** T60f/T60g's cycles, their run, lock and entry
  (`money.Allocate` over exact fractions); the ledger's port for the
  centers (`CostCenterRefs`); the company's regular periods
  (`regularPeriod`); `ref.unit_of_measure`; audit; the controlling
  outbox.
- **Adds:** tables `controlling.statistical_key_figure` (code unique per
  company, name, unit of measure; not changed in the first version) and
  `controlling.statistical_key_figure_value` (insert-only: one row per
  cost center of a posting, a non-zero quantity with up to six
  decimals; a center's value of a period is the sum of its rows);
  `POST /v1/statistical-key-figures`, `GET` (list, by code) and
  `GET /v1/statistical-key-figures/{id}`; `POST
  /v1/statistical-key-figures/{id}/postings` (a period's values for
  cost centers of the company); `GET
  /v1/statistical-key-figures/{id}/values` (a period's value per cost
  center with the total); permissions `controlling.key_figure.read` and
  `.write`; the cycle's `share_basis` (`fixed_percentages`, the default,
  or `statistical_key_figure` naming the key figure, whose receivers
  carry no percentage); events `StatisticalKeyFigureChanged`,
  `StatisticalKeyFigurePosted`, and `AllocationCycleChanged` .v2 beside
  .v1 (whose receivers must have a percentage).
- **Walkthrough:** `step_distribution` extended: Carla defines the key
  figure `EMPL` (employees, unit EA), posts 3 for CC-1100 and 1 for
  CC-1300 for the period, defines a distribution `SHARED-EMP` from the
  shared services whose shares are the key figure, Alex posts more
  shared supplies, and the run splits them 3 to 1, rounded to the cent.
- **Acceptance:** a key figure's code is unique per company (409); its
  unit is a known unit of measure (422); a posting names a regular
  period of the company and active cost centers of its company, each
  once, with non-zero quantities (422 naming each line); a cycle by key
  figure names one of its company (422) and its receivers no percentage
  (422 `not_allowed`), a cycle by fixed percentages as before; a run
  reads the receivers' values of the period it is run for: a receiver
  whose value is negative refuses the run (409
  `controlling.allocation_key_figure_negative`), receivers whose values
  add up to nothing refuse it (409
  `controlling.allocation_key_figure_missing`), a receiver without a
  value takes nothing; the shares add up to each cost to the minor unit;
  the values used are in the run's audit record; schema tests for every
  new rule.
- **Depends on:** T60f, T60g.

### T60k Fixed amounts per receiver, and an allocation run reversed

**Capability.** A controller defines an assessment cycle whose
receivers are charged fixed amounts each period, and reverses a run of
any cycle in its period, after which the cycle may be run for the period
again.

- **Reuses:** T60g's assessment (one entry on the assessment account);
  `ledger.Postings.Reverse` (the run's entry reversed in every ledger it
  was posted to, on its own date), as T60i reverses a settlement; the
  lock on the sender's cycles (`heldCycle`).
- **Adds:** `share_basis` `fixed_amounts`, an assessment's alone, each
  receiver with its `amount` in the company's currency (above zero);
  table `controlling.allocation_run_reversal` (insert-only, one per
  run); the run's `attempt` within its cycle and period, so that one run
  per cycle and period is in force; `POST /v1/allocation-runs/{id}:reverse`;
  the reversal on `AllocationRun`; event `AllocationRunReversed`; the
  ledger's `:reverse` of a run's entry names the path.
- **Walkthrough:** `step_assessment` extended: Carla reverses the
  `FACILITIES` run (the receivers' charges gone, the sender's credit
  gone), defines `FAC-FIXED` charging CC-1100 CAD 250.00 and CC-1300
  CAD 150.00 from the facilities, and runs it for the period: CAD
  400.00 charged; a second reversal of the first run is refused.
- **Acceptance:** a fixed-amounts cycle is an assessment (422
  `not_allowed` on a distribution), each receiver with an amount above
  zero in the company's currency and no percentage (422 naming the
  field); its run charges each receiver its amount and credits the
  sender with their total whatever the sender's costs, posts even when
  the sender has none, and reads no cost (`account_count` 0); a run is
  reversed once (409 `controlling.allocation_run_reversed`), on its own
  posting date, so its period must still be open (the ledger's 409
  `ledger.period_not_open`), at the cycle's version (412); a reversed run
  no longer counts as the cycle's run of the period nor as the sender's
  assessment of it, so either cycle may run again; a reversal and a run
  of one sender follow one another under the sender's cycles' lock;
  every reversal audited and published; schema tests.
- **Depends on:** T60j.

**Later (still in the placeholder):** shares from actual or plan values;
fixed portions; fixed values of a key figure carried to later periods;
several senders and segments in a cycle; iterative cycles; simulating a
run; plan allocations; overhead rates and activity allocation; receivers
other than cost centers; fixed amounts on a distribution (per account);
reversing into a later open period when the run's period is closed; a
lock of a period for statistical key figure postings; a listing of a key figure's postings; changing or
deleting a key figure.

## crlim

Placeholder: "later: credit limit variants" (`plan-breadth-week1.md`
§T77a, Later; ADR-0066 §2). Two variants are split here: validity dates
and reviews, and the approval of limit changes. Limits across the group
and risk classes stay in the placeholder. SAP S/4HANA's credit
management (FSCM, the business partner's credit segment data: a limit
with a "valid to" date, a date of the next check, and limit requests
approved before they apply) is the reference, stated from memory and not
checked against a source in the lane; nothing below depends on a fact
of SAP's beyond that shape.

### T77c Credit limit validity and reviews

**Capability.** A credit manager gives a customer's limit a validity (from
and to, either open) and a date for its next review; the credit check and
the exposure use the limit only on the days it is valid, and a worklist
lists the limits due for review or expiring by a date.

- **Reuses:** T77a's limit, its `If-Match` change, audit record and
  `CreditLimitChanged`; `config.DocumentDay` for the company's day in its
  time zone (the limits port of tenancy gives the code); the list's
  keyset pages.
- **Adds:** `valid_from`, `valid_to` and `next_review_on` (dates, each
  optional) on `POST /v1/credit-limits`, `PATCH /v1/credit-limits/{id}`
  (merge patch: `null` clears; `limit` no longer required, at least one
  field) and the representation; `valid_to` before `valid_from` is 422
  `credit.invalid_validity` naming `valid_to`. `GET /v1/credit-limits?due_by=`:
  the limits whose next review date or end of validity is on or before
  the date (the worklist), still by customer number.
  `GET /v1/credit-exposure` gains `day`, the company's day the limit's
  validity is judged on; `has_limit` is true for a limit valid on that
  day, and `credit_limit_id` names the customer's limit even when it is
  not. `CreditLimitChanged` and the audit records carry the dates.
  Migration: three nullable columns (absent: valid without a start or
  an end, no review planned, which is what every limit set before is),
  the check `valid_to >= valid_from`, the update grant widened.
- **Walkthrough:** `step_credit_limit_validity`: Ruth plans Northwind's
  next review and the limit's end; the worklist by the review date lists
  it, by an earlier date not; a limit whose validity ended yesterday is
  not used: the exposure has no limit and an amount far above the old
  limit is within; a validity ending before it starts is refused; the
  validity is restored and the limit is used again.
- **Acceptance:** the check at order confirmation (`credit.Check`) and
  the exposure read compare with a limit only when the company's day is
  within its validity; outside it the customer is treated as one without
  a limit (no restriction, ADR-0066 §4), and nothing is converted or
  stored; the worklist's filter, the dates' merge-patch semantics and the
  check constraint are tested; changes of dates alone are audited with
  their values before and after.
- **Depends on:** T77a, T77b (in `main`).

### T77d Approval of credit limit changes

**Capability.** A new credit limit, or a change that raises a limit or
changes its validity, takes effect only once a second person approves
it through the approvals context; a lowered limit, or a new review date,
takes effect at once.

- **Reuses:** the approvals context (`approvals.Approvals.Open`, the
  `Handler` and `Locator` callbacks, the approver never the requester),
  materials' governed changes (S1b) as the pattern, T77a's limit, lock
  (`hold`, then the limit's row) and records.
- **Adds:** the object type `credit_limit_change`, approved by the fixed
  role `controller` (as period unlocks and manual entries are) holding the
  new approval permission `credit.limit.approve`. Table
  `credit.credit_limit_change`: kind `create` or `change`, the values
  asked for and those before, status `pending`, `applied`, `rejected` or
  `cancelled`, the approval request, one pending per limit. A limit gains
  `status` `pending` (asked for, never approved, not in effect) or
  `active`, and `pending_change_id`. `POST /v1/credit-limits` answers the
  limit `pending` with its create change; approved, it is `active` and
  `CreditLimitChanged` is published. `PATCH` of an active limit applies
  at once a change that raises nothing (the amount not above the limit's,
  the validity kept; the review date may change with it); any other
  opens a change and answers the limit as it is; a change of a limit with
  a pending change is 409 `credit.change_pending`; a change of a pending
  limit asks again (a new create change). `GET /v1/credit-limit-changes`
  (filters limit and status) and `GET /v1/credit-limit-changes/{id}`, the
  approver's read. Audit records `credit.credit_limit_change.request`,
  `.apply`, `.reject`, `.cancel`; the limit's `.change` when a change is
  applied, naming it. Migration: the status (default `active`, every
  existing limit), the table, the permission.
- **Walkthrough:** `step_credit_limit` (step 59) and the steps that raise
  Northwind's limit later have each new or raised limit approved by
  Carla, the controller; `step_credit_limit_approval`: Ruth raises the
  limit, the exposure still uses the old one, she cannot approve her own
  request, a second change waits for the first; Carla approves and the
  raise holds; Ruth lowers the limit, which holds at once; a raise Carla
  rejects leaves the limit as it was.
- **Acceptance:** only an approved change applies, once, and nothing is
  in effect before; a pending limit is not used by the check or the
  exposure; the requester cannot approve; rejection or withdrawal leaves
  the limit as it is; a lowering applies at once; the lock order (the
  customer's key, the limit's row, the change's row) is the check's, so
  an approval and an order confirmation of the customer run one after
  the other; audited and published; schema tests for every rule.
- **Depends on:** T77c.

**Later (still in the placeholder):** limits across the group; risk
classes; approval tiers by amount or a configurable approver role;
lowering a limit while a raise waits (it is refused until the raise is
decided); a limit request raised from an agent's recommendation.
