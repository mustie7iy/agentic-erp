# ADR-0063: Accounts receivable

**Status:** accepted (T75a, 2026-11-11; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T75a's last commit). It decides
the receivables context and the documents of T75a-T75e
(`docs/design/plan-breadth-week1.md`, Lane A). Each slice records what it
built under "As built"; those of T75a to T75e are there now. The
defaults taken and the questions left are listed at the end for the product
owner's digest.

## Context
The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Finance › Accounts receivable has four placeholders:
Customer open items, Incoming payment posting and clearing, Credit memos
and AR aging report. `plan-breadth-week1.md` splits them into the slices
T75a-T75e and asks T75a to write this ADR.

What the design already settles:

- 02 §2.2 names `receivables` among the later contexts, "with the same
  rules" as the built ones. 03 §3.16 gives the patterns: documents as a
  header and items with a `document_number` from the number ranges;
  financial effects only through ledger postings, with `source_context`
  and `source_document_id` on the journal entry; partners through partner
  roles ("`customer` joins `supplier`").
- The ledger already knows customers: a line on a customer reconciliation
  account names a business partner that is active and has the customer
  role (`partner.Postings.PostingBlock`), and such lines are open items
  (`GET /v1/open-items`).
- ADR-0047: incoming payments wait on the house bank's incoming payments
  clearing account "for accounts receivable" (T49h).
- ADR-0049 §4: a document's tax date decides its rate. §7: the ERP issues
  no sales invoices until phase 3, so the pilot's output tax enters as
  journal entries with output tax codes; the pilot bills in its own
  system. Sending e-invoices is T65h (ADR-0061).
- ADR-0005 (money in minor units), ADR-0003 (RLS), ADR-0007 (outbox),
  ADR-0008 (row locks and state guards), ADR-0011 (audit), ADR-0053
  (slices, headless first).

No external source was read for this ADR (no network in the lane), and it
states no tax rate, regulation or SAP table of its own.

## Decision

### 1. A context `receivables`, built as payables is
`internal/receivables` with the schema `receivables` (RLS on every table,
`tenant_id` leading every key, its own outbox and inbox in
`outbox.Contexts`). It imports `ledger`, `partner`, `tenancy` and `config`
and reads them through the ports it declares, as payables does
(`internal/layout`): the ledger's `Accounts` (postable accounts, tax codes
on a date, assignment checks) and, from T75b, `Postings`; the partner
context's check of a customer and its names; tenancy's legal entities; the
configuration's number ranges and tax settings. The dependency directions
are those 02 §2.2 draws for payables: ledger → receivables, partner →
receivables. It is not a leaf: week 2's billing posts through it. The
readers other contexts need of customer open items (collections, credit,
the aging) are the ledger's (`ledger.OpenItems.Receivable`, T75b), not
receivables', so they do not import it.

Nothing is copied from payables: a helper both need moves to
`internal/platform` or the shared package that owns it. T75a moved two,
which payables now calls too: `problem.Invalid` (a 422 naming the
request's field) and `currency.ParseAmount` (an API amount read with its
currency's digits, 400 `request.invalid_amount` otherwise). The rest was
shared already: the tax calculation is `internal/tax`'s, amounts
`money`'s, paging `platform/pagination`'s.

### 2. The customer invoice
A customer invoice entered in receivables is **the accounting record of an
invoice the company issued**. It is not the legal tax invoice and sends no
e-invoice (ADR-0049 §7: the pilot bills in its own system; T65h) — plan
default 1.

- `receivables.customer_invoice` is the header: company, customer, kind,
  invoice date, an optional posting date, currency, the net, tax and gross
  totals, the status, a `document_number` from the company's
  `customer_invoice` number range of the invoice date's fiscal year.
  `receivables.customer_invoice_item` are its items: an account (revenue,
  as a rule: any account that can be posted in the company), a net amount,
  an optional output tax code with the tax calculated for the item, an
  optional description, cost center and profit center.
- Amounts are `money.Minor`, never negative; the **kind** gives their
  direction, as `payables/kinds.go` does for supplier documents. T75a has
  the kind `invoice`; T75d adds `credit_memo` with `reference_invoice_id`.
- The gross is net and tax exactly, in minor units: the items' net and tax
  add up to the header's, and the header's gross is their sum (a check
  constraint holds the header's).
- Statuses: `draft` when entered (T75a), `posted` by T75b. A draft is not
  changed or cancelled in the first version (plan, "Later"): what was
  entered is fixed, which the grants and an insert-only trigger hold.
- No sales order is involved; invoices created by billing come in week 2
  and post through this context.

### 3. The customer
The customer is a business partner with the customer role, as the ledger
checks one of a line on a customer reconciliation account
(`partner.Postings.PostingBlock`): active, holding the role and, since
T74a, not blocked for posting in the company (422
`receivables.customer_not_usable` otherwise). Entering an invoice checks
that and nothing more: a customer without company data may be invoiced.
From T75b a receivables document **requires the customer's company data**
in the company to post (its reconciliation account and payment terms,
`partner.customer_company`, T74a): a customer without it is refused at
posting, 422. A customer without company data stays postable by manual
journal entries as today — plan default 2.

### 4. Tax
Each item's tax is what its **output** tax code gives at the **invoice
date**, which is the document's tax date (ADR-0049 §4; the first version
has no tax date of its own, as supplier invoices default to theirs). The
calculator is `internal/tax`: rounded as the company's tax settings say
(once per code and component over the document by default, or per line),
each item keeping its share so the items add up to the document. A code
that is no output code of the company, or has no rate in force on the
date, is refused naming the item's field. An item without a code bears no
tax.

The tax is calculated when the invoice is entered and kept with the item;
T75b posts revenue per item and output tax from the tax items
(ADR-0049 §5, §7).

A code whose component levies the subdivision its lines name (one output
code for every emirate, ADR-0049 §7) needs a jurisdiction per item, which
neither the item nor `ledger.Posting`'s lines carry yet (ADR-0049 §7 As
built says so of postings that build their own lines). The first version
refuses such a code at entry rather than enter a document it could not
post correctly. This is a gap for the UAE company, listed below. (Closed
by the product owner's decision of 2026-11-12: an item names its
jurisdiction; As built, "The emirate on the item".)

### 5. Posting: the customer's open item (T75b)
`POST /v1/customer-invoices/{id}:post` moves a draft to posted once, under
the row lock with a state guard; a replay answers the same. It posts
through `ledger.Postings.Post`: debit the customer's reconciliation
account with the partner, credit each item's account, credit output tax,
`source_context` `receivables`. The debit line is the customer's open item
with its payment terms and net due date (`Posting.Terms`, kept as for
supplier items): from the invoice, else the customer's company data. A
closed period is refused. The ledger's `:reverse` refuses the entry
(04 §4.9); a posted customer invoice is not reversed in the first version
and is corrected by a credit memo — plan default 3.

### 6. Incoming payments applied (T75c)
An incoming payment sitting on a house bank's incoming payments clearing
account (ADR-0047, T49h) is applied to a customer's open items **in full
and exactly**: the items applied, credit memos netted, equal the receipt
in its currency; one entry clears payment and items. No partial payment,
difference or discount — plan default 4. The application is a receivables
document (`receivables.incoming_payment`); the clearing is the ledger's.

The payment is a **receipt**: the journal entry a bank statement line
matched as incoming posted, whose credit line on the incoming account is
an open item (ADR-0047). The clearing entry offsets the receipt and each
item at the amounts they were booked at in all three currencies, as a
settlement offsets what it pays; what that leaves in local or group
currency, receipt and items having been booked at different rates, is a
realized exchange difference on the company's `fx_gain` or `fx_loss`
account. Credit items already open on the customer's account net by
themselves: they are items with negative amounts. An application is not
undone in the first version (plan, "Later").

### 7. Credit memos (T75d)
A customer credit memo is the same document with the kind `credit_memo`,
posted with the signs turned: credit the customer, debit each item's
account and output tax. Its amounts are never negative; the kind gives
their direction. It may name the invoice it credits
(`reference_invoice_id`): a posted invoice of the same company and
customer in its currency. The posted credit memos naming an invoice may
not together exceed that invoice's gross, checked at posting under the
invoice's row lock, as the product owner decided for supplier credit memos
(2026-11-10) — plan default 5, to confirm (question 7 of the plan). The
limit is the gross, not what is open of it, so a paid invoice is credited
too; a memo naming no invoice has no limit. Its tax is at its own date.
Its credit line is the customer's open credit item: it clears against the
invoice through the ledger's clearing, or nets when an incoming payment is
applied (§6).

### 8. Permissions and agents
`receivables.invoice.read`, `receivables.invoice.write` (entering) and
`receivables.invoice.post` (T75b); T75c adds `receivables.payment.apply`.
Reading the incoming payments applied and the receipts not yet applied
takes `receivables.invoice.read`: the plan names no read permission of
their own. None is an approval permission. Company access applies as everywhere: a
document of a company the caller may not act in is not found. Entering is
human-only in the first version (`x-agent: false`): no design gives an
agent or a service a part in entering customer invoices yet, and week 2's
billing enters them in process.

### 9. Events and audit
Every state change writes an audit record and publishes an event through
the context's outbox in the same transaction. `CustomerInvoiceChanged`
(T75a) is emitted when an invoice is entered, with the status before and
after; T75b adds `CustomerInvoicePosted`, emitted alone when an invoice
posts, T75c `IncomingPaymentApplied`.
Events carry the totals, not the items, and name the customer by id and
number, never by name, as the product owner decided for suppliers in
webhook events (2026-11-05).

### 10. The aging read (T75e)
`GET /v1/receivables-aging` is computed on read from the ledger's customer
open items (`ledger.OpenItems.Receivable`) with the bucket logic of
`ledger/aging.go`, exported, not copied; no table. It gives a company's
open customer items on a key date per customer and in total, not yet due
and in buckets of days overdue by its net due date (an item posted
without payment terms is due on its document date, product owner
2026-11-12), in the company's local currency; a credit
item reduces its customer's bucket and total; the totals are the open
items of the company's customer reconciliation accounts. It ages the
items open now; T75l adds the items open on the key date (`open_on`
`key_date`) and the aging by document date (`aging_basis`), T75m the
aging by profit center or segment and its CSV (As built).

## Alternatives
- **A leaf context reading everything through ports the composition root
  wires** (as assets, cash, closing): not taken. The plan asks for the
  payables pattern, T75b uses the ledger's posting types directly, and
  billing will depend on receivables, which a leaf forbids.
- **Customer invoices as journal entries only** (today's way, ADR-0049
  §7): keeps no document a credit memo can name or an AR accountant can
  list by customer and status.
- **Tax calculated when the invoice posts, not when it is entered**: the
  accountant would not see the totals of what they entered; a draft cannot
  be changed in the first version, so nothing goes stale between entry and
  posting but the configuration, which T75b decides how to treat.

## Consequences
- A new schema, three permissions and a document type for number ranges
  (`customer_invoice`); a company without that range cannot enter
  invoices (422 `config.no_number_range`). The country packs bring it
  from Canada 2026.9 and UAE 2026.6 (ADR-0048 As built); a company on an
  earlier version adds it by applying one of them or by a change request.
- Customer invoices and manual sales entries exist side by side; the
  pilot's billing integration keeps posting journal entries until billing
  is built.

## Defaults chosen (for the product owner's digest)
Plan defaults 1-5 above. Taken in T75a beside them:

1. The tax date is the invoice date; a person cannot set another in the
   first version.
2. Any account that can be posted in the company may carry an item, not
   revenue accounts alone.
3. An item's net amount is more than zero.
4. Entering is human-only (§8).
5. A code that levies its lines' subdivision is refused (§4).
6. The response names what it references: the company by code and name,
   the customer by number and name, each item's account by number and
   name and its centers by code (REVIEW.md, API-consumer classes).

## Questions
- Plan question 1 (tax specialist): is the document's date the right tax
  point for customer invoices and credit memos in Canada and the UAE?
- Plan question 7 (product owner): the credit memo limit for customers.
- **The emirate on a customer invoice's items** (product owner; scope):
  §4 refuses the UAE's one-code-for-every-emirate output codes. Entering
  UAE customer invoices needs a jurisdiction per item and on the posting's
  lines; not built, no leaf added. **Decided 2026-11-12** (product owner):
  the item carries the emirate; As built, "The emirate on the item".

## As built

### T75a Customer invoice entered (migration 000265)
- `internal/receivables` (`receivables.go`, `invoices.go`, `tax.go`,
  `api.go`), wired by `app.Receivables`; `problem.Invalid` and
  `currency.ParseAmount` moved to the platform out of payables, which
  calls them (its codes and answers unchanged); `internal/layout` allows its
  imports of `config`, `ledger`, `partner` and `tenancy`; `receivables` is
  in `outbox.Contexts` and among the tenant schemas `db/schema_test.go`
  holds to the design rules.
- Migration 000265: the schema with its outbox and inbox;
  `receivables.customer_invoice` (`customer_invoice_number_once` per
  company; `customer_invoice_totals`, gross = net + tax;
  `customer_invoice_amounts_not_negative`; kind `invoice` and status
  `draft` alone, which T75d and T75b widen; no update or delete for the
  application) and `receivables.customer_invoice_item` (a positive net
  amount, tax only under a tax code, insert-only by
  `platform.forbid_mutation`); the three permissions. Schema tests in
  `db/receivables_test.go`.
- `POST /v1/customer-invoices` (201 with `Location` and `ETag`,
  idempotent by `Idempotency-Key`, human-only), `GET /v1/customer-invoices`
  (the companies the caller may act in; filters `legal_entity_id`,
  `customer_id`, `status`, `document_number`, `invoice_date_from`,
  `invoice_date_to`; sortable by `invoice_date` and `document_number`;
  cursor paging), `GET /v1/customer-invoices/{id}` (404 for another
  company's).
- Refusals, each naming the request's own field where one caused it: an unknown customer 422
  `request.reference_not_found` (`customer_id`); one that is not active or
  has no customer role `receivables.customer_not_usable`; an account that
  cannot be posted `receivables.account_not_postable`
  (`items[i].account_id`); a tax code that is no output code of the
  company with a rate on the invoice date `receivables.unknown_tax_code`
  (`items[i].tax_code`); one levying its lines' subdivision
  `receivables.tax_jurisdiction_not_supported`; an unknown currency
  `receivables.unknown_currency`; an item in another currency
  `receivables.currency_mismatch`; an item of zero or less
  `receivables.amount_not_positive`; amounts out of range
  `receivables.amount_too_large`; the ledger's assignment refusals
  (`ledger.cost_center_required` and the others of `CheckAssignments`),
  checked on the posting date, else the invoice date; no fiscal year for
  the invoice date `ledger.no_fiscal_period`; no number range
  `config.no_number_range`. `receivables.items_required` guards the port
  against an invoice without items, which the contract refuses first.
- The customer's and company's names, the accounts' numbers and names and
  the centers' codes are read through `partner.Payees.Names`,
  `tenancy.Directory` and `ledger.AssetReads` (`AccountNames`,
  `CenterNames`), once per page.
- `CustomerInvoiceChanged` (`x-webhook`) and the audit record
  `receivables.customer_invoice.create`, in the entry's transaction.
- Walkthrough `step_customer_invoice` (step 48): Alex enters an invoice
  to the customer of `step_customer` (step 47, T74a; batch 66 joined the
  two), three items with GO5, and reads it back; an input tax code (GA, an adjustment) is refused naming its
  field; the customer has
  no open item. The pilot's settings (step 2) add the `customer_invoice`
  number range.
- **Decided by default:** the six defaults above. Not built, by the plan:
  posting, changing or cancelling a draft, payment terms on the invoice
  (T75b reads them), a tax date of its own, attachments and custom fields.

### T75b Customer invoice posted: the customer's open item (migration 000271)
- `internal/receivables/post.go`: `POST /v1/customer-invoices/{id}:post`
  (`receivables.invoice.post`, `If-Match`, idempotent by
  `Idempotency-Key`, human-only) posts a draft once through
  `ledger.Postings.Post`: Dr the customer's reconciliation account in the
  company with the customer, the gross; Cr each item's account with its
  net, tax code and centers; Cr output tax, one line per code and
  component on the component's account. The entry has the document type
  `DR` (number range `journal_dr`), the invoice's number as its reference,
  the invoice date as document and tax date and `source_context`
  `receivables`; the ledger derives its tax items from the lines and
  holds the tax lines to the codes (ADR-0049 §7), as for the pilot's
  sales entries.
- The invoice's row is read `FOR UPDATE` and only a draft posts (409
  `receivables.invalid_status`); a concurrent post waits and then meets
  another version (412). The entry, the invoice's status, the audit
  record `receivables.customer_invoice.post` and `CustomerInvoicePosted`
  (`x-webhook`) are written in one transaction.
- Migration 000271: `payment_term_code` (optional at entry),
  `net_due_date`, `journal_entry_year`, `journal_entry_id` on
  `receivables.customer_invoice`; the status `posted`;
  `customer_invoice_posted` (a posted invoice names its entry, posting
  date, term and due date; a draft no entry); `customer_invoice_entry_once`;
  the trigger `customer_invoice_posted_once` (only a draft changes, and
  only by being posted); the update grant on the columns posting sets.
  Schema tests in `db/receivables_test.go`.
- Terms: the term entered with the invoice (`payment_term_code` on
  `createCustomerInvoice`, a configured term, 422
  `receivables.unknown_payment_term` naming it), else the customer's in
  the company, counted from the invoice date. In the ledger `keepTerms`
  keeps `Posting.Terms` with customer open items as with supplier ones
  (`ledger.open_item_terms`), so `GET /v1/open-items` shows the item's
  `payment_terms` and ages it by its net due date.
- `ledger.OpenItems.Receivable(ctx, legalEntity, customers)`
  (`internal/ledger/receivable.go`): the legal entity's open items on
  customer reconciliation accounts in the leading ledger, of the customers
  named or of all, ordered by customer and item; each a
  `ledger.ReceivableItem` with its item reference, account, customer,
  document and posting dates, amount in transaction and in local currency
  with their exponents (debit positive), the terms it was posted with
  (nil without), its special G/L indicator, its entry's number and
  reference and the document that posted it; `NetDueDate()` and
  `DaysOverdue(on)` give its due date and days overdue, none without
  terms (since 2026-11-12 the document date without terms: As built,
  Items without payment terms). Cleared lines and lines of a reversed entry or a reversal are
  left out; company access is the caller's to check. T75e, T76a, T76c
  and T77a read it.
- Refusals at posting: a customer that is not active, no longer a
  customer, without company data in the company or blocked for posting
  there 422 `receivables.customer_not_usable`; a closed period 409
  `ledger.period_not_open`; tax the codes no longer give on the invoice
  date 409 `receivables.tax_changed`; a tax component without an account
  422 `receivables.no_tax_account`; the ledger's refusals of the entry.
  The ledger's `:reverse` refuses the entry, 409
  `ledger.entry_owned_elsewhere` (04 §4.9, as built before T75b for
  every context without a reversal path).
- The invoice shows `payment_term_code`, `net_due_date`,
  `journal_entry_id`, `journal_entry_year` and `journal_entry_number`
  (read through `ledger.OpenItems.Numbers`).
- Walkthrough `step_customer_invoice` (step 48) extended: Ruth posts the
  invoice; the entry's lines move 1200, 4000 and 2300 by the invoice's
  amounts; Northwind's open item shows with NT30 and its due date; a
  second post and the ledger's `:reverse` are refused. Step 2 adds the
  `journal_dr` number range.
- **Decided by default:** (1) the journal document type is `DR`, with its
  own number range `journal_dr` (SAP's customer invoice type, stated from
  memory, not checked against a source in the lane); (2) posting is
  human-only, as entering is; (3) an optional `payment_term_code` is
  entered with the invoice, since a draft cannot be changed; cash
  discount tiers of the term are kept with the item as information, and
  no discount is granted (plan, "Later"); (4) the tax is recalculated at
  posting and a difference refuses the post rather than posting other
  tax than the document shows; (5) in a foreign currency the tax lines'
  local amounts are the posting date's conversion, as a manual sales
  entry's are (ADR-0049 §7); (6) the posting emits `CustomerInvoicePosted` alone, not also
  `CustomerInvoiceChanged`.
- Known limit, the ledger's (ADR-0049 §7 As built): an output tax code
  whose components share one account is refused at posting, 422
  `ledger.output_tax_mismatch`, as a manual entry with it is.

### T75d Customer credit memo (migration 000276)
- `internal/receivables/kinds.go`: the kinds `invoice` and `credit_memo`.
  `POST /v1/customer-invoices` takes `kind` (absent: an invoice) and, on a
  credit memo, `reference_invoice_id`; no new operation or table. A kind
  that is neither is refused, 422 `receivables.unknown_kind`; a reference
  on an invoice, 422 `receivables.reference_not_allowed`; a reference
  that is not a posted invoice of the same company and customer in the
  memo's currency (a draft, a credit memo, another customer's, another
  company's, one that does not exist), 422
  `receivables.invalid_reference_invoice`; each names the request's field.
  A memo is entered as an invoice is: items more than zero, each item's
  tax what its output code gives at the memo's own date.
- `POST /v1/customer-invoices/{id}:post` posts a memo with the signs
  turned (`signed`): Cr the customer's reconciliation account with the
  customer, the gross; Dr each item's account, its net, with its tax code
  and centers; Dr output tax per code and component. The entry has the
  document type `DG` (number range `journal_dg`), the header text
  "Customer credit memo <number>", the memo's date as document and tax
  date and `source_context` `receivables`; the ledger derives its tax
  items from the lines as for an invoice, negative.
- **The limit** (`creditedInvoice`): posting a memo that names an invoice
  reads that invoice `FOR UPDATE` (ADR-0008) and sums the gross amounts of
  the posted credit memos naming it, in minor units; with the memo's own
  gross they may not exceed the invoice's gross (422
  `receivables.credit_exceeds_invoice`, naming `gross_amount`, with the
  problem's `credit_limit`: the invoice by id and number, its gross, the
  amount credited and the amount refused). The refusal is the one
  payables gives, made once for both contexts by
  `problem.CreditExceedsInvoice` (`internal/platform/problem`), which
  payables' `requireWithinInvoice` now calls too, its answer unchanged;
  each context keeps its own lock and sum. A refused memo stays a draft and does not count; of two
  memos posted at once that together exceed the invoice, the second waits
  for the lock, counts the first and is refused. The memo's row is locked
  before its invoice's and nothing locks them the other way round. A memo
  naming no invoice is not limited. Only the application enforces the
  limit, as for suppliers: it reads other rows, which a check constraint
  cannot.
- **Terms.** A memo's open item keeps the term entered with the memo,
  counted from the memo's date; without one, a memo naming an invoice
  keeps that invoice's term from that invoice's date, so it is due when
  its invoice is and ages with it (T75e); a memo naming none keeps the
  customer's term in the company from its own date.
- Migration 000276: `reference_invoice_id` on
  `receivables.customer_invoice` (`customer_invoice_reference_fkey` to a
  document of the tenant; `customer_invoice_reference_on_credit`, only a
  credit memo names one; `customer_invoice_reference_not_itself`); the
  kind `credit_memo`; the index `customer_invoice_by_reference`. The
  column is not among those posting may update, so a reference is fixed
  once entered. Schema tests in `db/receivables_test.go`
  (`TestCustomerCreditMemoRules`).
- The document shows `reference_invoice_id` and
  `reference_invoice_number`; `GET /v1/customer-invoices` filters by
  `kind` and `reference_invoice_id`. In the API a memo's amounts are
  positive, as the document keeps them. `CustomerInvoiceChanged` and
  `CustomerInvoicePosted` carry `kind` `credit_memo`,
  `reference_invoice_id` and `reference_invoice_number` (null without)
  and the memo's amounts **negative**, as payables' events carry a supplier credit memo's, so a
  consumer adding up `gross_amount` has what the customer owes; the audit
  records carry the kind.
- **Netting and clearing.** No ledger change was needed: a memo's credit
  line is an open item with a negative amount, which
  `ledger.Postings.ApplyReceipt` nets (T75c) and `createClearing` clears
  against the invoice when the two balance.
  `TestCreditMemoNetsAndClears` applies a receipt of 100.00 to an invoice
  of 119.00 and its memo of 19.00 together, exactly, and clears an invoice
  credited in full against its memo through `POST /v1/clearings`.
- Walkthrough `step_customer_credit` (step 55): Ruth enters and posts a
  second invoice to Northwind, CAD 2,100.00, and a credit memo naming it,
  CAD 420.00; the DG entry credits Northwind on 1200 and debits 4000 and
  2300; invoice and memo are Northwind's open items, CAD 1,680.00 net; a
  second memo of CAD 1,680.01 is refused at posting with the figures.
  Step 2 adds the `journal_dg` number range.
- **Decided by default:** (1) the journal document type is `DG` with its
  own range `journal_dg` (SAP's customer credit memo type, stated from
  memory, not checked against a source in the lane); (2) a memo draws its
  document number from the `customer_invoice` range, being the same
  document; (3) the terms rule above: payables gives a supplier credit
  memo naming an invoice that invoice's terms, which is followed; a
  supplier memo naming none has no terms and is due at once, which is not
  followed, since a posted customer document always names its term and due
  date (`customer_invoice_posted`, `CustomerInvoicePosted`): such a memo
  takes the customer's term from its own date; (4) events carry a memo's
  amounts negative, the API positive (above); (5) only credit memos count
  against the limit, there being no other credit kind, and none is
  reversed in the first version, so no credit is ever freed; (6) entering
  and posting a memo take the invoice's permissions and are human-only,
  as an invoice's are; (7) the list filters `kind` and
  `reference_invoice_id` were added so the credits of an invoice can be
  found (REVIEW.md, API-consumer classes).
- Not built, by the plan: debit memos, credit memos from returns and
  credit memo requests, paying a credit out, reversal.

### T75e AR aging (no migration)
- `internal/receivables/aging.go`: `GET /v1/receivables-aging`
  (`getReceivablesAging`, `receivables.invoice.read`; an agent tool, being
  a read) with `legal_entity_id` and `key_date` (required), `aging_days`
  (the overdue buckets' last days, ascending, at most six of at most 3650
  days; `30,60,90` by default, as `GET /v1/open-items` takes them),
  `customer_id`, `page_size` and `page_token`. It answers the company by
  id, code and name, the key date, the local currency, the days used, the
  `totals` (customers, items, amount, buckets) and one page of customers
  (`items`: id, number, name, items, amount, buckets). Every bucket is
  given in a fixed order, an empty one with no items and 0: `not_due`,
  each `overdue` bucket with `days_from` and `days_to` (the last without
  `days_to`), `no_due_date` (always empty since 2026-11-12: As built,
  Items without payment terms). Every amount is a `Money` in the company's
  currency.
- **The items** are `ledger.OpenItems.Receivable`'s, read once per
  request for the company (or the one customer): open lines on customer
  reconciliation accounts of the leading ledger, whoever posted them, so
  a manual or migrated entry's customer item counts as an invoice's does;
  cleared lines and reversed entries are left out, special G/L items
  kept. Amounts are the local-currency amounts the items were booked at,
  a debit positive: a credit memo or a payment on the account is negative
  and reduces its customer's bucket and total. The sums are checked
  (`money.Sum`); one out of range is refused, 422
  `receivables.amount_too_large`.
- **The buckets** are the ledger's (`internal/ledger/aging.go`, exported
  for this): `ledger.AgingDays` (the default and the ascending check that
  `listOpenItems` makes of `aging_days`), `ledger.AgingBucket` (an item's
  bucket from `ReceivableItem.DaysOverdue`: without terms it has no due
  date (since 2026-11-12 it is due on its document date: As built, Items
  without payment terms); due on the key date or later it is not due; else the bucket
  holding its days) and `ledger.AgingRange` (a bucket's name and days,
  which `listOpenItems`' own aging now reads too). The list's aging counts
  in SQL (`agingSelect`); `TestAgingBucketAsTheList` (ledger) holds
  `AgingBucket` to it over key dates on, before and after each bucket's
  edge, and `TestAgingTotalsAreTheOpenItems` (receivables) holds the
  report's totals, bucket by bucket, to `GET /v1/open-items` of the
  receivables account and to that account's open lines. So an item without
  terms, and an item due on the key date, are treated as `listOpenItems`'
  aging treats them (an item without terms no longer, since 2026-11-12:
  As built, Items without payment terms).
- **Paging.** One row per customer with open items, by customer id (the
  order the ledger reads them in), in cursor pages (`page_size`, at most
  500); the token is signed for the company, key date, days and customer
  filter, and refused with any other (400 `request.invalid_page_token`).
  The totals are those of every customer the request selects, on every
  page. Every page reads all the company's open customer items, since the
  totals need them.
- **The key date** is in the years 1900 to 2999, the contract's range of
  fiscal years (422 `receivables.key_date_out_of_range` naming
  `key_date`). `ReceivableItem.DaysOverdue` counts through a
  `time.Duration`, which holds about 292 years and stops there
  (`time.Time.Sub` saturates); the method was left as it is, since other
  contexts read it. The bound refuses a date no fiscal period can hold;
  it is not what keeps the buckets exact. With a bucket's last day at
  most 3650, an item's bucket is exact whatever the key date: anything
  292 years overdue is past every bucket, and anything 292 years before
  its due date is not due (`TestAgingFarDates`).
- **Budget** (01 §1.3, a new row): the aging of one company over 100,000
  open customer items, p95 3 s, as the liquidity forecast over a
  company's open items has; `TestReceivablesAgingBudget` seeds 2,000
  customers with 50 items each, half with terms.
- Other refusals: days that do not ascend 400
  `request.invalid_parameter` (`aging_days`); a page size out of range
  400 `request.invalid_page_size`; a company the caller may not act in
  422 `request.reference_not_found` (`legal_entity_id`).
- Walkthrough `step_ar_aging` (step 56): Ruth reads Maple's aging on the
  day Northwind's invoice and credit memo of step 55 are due (not due,
  CAD 1,680.00 in two items) and fifteen days later (1 to 30 days
  overdue); the rows and the buckets add up to the totals, which are the
  open items of account 1200 as `GET /v1/open-items` ages them; a key date
  in the year 3000 is refused. The key dates are counted from the
  invoice's date, never from the day the step runs.
- **Decided by default:** (1) the permission is `receivables.invoice.read`,
  as for T75c's reads: the plan names none and a new one needs a
  migration; (2) the buckets' parameter is `aging_days`, the name and
  limits `GET /v1/open-items` uses; (3) every bucket is given, empty ones
  too, so a consumer has the same columns for every customer; (4)
  customers are ordered by id, not by number or name: the order is stable
  across pages and needs no read of every customer's name; (5) the key
  date's bound above; (6) the budget's volume and figure above, 01 naming
  none for receivables; (7) a customer with open items that net to zero
  still has a row.
- Not built, by the plan: aging as at a past date, by document date, by
  profit center or segment, in group currency, export, analytics metrics,
  an allowance for expected credit losses.

### T79b An invoice entered by billing names its source (migration 000296)
- Week 2's billing posts through this context (§1, §2; ADR-0067 §9):
  sales calls `Endpoints.CreateInvoice` and `PostInvoice` in process, in
  the billing document's transaction. `Input.Source` (`Source{ID,
  Number}`) names the billing document; the invoice keeps it
  (`source_document_id`, `source_document_number`, both or neither,
  `customer_invoice_source`; one invoice per document,
  `customer_invoice_source_once`) and shows it on `CustomerInvoice`.
  `createCustomerInvoice`'s request takes no source, so an invoice a
  person enters names none. Nothing else of the context changed: the
  checks, the tax, the posting and the events are T75a's and T75b's.
  Receivables does not import sales.

### T85d A credit memo entered by sales bears the tax it is given (no migration)
- Sales posts an approved credit memo request as a customer credit memo
  in process (ADR-0067, As built T85d): `CreateInvoice` with the kind
  `credit_memo`, `reference_invoice_id` the billing document's invoice,
  `Source` the request, and `PostInvoice`. Each `ItemInput` may give its
  `Tax` (`givenTax`, `invoices.go`): the share of the tax the credited
  invoice's item bore, so an invoice credited in parts gives back exactly
  the tax it bore (product owner 2026-11-12). Only a credit memo entered
  from a source document and naming its invoice gives a tax, zero or
  more and zero without a code; anything else is a caller's error (500),
  the API's request having no such field.
- Posting a credit memo entered from a source document (`given`,
  `tax.go`) posts the tax it was entered with: each item's tax is shared
  out among its code's components as the calculator splits it
  (`shareGiven`, `money.Allocate`) and summed per code and component; it
  is not compared with what the code gives again, and a code that gives
  no tax at all for an item with tax refuses it (409
  `receivables.tax_changed`). The ledger holds the tax lines to the
  code's rate on the memo's date within a minor unit per line (ADR-0049
  §7). Every other document is calculated and checked as T75a and T75b
  built it.
- Terms, limit, events and clearing are T75d's: without a term of its
  own the memo keeps the invoice's from the invoice's date; the limit is
  the invoice's gross; the memo is the customer's open credit item.
  Tests: `TestCreditMemoWithGivenTax` (`source_test.go`).

### T75c Incoming payment applied to a customer's invoices (migration 000272)
- **The receipt.** A statement credit matched `incoming: true` posts Dr
  the house bank's main account, Cr its `incoming_gl_account`, with
  `source_context` `payments` (T49h). That open credit line is the
  receipt. No existing statement read says whether it is still open:
  `GET /v1/bank-statements/{id}/matches` names each match's posting, not
  whether accounts receivable cleared it. So `GET /v1/unapplied-receipts`
  was built: a company's receipts not yet applied, by posting date, each
  with its entry's number and header text, its amount, the account's
  number and name, the house bank's code and the bank statement and line
  that posted it (`bank_statement_id`, `bank_statement_ref`,
  `bank_statement_line_no`: `GET /v1/bank-statements/{id}` shows that
  line with the payer's references and remittance text); filters
  `house_bank_code`, `currency`, `posting_date_from`, `posting_date_to`;
  cursor paging. The statement line is read through a port receivables
  declares (`Statements`) and the composition root wires to
  `payments.StatementPostings` (`internal/payments/statementpostings.go`:
  the line whose match posted an entry, by `statement_match`'s journal
  entry); the incoming payment shows the same three fields of its
  receipt. A receipt on an account the configuration no longer names as
  a house bank's `incoming_gl_account` is neither listed nor applied
  (422 `receivables.receipt_not_open`). Receivables may not
  import payments (`internal/layout`), so it reads the incoming accounts
  from the configuration (`house_bank`, `house_banks[].incoming_gl_account`)
  and the open lines from the ledger (`ledger.OpenItems.Receipts`,
  `internal/ledger/receipt.go`): the leading ledger's open credit lines
  on those accounts of entries the payments context posted, one line per
  entry.
- **The ledger's port**, `ledger.Postings.ApplyReceipt`
  (`internal/ledger/receipt.go`): the receipt's line and the customer's
  items are read in one `loadItems`, under one sorted set of entry locks
  taken before any number range (`lockEntries`, ADR-0008), so a
  reversal or a reservation of any of them finishes first or waits, and
  the application cannot deadlock with a payment run or a manual
  clearing on those locks. `accountItems` (bank postings) was split into
  the lookup of an entry's lines on an account, which takes no lock
  (`accountLines`), and the load, so the application locks once. The
  receipt must be the entry's one line on the account, of the leading
  ledger, a credit, not reversed, not cleared (422 `ledger.not_a_receipt`,
  409 `ledger.entry_reversed`, `ledger.item_already_cleared`). Each item
  passes the check every use of open items shares (`checkItem`: a line on
  a reconciliation account of the company, not reversed, not cleared, no
  special G/L item, not reserved) and is on a customer reconciliation
  account with the customer as its partner (422
  `ledger.not_the_customers_item`) and in the receipt's currency (422
  `ledger.mixed_items`). Every refusal of an item names the request's
  own field, `items[i]`, an account that can no longer be posted
  included (422 `ledger.account_not_postable`: the item's by its place,
  the receipt's and the exchange difference's by name, never the entry's
  line). `offsetItems` offsets them all at their
  original amounts; the transaction-currency total must be zero
  (422 `ledger.clearing_not_balanced`, naming the receipt and the
  difference); `fxLines` books the local and group totals as the
  exchange difference (422 `ledger.no_fx_account` without the account),
  with the items' shared trading partner (ADR-0059 §4). The entry posts
  to the ledgers every item was posted in and `clearWithEntry` clears
  receipt, items and their copies (ADR-0045), publishing the ledger's
  `items_cleared`. Two applications of one receipt or item do not wait
  for each other: the second to mark its lines cleared is refused
  (`clearing_item_pkey`, 409 `ledger.item_already_cleared`) and rolls
  back whole. Where two clearings mark the same lines in different
  orders, PostgreSQL ends one as a deadlock victim, answered 503 with
  `Retry-After` as everywhere (`problem.From`); the retry finds the lines
  cleared.
- **The document**, `internal/receivables/incoming.go` and
  `incoming_api.go`: `POST /v1/incoming-payments`
  (`receivables.payment.apply`, idempotent by `Idempotency-Key`,
  human-only) takes the company, the customer, the receipt's journal
  entry, an optional posting date (else the company's date in its time
  zone) and the items by (fiscal year, journal entry, line). It finds the
  receipt among the company's unapplied receipts (422
  `receivables.receipt_not_open` naming `receipt_journal_entry_id`; 409
  `receivables.receipt_already_applied` naming the payment that applied
  it), calls the ledger, then numbers the document from the company's
  `incoming_payment` range of the posting date's fiscal year and writes
  it with its items, each at the amount the ledger cleared it at. The
  ledger is called before the number is taken, so the entries' locks come
  before every number range; the clearing entry therefore carries no
  reference, and names the payment as its `source_document_id` and the
  customer's number in its header text. The entry has the document type
  `DZ` (number range `journal_dz`) and `source_context` `receivables`;
  the ledger's `:reverse` refuses it (04 §4.9).
  `GET /v1/incoming-payments` (the companies the caller may act in;
  filters `legal_entity_id`, `customer_id`, `document_number`,
  `posting_date_from`, `posting_date_to`; sortable by `posting_date` and
  `document_number`; cursor paging) and `GET /v1/incoming-payments/{id}`
  (404 for another company's) show the company by code and name, the
  customer by number and name, the receipt and the clearing entry by
  number, the incoming account by number and name, and each item with
  its entry's number, its reference (a customer invoice's number) and its
  amount with the currency.
- Migration 000272: `receivables.incoming_payment`
  (`incoming_payment_number_once` per company;
  `incoming_payment_amount_positive`; `incoming_payment_receipt_once`, a
  receipt is applied once; `incoming_payment_entry_once`) and
  `receivables.incoming_payment_item` (`incoming_payment_item_once`, an
  open item is applied once; a non-zero amount), both insert-only
  (`platform.forbid_mutation`, no update or delete grant); the permission
  `receivables.payment.apply`. Schema tests in `db/receivables_test.go`.
- `IncomingPaymentApplied` (`x-webhook`: the customer by id and number,
  the amount, the receipt's and the clearing entry's ids, the number of
  items) and the audit record `receivables.incoming_payment.apply`, in
  the application's transaction, beside the ledger's two
  (`ledger.journal_entry.post`, `ledger.clearing.create`).
- Walkthrough `step_incoming_payment` (step 54): RBC's third statement of
  the day carries Northwind's CAD 1,595.05; Tom matches the line as
  incoming and posts it; Ruth finds it among the unapplied receipts and
  applies it to the invoice of step 48; the DZ entry debits 1015 and
  credits Northwind on 1200; Northwind has no open item and nothing waits
  on 1015; a supplier named as the customer and a second application are
  refused. Step 2 adds the `incoming_payment` and `journal_dz` number
  ranges.
- **Decided by default:** (1) `GET /v1/unapplied-receipts` is needed and
  built (above); (2) a receipt is an entry the payments context posted
  (a matched statement line); a credit another context posted to an
  incoming account is not offered (receipts without a statement are in
  the plan's "Later"); (3) reads take `receivables.invoice.read`; (4)
  the journal document type is `DZ` with its own range `journal_dz`
  (SAP's customer payment type, stated from memory, not checked against
  a source in the lane); (5) applying is human-only, as entering and
  posting are; (6) the customer is not checked for a posting block: a
  blocked customer may still pay what it owes, and the items are its own
  by the ledger's check; (7) the ledger call precedes the document's
  number, so the clearing entry has no reference (above); (8) the
  document has no version of its own: it shows `version` 1 and
  `updated_at` its creation, since it never changes; (9) a receipt and
  a payment name the statement and line that posted the receipt, not the
  line's text or the payer's references: those are the payments
  context's to show, under its own permission
  (`GET /v1/bank-statements/{id}`).
- Not built, by the plan: partial payments, residual items, payments on
  account, differences and tolerances, cash discount granted, one receipt
  for several customers, a receipt in another currency than the items,
  undoing an application, rule or agent matching, receipts without a
  statement, direct debits, refunds. Credit memos are T75d; the netting
  of a customer's credit items is built and tested here with credits
  posted through the ledger's port.

### Items without payment terms (po:2026-11-12, no migration)

- **Rule** (product owner 2026-11-12): a customer open item posted
  without payment terms (a manual or migrated entry's) is due on its
  document date, so it is not due on that date and overdue from the day
  after. It replaces what T75b and T75e built, where such an item had no
  due date.
- **One place.** `ledger.ReceivableItem.NetDueDate()` returns the terms'
  net due date or, without terms, the document date; `DaysOverdue(on)`
  counts from it. Both lost their `ok` result: every reader of customer
  items (the AR aging here, dunning and the collection worklist in
  ADR-0065) asks the item and none looks at `Terms` for a due date. The
  credit exposure (T77a) does not read due dates and is unchanged.
- **The AR aging** puts an item without terms in `not_due` or in the
  overdue bucket of its days past its document date. A credit without
  terms ages the same way, negative in its bucket, as a credit with
  terms always did. Since every item now has a due date, the report's
  `no_due_date` bucket is always empty. Decided by default: it is still
  given and stays in `ReceivablesAgingBucket`'s open enumeration, because
  taking a value out of a response is a breaking change (`cmd/enumcompat`,
  `api/breaking-accepted.txt`) that this fix does not need; dropping it is
  the product owner's to ask for.
- **Not changed:** `GET /v1/open-items` (its `due_date_to` filter, its
  `payment_terms` and its aging in SQL, `agingSelect`) shows an item
  without terms, a customer's too, without a due date: it lists supplier
  and customer items as posted and shares no code path with the readers
  above. The report and the list therefore differ for a customer item
  without terms, and only there: `TestAgingTotalsAreTheOpenItems` holds
  the two together bucket by bucket with that one item moved, and
  `TestAgingBucketAsTheList` holds `ledger.AgingBucket` to the list and
  shows the reader's document-date ageing beside it. Supplier items
  (`PayableItem`, due by the supplier's current terms without their own)
  are unchanged.
- **Walkthrough.** No expectation of steps 53 to 58 changed: none of
  their customer items is posted without terms (full run, 59 steps).

### The emirate on the item (product owner 2026-11-12; migration 000303)
- A customer invoice's item names the subdivision its tax is levied in,
  `tax_jurisdiction` (ISO 3166-2, `AE-DU`), as a journal line does
  (ADR-0049 §7): the field, its name and its rules are the ledger's, held
  by the same check (`ledger.TaxCode.CheckJurisdiction`, which the
  ledger's lines and receivables' items share). An item whose output
  code's component levies its lines' subdivision (the UAE pack's `S5`, one
  code for every emirate) must name one, 422
  `receivables.tax_jurisdiction_required`; it is a subdivision of the
  component's country and, where the component lists the subdivisions it
  accepts (the emirate guard), one of them; an item of any other code, or
  of none, names none, 422 `receivables.tax_jurisdiction_not_levied`. Each
  refusal names `items[i].tax_jurisdiction`. A value that has not the
  form of a subdivision's code is refused by the contract's pattern, 400
  `request.invalid_body`. `receivables.tax_jurisdiction_not_supported` is
  gone with the refusal it named; §4's last paragraph and "Defaults
  chosen" 5 no longer hold.
- `receivables.customer_invoice_item.tax_jurisdiction` (000303): nullable,
  only with a tax code and in the form of a subdivision's code
  (`customer_invoice_item_tax_jurisdiction`; `TestCustomerInvoiceItemJurisdiction`).
  That the code levies it is the context's check when the invoice is
  entered and again when it posts (a draft whose jurisdiction the
  configuration no longer accepts is refused then and entered again), and
  the ledger's on the posted lines.
- Posting: the item's line carries its code and jurisdiction
  (`ledger.PostingLine.TaxJurisdiction`, new on the posting port), and the
  output tax posts one line per code, jurisdiction and component
  (`taxGroups`), so the entry's tax items are per emirate and the VAT
  201's boxes 1a-1g select them as they select a manual sales entry's. A
  credit memo posts the same with the signs turned.
- **Decided by default:** (1) the invoice's tax stays its code's, rounded
  as the company's tax settings say over the whole invoice and shared out
  over the items (§4); a code's tax in an emirate is the sum of its
  items' shares there, not a calculation of its own, so the emirates' tax
  adds up to the invoice's `tax_amount` exactly. The ledger holds each
  emirate's tax lines to its base lines within one minor unit per base
  line (ADR-0049 §7), which shares keep, each being its item's exact tax
  rounded down or up. (2) The jurisdiction is entered, not derived from
  the customer's address or the company's establishment: ADR-0049 §11
  reads boxes 1a-1g by the emirate each sale names, and nothing in the
  configuration says which establishment made a sale. (3) The events
  carry totals, not items (§9), so neither names the jurisdiction. (4)
  The down migration drops the column: a posted invoice keeps its
  jurisdictions on its journal lines; a draft naming one is refused by
  the previous version's posting, which refuses its tax code as it did
  at entry (422 `receivables.tax_jurisdiction_not_supported`).
- Rolling upgrade (06 §6.4): 000303 is additive. An instance of the
  previous release still refuses such a code at entry and at posting
  (422 `receivables.tax_jurisdiction_not_supported`) and shows no
  `tax_jurisdiction`: a draft naming one waits for the upgrade, and
  nothing posts in the wrong emirate.
- Tests: `TestItemJurisdiction`, `TestPostJurisdictions`,
  `TestPostJurisdictionNoLongerLevied`, `TestRefusals` (receivables);
  `TestCustomerInvoiceItemJurisdiction` (db). The walkthrough's customer
  invoice step is the Canadian company's, whose codes levy no line's
  subdivision, and the UAE company (step 41) has no customer set up, so
  no walkthrough step shows it: tests only.
- Not built: the emirate on a sales document's items and on the customer
  invoice a billing document posts (ADR-0067, Questions). Quotations and
  sales orders still refuse such a code, 422
  `sales.tax_jurisdiction_not_supported`, so the invoice T79b's posting
  enters (batch 71: merged after it) has no item that must name a
  jurisdiction and names none; passing the emirate through sales is a
  later slice. (Built in batch 85: ADR-0067 As built, the emirate on a
  sales item; the item's check is `ledger.TaxCode.RefuseItemJurisdiction`,
  shared with sales, its refusals and fields unchanged.)

### The emirate's default on a sales item (product owner 2026-11-21; migration 000431)
The product owner's answer of 2026-11-21 to ADR-0067 As built (the
emirate on a sales item, decided by default 1), src `po:2026-11-21`:
a UAE sales item's emirate defaults from the seller's side, never the
customer's or the ship-to address, as the VAT 201 reports a supply by
the emirate of the supplier's establishment most closely connected with
it (ADR-0049 §11). "Decided by default" (2) above still holds for a
customer invoice entered in receivables: its item names the emirate.
- A quotation's, an order's, an order change's new or a captured item
  whose code levies the UAE's emirates (`S5`: the component's country is
  `AE`) and that names no `tax_jurisdiction` takes, when it is entered
  (`sales` `defaultJurisdiction`): for an item of a material kept in
  stock of an order with a plant, the plant's `region` (ADR-0082 As built
  S2j, the plant required); else the company's establishment, the
  `region` of its postal address in the configuration (`companies[].
  postal_address`, which E2 renders as the seller's address). A default
  its code does not levy (no region, or one outside the code's
  subdivisions) is none, and the item is refused as before (422
  `sales.tax_jurisdiction_required`).
- It stays editable: an item may name another emirate, which it keeps
  and is held to its code as before. The document's audit record lists
  each item that had a default (`tax_jurisdiction_defaults`: the item,
  its default, the jurisdiction it was entered with and `overridden`
  when they differ; ADR-0011), so a change from the default is audited.
  An item's jurisdiction is fixed once entered (000420), so a later
  change of the plant's region or of the company's address changes no
  item.
- **Decided by default:** (1) the default is the UAE's alone: another
  country's codes that levy their lines' subdivision (none in the packs
  today) keep the jurisdiction entered, the product owner's decision
  being about the VAT 201; (2) a quotation has no plant, so its stock
  items take the company's establishment, and an order from it keeps its
  items as offered (ADR-0067 §5); (3) an order change's kept items keep
  theirs, its new items default from the order's plant.
- For the tax specialist (Musto Ibrahim, UAE VAT signer): confirm in one
  line "the plant's emirate, else the company's establishment" as the
  default (product owner 2026-11-21; a default, editable, so the build
  did not wait), and that an order from a quotation keeps its items'
  emirates as offered, the establishment's for a stock item even when
  the order has a plant (decided by default 2; re-defaulting would need
  the quotation item to record that it defaulted). **Confirmed by Musto
  2026-11-21** (relayed by the product owner): the item emirate default
  (the plant's, else the company's establishment) and a quotation keeping
  its emirates.
- Tests: `TestItemJurisdictionDefaults` (sales), `TestOrgUnitRegion`
  (tenancy, db). Walkthrough step 148.

### T75f Partial payment: residual items and payment differences within tolerance (migrations 000350, 000351)
The first variant of §6 (product owner 16–17 November 2026: variants
start; `plan-depth-1.md` §T75f). §6's "in full and exactly" stays the
rule of a company without a tolerance and of an item without a residual.

- **Residual items** (SAP's residual item of F-28, from memory): each
  item of `POST /v1/incoming-payments` may name a `residual`, what stays
  open of it: an amount in the receipt's currency, more than zero and
  less than the item, which must be a debit (an invoice; what is left of
  a credit is not a residual), and a reason `short_payment`, `disputed`,
  `deduction` or `other` (an open enumeration) with an optional free
  text. The ledger (`ledger.Postings.ApplyReceipt`,
  `internal/ledger/receiptdiff.go`) clears the item in full, as before,
  and posts the residual as a new debit line of the clearing entry on the
  item's account with the customer, at the **item's own rate** (its local
  and group amounts the item's, prorated), so the part still owed
  realizes no exchange difference; its text names the residual and its
  reason. The residual takes the item's terms (`ledger.open_item_terms`:
  code, baseline date, net days) **without cash discount tiers**, so it
  falls due when the item did and earns no second discount; an item
  without terms leaves its residual without, due on the application's
  date as any customer item without terms is. The payment names each
  residual by its line in the clearing entry (`incoming_payment_item`'s
  residual columns); `GET /v1/open-items` lists it while it is open, and
  aging, dunning and the collection worklist read it as any customer item.
- **Payment differences** (SAP OBA3's customer tolerance group, from
  memory): what the items, net of their residuals, differ from the
  receipt by is written off when it is within the company's
  `companies[].receivables.payment_tolerance` for its direction: an
  amount in the company's currency and a percent of the items applied
  (net of residuals), **the lower binding**; both set together or not at
  all, and a direction allowing a difference needs its account
  (configuration validation). An underpayment is debited to the account
  determination's `payment_difference_expense`, an overpayment credited
  to `payment_difference_revenue` (422 `ledger.no_payment_difference_account`
  without it), at the **receipt's rate**; anything beyond the tolerance is
  refused as before (422 `ledger.clearing_not_balanced`, the message now
  giving the difference and its direction). The payment shows the
  difference (`difference`, positive an underpayment); the configuration
  change is tier 3 (it decides what is written off to profit and loss).
- **Events and audit:** IncomingPaymentApplied carries `residual_count`
  and `difference` (optional fields, so a consumer of v1 is unaffected);
  the audit record carries both.
- **Schema** (000350): `incoming_payment.difference` (default 0) and the
  residual columns of `incoming_payment_item`, all together or none,
  checked; the two account determination keys (added NOT VALID, validated
  by 000351). The down migration is refused while a residual or a
  difference exists.
- **Decided by default:** (1) a residual only on a debit item; (2) the
  residual at the item's rate, the difference at the receipt's; (3) the
  residual's terms copied without discount tiers; (4) the tolerance's
  percent is of the items applied net of their residuals, and both limits
  must allow the difference (the lower binds), as SAP's tolerance group
  does (from memory); (5) a tolerance of zero or none allows nothing,
  keeping T75c's exact rule; (6) reason codes are a fixed open
  enumeration, not configuration (configurable reasons stay in the
  "later" leaf); (7) the difference is written off gross: **no output
  tax is corrected on it** (specialist question below); (8) the payment
  tolerance is a tier 3 configuration change.
- **For the specialist:** whether a payment difference written off on a
  taxable invoice must reduce the output tax (a tax adjustment as a cash
  discount granted would make) in Canada (GST/HST) and the UAE (VAT); the
  first version posts none. Not built: cash discount granted (built for
  the UAE since: As built, cash discount granted), configurable
  reason codes.

### T75g Payment on account and its later application (migration 000352)
The second variant of §6 (`plan-depth-1.md` §T75g).

- **On account at the payment.** `POST /v1/incoming-payments` takes
  `on_account`: what the receipt leaves after its items, the whole
  receipt when it names none (the items may then be empty), is posted to
  the customer's reconciliation account from its company data (T74a; 422
  `receivables.customer_not_usable` without it) as a credit of the
  customer's, a line of the payment's clearing entry, at the **receipt's
  rate**, so a receipt left whole on account realizes no exchange
  difference. With `on_account` an overpayment is left on account rather
  than kept within the tolerance; something must be left (422
  `receivables.nothing_left_on_account`). The credit is an ordinary open
  item of the customer (SAP's payment on account, from memory): the open
  items list, the aging and the credit exposure net it.
- **The later application** (`POST /v1/incoming-payments/{id}/applications`,
  `receivables.payment.apply`, idempotent, human-only): the payment's
  credit still open, its own or the one its last application left, is
  applied through the same ledger port as a receipt
  (`ReceiptApplication.Credit`: the receipt is that line, on the
  customer's account, the customer's), with residuals and the
  underpayment tolerance as T75f; what the items leave stays on account
  as a new credit line of the application's entry, at the credit's rate.
  The credit is checked open first (`ledger.OpenItems.Closed`, a read
  that takes no lock, so the entries' locks are taken once, in the
  ledger's one sorted call before any number range; 409
  `receivables.no_credit_on_account` when it is all applied or was
  cleared otherwise, a manual clearing or netted by another payment);
  two applications racing for one credit both pass that read and the
  ledger refuses the second when it marks the credit cleared (409
  `ledger.item_already_cleared`), which rolls back whole: no lock is held
  across anything but the request's transaction. The application is a
  row of `receivables.incoming_payment_application`, numbered in the
  payment from 1, with the credit it consumed, what it wrote off and left;
  its items join the payment's with its number (`application_no`, 0 for
  the payment's own). Its entry is `DZ`, source document the payment.
- **The payment's version** moves with each application (1 + their
  number; `updated_at`/`updated_by` the last one's); the representation
  adds `on_account` (what was left, what is open now and the open
  credit's line, read from the ledger, so a credit cleared elsewhere
  shows nothing open) and `applications`. IncomingPaymentOnAccountApplied
  (`x-webhook`) and the audit record
  `receivables.incoming_payment.apply_on_account` are written with each
  application; IncomingPaymentApplied carries `on_account_amount`.
- **Decided by default:** (1) the credit goes to the customer's
  reconciliation account of its company data, not to a special G/L
  account (SAP's on-account posting without special G/L, from memory);
  (2) receipt's rate for the credit, credit's rate for what an
  application leaves; (3) an application writes off underpayments within
  the tolerance but never an overpayment, which stays on account; (4) a
  customer blocked for posting may still pay on account, as T75c lets it
  pay; (5) an application has no number range of its own: its clearing
  entry's number and the payment's identify it; (6) one credit open per
  payment at a time, applied in order.
- Not built: refunds of a credit on account, undoing an application,
  one receipt for several customers (the placeholder's "later").

### T75h One receipt for several customers (migration 000363)
A variant of §6 (`plan-depth-2.md` §T75h): one bank receipt applied to
the open items of more than one customer, as a parent pays its
subsidiaries' invoices (SAP's other accounts of F-28, from memory, not
checked against a source in the lane).

- `POST /v1/incoming-payments` takes `other_customer_ids`: the other
  customers whose items the receipt pays besides `customer_id`, which
  stays the payer, the payment's customer (its number heads the clearing
  entry's text, its account takes what is left `on_account`, and its
  credit on account is applied to its own items only, T75g). Each is a
  business partner (422 `request.reference_not_found` naming
  `other_customer_ids[i]`), not the payer (422
  `receivables.duplicate_customer`), named once (the contract's
  `uniqueItems`, 400), and has at least one of the items (422
  `receivables.customer_without_items`, checked on what the ledger
  applied, so the posting rolls back). The ledger
  (`ReceiptApplication.OtherCustomers`) accepts an item of any of them in
  `checkCustomerItem` (422 `ledger.not_the_customers_item` otherwise, as
  before) and returns each item's customer (`AppliedReceipt.Customers`).
  One DZ entry clears the receipt and every customer's items; a residual
  item is a line of its item's customer (it took the item's partner
  already), the payment difference is no customer's.
- The payment shows each item's customer by id, number and name
  (`IncomingPaymentItem.customer_*`) and `customers`: each customer whose
  items it applied with the receipt and its part, those items net of
  their residuals (`IncomingPayment.Parts`, `money.Sum`), the payer first,
  with zero when none of the items was its own, then the others in the
  order of their first item. The parts, less `difference` and with what
  was left on account, are the receipt. `customer_id` on
  `GET /v1/incoming-payments` also finds a payment that paid that
  customer's items for another. IncomingPaymentApplied carries
  `customers` (id, number and part, never the name: product owner
  2026-11-05), optional so a v1 consumer is unaffected; the audit record
  carries them too.
- Schema (000363): `incoming_payment_item.customer_id`, nullable: rows
  written before it, or by the previous version during a rolling upgrade
  (06 §6.4), are the payment's customer's and are read as
  `coalesce(customer_id, payment's customer_id)`. Indexed with the
  payment for the list's filter. The down migration is refused while an
  item names another customer than its payment's.
- **Decided by default:** (1) the payer is the request's `customer_id`,
  unchanged in meaning, so a payment of one customer is as before; (2)
  the other customers need not be set up in the company: their items are
  open items on customer reconciliation accounts, which is the ledger's
  check, as for the payer; (3) what is left on account is the payer's,
  never a subsidiary's; (4) an application of a credit on account (T75g)
  stays with the payer's items; (5) no relationship between the
  customers is required (a head office or alternative payer in the
  customer master is not modelled; SAP's alternative payer, from memory,
  stays later).
- Tests: `TestReceiptForSeveralCustomers`, `TestSeveralCustomersOnAccount`,
  `TestSeveralCustomersRefusals` (receivables); `TestSeveralCustomersRevert`
  (db). Walkthrough `step_receipt_several_customers` (step 116).

### T75i Undoing an application (migration 000364)
A variant of §6 (`plan-depth-2.md` §T75i): an incoming payment applied to
the wrong items, or an application of its credit on account (T75g), is
undone, as SAP's FBRA resets cleared items and reverses the clearing
document (from memory, not checked against a source in the lane). §6's
"an application is not undone in the first version" no longer holds.

- **The ledger's reset** (`ledger.Postings.ResetClearing`,
  `internal/ledger/clearingreset.go`): the clearing a clearing entry made,
  and its copies' in the other ledgers (clearCopies), is reset: its
  reset is recorded in `ledger.clearing_reset`, whose trigger
  (`clearing_reset_moves_items`, security definer as `erp_owner`, under
  row-level security) keeps its `ledger.clearing_item` rows in
  `ledger.clearing_reset_item` and deletes them; the clearing item's own
  trigger (`ledger.clearing_item_reset_only`, replacing
  `platform.forbid_mutation` there) allows a delete only of a reset
  clearing (or with the erasure of its tenant), and the application
  deletes from no tenant table and writes no reset item itself; every reader of open items (the
  `ledger.open_item` view, `OpenItems.Receivable`, `Receipts`, `Closed`,
  `loadItems`, the analytics snapshot) reads "not in `clearing_item`" and
  sees the lines open again unchanged. A line is still cleared at most
  once at a time (`clearing_item_pkey`). The clearing entry is then
  reversed by the ledger's reversal (`reverse`, in every ledger of its
  posting group, `ledger.reversal`), on its own posting date: a period
  closed since refuses the reset with the posting's problem (409
  `ledger.period_not_open`) and nothing changes. The lines the entry
  posted (a residual item, a credit on account, the offsets) are of a
  reversed entry, no open items, and the reversal clears its own against
  them as any reversal does. `GET /v1/clearings/{id}` shows a reset
  clearing with the lines it had cleared and `reset_at`, `reset_by`;
  ClearingReset (`x-webhook`) names the lines opened again; the audit
  record is `ledger.clearing.reset`.
- **Locks and refusals under them:** the entries of the clearing's lines
  are read without a lock to name them, then locked in one sorted call
  (`lockEntries`) before anything else, as an application and a reversal
  of any of them take theirs, and read again under the locks: a line of
  the entry cleared by another clearing since (a residual item or a credit
  on account applied) refuses the reset, 409 `ledger.clearing_lines_cleared`
  (undo that one first), a clearing reset already 409
  `ledger.clearing_already_reset` (`clearing_reset_pkey` maps to it as a
  backstop); an entry that made no clearing is 422
  `ledger.not_a_clearing_entry`. No lock is held across anything but the
  request's transaction.
- **Receivables:** `POST /v1/incoming-payments/{id}:reset` undoes the
  payment's own application of its receipt, once every application of its
  credit is undone (409 `receivables.applications_in_force`);
  `POST /v1/incoming-payments/{id}/applications/{application_no}:reset`
  undoes the latest application in force (409
  `receivables.application_not_latest`; 404 for a number the payment has
  not). Each once (409 `receivables.already_reset`;
  `incoming_payment_reset_once` maps to it), with `If-Match` the payment's
  version (412) and a reason of 1 to 200 characters, not white space
  alone; `receivables.payment.apply`, human-only, idempotent. What may be
  undone is read without a lock; the ledger's check under the locks is
  what holds (write skew: an application or a reset committed meanwhile
  is refused there and the request rolls back). A row of
  `receivables.incoming_payment_reset` records the undoing with the
  reversal; the payment's `status` is `reset` once its own application is
  undone (`applied` before), `reset` and each application's `reset` show
  the reason, the reversal by number, when and by whom; its `version` is
  1 + applications + undoings. The credit on account (`on_account`) is the
  line the last application in force left, else the payment's own while
  it is applied: undoing application n opens the credit it consumed again
  as it stood. The receipt, once its payment is undone, waits among the
  unapplied receipts and may be applied again by a new payment; its items
  too. IncomingPaymentReset (`x-webhook`: the customer by id and number,
  `application_no`, what it had applied, how many items, the clearing
  entry and its reversal, and for the payment's own each customer's part,
  T75h; no reason, which is free text) and the audit
  record `receivables.incoming_payment.reset` (with the reason).
- **Schema (000364):** the two ledger tables, insert-only (no insert of
  a reset item by `erp_app`), the two triggers;
  `receivables.incoming_payment_reset` (keyed by tenant and reversal, a
  reversal one undoing's; application NULL for the payment's own; foreign
  keys to the payment and to the application; once each,
  `UNIQUE NULLS NOT DISTINCT`; insert-only).
  `incoming_payment_receipt_once`, `incoming_payment_item_once` and
  `incoming_payment_application_credit_once` give way to indexes: a
  receipt, an item and a credit line applied again after an undoing are
  held to one application at a time by `clearing_item_pkey` under the
  entries' locks. The down migration is refused while anything was reset.
- **Decided by default:** (1) a reset always reverses the clearing entry
  (SAP's "reset and reverse"); a reset alone, leaving the entry's offsets
  open, stays later; (2) the reversal is dated the clearing entry's own
  posting date, so a period closed since refuses the undoing (the brief's
  rule) rather than reversing into the current period; (3) undoings are
  last first, the payment's own after its applications', matching the
  chain each application consumes; (4) a reason is required; (5) the
  reason stays out of the event; (6) only incoming payments' clearings
  are reset through the API; the ledger's port resets a clearing only for
  the context and document that posted its entry (`ClearingReset`'s
  source, 409 `ledger.entry_owned_elsewhere` otherwise, as `:reverse`
  holds an entry to its owner), manual clearings and settlements being
  later slices.
- Tests: `TestResetPayment`, `TestResetResidualClearedSince`,
  `TestResetApplication`, `TestResetClosedPeriod`, `TestResetConcurrently`,
  `TestResetRacesAnApplication`,
  `TestDatabaseFailures` (receivables); `TestResetClearingRefusals`,
  `TestParallelClearing/reset` (ledger); `TestClearingResetRules`,
  `TestIncomingPaymentResetRules`, `TestClearingResetRevert` (db).
  Walkthrough `step_payment_reset` (step 117).

### T75j Customer debit memo (migrations 000379, 000380)
A variant of §7 (`plan-depth-3.md` §T75j): a customer debit memo charges
the customer what an invoice did not (freight, a price correction), as
SAP's debit memo does (FB70/FB75 in FI, billing type L2 in SD; from
memory, not checked against a source in the lane).

- `internal/receivables/kinds.go`: the kind `debit_memo` on
  `POST /v1/customer-invoices` and as a filter of
  `GET /v1/customer-invoices`. A debit memo is entered and checked as an
  invoice is (items more than zero, each item's tax at its own date) and
  names no invoice (422 `receivables.reference_not_allowed`, as an
  invoice); a credit memo naming a debit memo is refused as one naming no
  posted invoice (422 `receivables.invalid_reference_invoice`). Its
  number is from the `customer_invoice` range, as a credit memo's.
- **Posting** (`documentTypes`): as an invoice, Dr the customer's
  reconciliation account with the gross, Cr each item's account and
  output tax, at its own date's rates, as journal document type `DD`
  with the header text "Customer debit memo <number>"; its entries draw
  their numbers from the range `journal_dd` (422
  `config.no_number_range` in a company without it, and nothing posts,
  `TestDebitMemoWithoutRange`). Its open item keeps the term entered
  with it, else the customer's in the company, from its own date. It is
  not counted against any invoice's credit limit, which only credit
  memos count. Events and audit records carry the kind, the amounts
  positive (`signed` turns a credit memo's alone).
- Migration 000379: `customer_invoice_kind_check` widened to
  `debit_memo`, added `NOT VALID`; 000380 validates it. The down is
  refused while a debit memo exists (`TestDebitMemoRevert`); schema
  cases in `TestCustomerInvoiceRules` and `TestCustomerCreditMemoRules`
  (a debit memo naming an invoice, `customer_invoice_reference_on_credit`).
- Tests: `TestDebitMemo`, `TestDebitMemoWithoutRange`,
  `TestCreditMemoRefusals` (a debit memo naming an invoice, a credit memo
  naming a debit memo). Walkthrough `step_debit_memo` (step 126).
- **Decided by default:** (1) the journal document type is `DD`, the
  brief's own type and range beside the invoice's `DR` (SAP's standard
  FI type for a customer debit memo is DR, from memory; a type of its own
  lets the ledger and reports tell them apart, and `DD` is not a standard
  SAP type); (2) the document's number is from the `customer_invoice`
  range, as a credit memo's is; (3) a debit memo names no invoice (a
  reference is the placeholder's "later"); (4) terms as an invoice's,
  from its own date; (5) `journal_dd` is in no country pack: the pilot's
  settings add it (walkthrough step 2, as they add `returns_order` and
  the other ranges the packs lack), and a company on Canada 2026.9 or UAE
  2026.6 adds it by a change request until a pack version brings it
  (numbering-only versions need the product owner's word on the
  sign-off, as 2026.9 and 2026.6 had; listed for the digest). Since
  batch 81 Canada 2026.10 and UAE 2026.7 bring it, under the sign-off of
  2026-11-09 (product owner, 2026-11-18; ADR-0048 As built), and the
  pilot's settings no longer add it. Since batch 83 Canada 2026.11 and
  UAE 2026.8 bring the other ranges the pilot added too, the material
  range aside (ADR-0048 As built, the pilot's ranges).

### T75k Reversing a customer credit memo or debit memo (migrations 000381, 000382)
A variant of §7 (`plan-depth-3.md` §T75k): a credit memo or debit memo
posted in error is reversed, as SAP's FB08 reverses a document (from
memory, not checked against a source in the lane). §7's "none is
reversed in the first version, so no credit is ever freed" (T75d's
default 5) no longer holds; an invoice is still corrected by a credit
memo (§5, plan default 3).

- `internal/receivables/reverse.go`: `POST /v1/customer-invoices/{id}:reverse`
  (`reverseCustomerInvoice`; `receivables.invoice.post`, `If-Match`,
  idempotent by `Idempotency-Key`, human-only) with a `reason` (1 to 200
  characters, not white space alone) and an optional `posting_date`. The
  memo's row is read `FOR UPDATE` (ADR-0008); the ledger's port
  (`ledger.Postings.Reverse`) posts the reversal: the original's lines
  with the signs turned, its document type (`DG`, `DD`, drawing from the
  same range), reference and `source_context` kept, the header text
  "Reversal of customer credit memo <number>" (or debit memo), its tax
  items negated on the original's tax date (ADR-0049 §4), and clears the
  memo's open item against the reversal's (`clearReversal`), so neither
  is open. The document becomes `reversed` with `reversal_entry_id`,
  `reversal_entry_year`, `reversal_entry_number`, `reversal_posting_date`
  and `reversal_reason`; its version moves on.
- **Refusals:** not posted (a draft, one reversed already) 409
  `receivables.invalid_status`; an invoice 409
  `receivables.invoice_not_reversible`; a memo another context entered
  from its document (sales' billing cancellation or credit memo request,
  `source_document_id`) 409 `receivables.entered_from_source`, since the
  sales document would still show it as posting; a reason of white space
  422 `request.validation_failed`; a date before the memo posted 422
  `receivables.reversal_before_posting` (`posting_date`); a memo whose
  item is cleared (netted in a payment, cleared against its invoice) 409
  `receivables.document_cleared`; a period not open on the reversal's
  date the ledger's 409 `ledger.period_not_open`, the period's problem,
  and nothing changes; the ledger's other refusals of a reversal.
- **Locks:** the memo's row, then the entry's lock the ledger takes in
  `reverse`. Whether the item is cleared is read before the entry's lock
  only to answer with this context's problem: an application or a
  clearing of the item takes the entry's lock, and the ledger checks
  again under it, so one that committed meanwhile refuses the reversal
  with `ledger.entry_cleared` and the request rolls back
  (`TestReverseRacesAClearing`). No other writer locks a posted memo's
  row (no document names a credit or debit memo by foreign key; an
  application locks entries only), and a credit memo's posting locks its
  own row and then its invoice's, never a memo's, so no two writers wait
  on each other in a cycle. Two reversals at once: the second waits for
  the row lock and meets another version (412,
  `TestReverseConcurrently`).
- **The limit:** `creditedInvoice` counts posted credit memos alone, so a
  reversed one frees what it took (`TestReverseCreditMemoFreesItsCredit`).
  A reversal and a posting naming the same invoice need no common lock:
  a reversal only lowers the sum the posting reads.
- **Events and audit:** `CustomerInvoiceReversed` (`x-webhook`: the
  customer by id and number, the kind, the invoice a credit memo names,
  the totals signed as `CustomerInvoicePosted` gave them, the entry and
  its reversal by id, year and number, the reversal's date) and the
  audit record `receivables.customer_invoice.reverse`; neither carries
  the reason, free text a person wrote (ADR-0079, the batch 79 review),
  which the document keeps. The refusals are audited on the document
  (`httpapi.RefusalsAudited`). The ledger's `reverseJournalEntry` now
  names this path for receivables' entries (`requireOwnEntry`), with the
  invoice's correction and T75i's reset.
- Migration 000381: `reversal_date`, `reversal_entry_year`,
  `reversal_entry_id`, `reversal_reason`; the status `reversed`
  (`customer_invoice_status_check`); `customer_invoice_posted` admits a
  reversed document with its entry; `customer_invoice_reversed` (the four
  together exactly when reversed, a reason of 1 to 200 characters not
  white space alone, only a credit or debit memo without a source
  document, on or after its posting date);
  `customer_invoice_reversal_once`; the trigger
  `customer_invoice_posted_once` admits posted → reversed keeping what the
  document posted with, and nothing after; the update grant on the four
  columns. The checks are added `NOT VALID` and 000382 validates them;
  the down is refused while a document was reversed. Schema tests:
  `TestCustomerMemoReversal`, `TestCustomerMemoReversalRevert`.
- Tests: `TestReverseDebitMemo`, `TestReverseCreditMemoFreesItsCredit`,
  `TestReverseRefusals`, `TestReverseConcurrently`,
  `TestReverseRacesAClearing`, `TestDatabaseFailures`; the ledger's
  `TestReversalBelongsToTheOwner`. Walkthrough `step_memo_reversal`
  (step 127).
- **Decided by default:** (1) the reversal is dated the memo's own
  posting date unless another is given, as the ledger's reversal and
  T75i's are; a period closed since refuses it, and the person names a
  date in an open period; (2) a date before the memo posted is refused
  (SAP refuses a reversal dated before the original, from memory); (3) a
  free-text reason is required and kept on the document, not in the
  event or the audit record; reason codes are later; (4) the reversal
  keeps the memo's document type and range, as the ledger's reversal of
  any entry does; (5) a memo sales entered is refused rather than leaving
  its sales document posted against a reversed memo; (6) only credit and
  debit memos: an invoice stays corrected by a credit memo; (7) the
  permission is `receivables.invoice.post`, as payables' reversal takes
  its posting permission.

### T75l AR aging as at a past key date, and by document date (no migration)
- `GET /v1/receivables-aging` takes `open_on`: `now` (the default, T75e's
  items open now, whatever their dates) or `key_date`, the items open on
  the key date; and `aging_basis`: `net_due_date` (the default) or
  `document_date`. Both are open enumerations, said in the response; any
  other value is 400 `request.invalid_parameter` naming the field, as
  `getAssetHistorySheet`'s `group_by` is refused. The page token is signed
  for both besides T75e's parameters.
- **Open on the key date** is `ledger.OpenItems.ReceivableOn`
  (`internal/ledger/receivable_on.go`), T75b's reader (`receivableSelect`,
  `scanReceivable`) with another condition: the line is posted on or
  before the key date, no clearing dated on or before it holds it
  (`ledger.clearing_item` and the clearing's `clearing_date`), and its
  entry was not reversed, nor is it a reversal, by a reversal posted on or
  before it. SAP's due date analysis and open items at a key date (FBL5N,
  S_ALR_87012178) select so by posting and clearing date (from memory, not
  checked against a source). A clearing that was reset (T75i) holds its
  items on no day: its items left `clearing_item`, and its clearing entry
  was reversed on its own posting date (`Postings.ResetClearing`), so the
  reset undoes it from the day it was dated, as FBRA leaves its items no
  clearing date; the clearing entry's own lines are of a reversed entry. A
  reversal the ledger dated before its original leaves the pair open on no
  day. The item's amount, terms and due date are as posted: a partial
  payment's residual (T75f) is its own item from its clearing's date, the
  item it came from cleared on that date.
- **By document date** each item is aged by its days past its document
  date (`ledger.DaysPast`, which `ReceivableItem.DaysOverdue` now counts
  with) as if due that day, its terms aside: `not_due` when dated on the
  key date or later, else the `overdue` bucket of those days. The buckets
  and their names are T75e's, so a consumer's columns do not change with
  the basis.
- **Budget:** `TestReceivablesAgingOnKeyDateBudget` holds the read as at a
  key date to T75e's 3 s over the same 100,000 items, half cleared after
  the key date (so all are aged, half looked up in their clearings);
  measured on a laptop under load 22-66: median 2.17 s, p95 2.98 s at
  `ERP_BUDGET_SCALE` 1, median 2.28 s, p95 3.21 s at 2 (T75e's read
  beside it: p95 2.91 s and 2.17 s). No index is added: the read is
  T75e's join with three exists checks by primary key.
- Walkthrough `step_aging_as_at` (step 133): Ruth sets up Harbour Tools,
  Alex charges it CAD 300.00 by a manual entry on the period's first day
  and reverses it today (on the period's first day: today and tomorrow),
  Ruth's invoice of CAD 1,000.00 dated today on NT30 posts on the
  reversal's day; the aging now and as at the reversal's day is the
  invoice, as at the charge's day the charge, the day before nothing; 45
  days on the invoice is 1 to 30 days past its net due date and 31 to 60
  past its document date; `open_on=yesterday` is refused.
- **Decided by default:** (1) `open_on` defaults to `now`, so T75e's
  consumers (the credit and collections agents among them) see no change;
  (2) the as-at read takes the clearing date, not the clearing entry's
  posting date, as the plan says (they are the same for every clearing
  the system makes); (3) a reset clearing counts on no day, as above; (4)
  by document date the bucket names stay T75e's (`not_due`, `overdue`)
  rather than new ones, the description saying what they count; (5) no
  new permission, problem code or table.

### T75m AR aging by profit center or segment, and its CSV export (no migration)
- `GET /v1/receivables-aging` takes `profit_center_id` and `segment_id`
  (the items whose customer line carries it; both together select the
  items carrying both), `group_by` `customer` (the default), `profit_center`
  or `segment`, and `format` `json` (the default) or `csv`; `group_by` and
  `format` are open enumerations refused as `open_on` is. The response
  says `group_by` and the filters; grouped, its rows are `groups`
  (`ReceivablesAgingGroup`: the center by id, code and name, its
  customers, items, amount and buckets) and `items` is empty. The page
  token is signed for the grouping and both filters.
- **The assignment** is the customer line's own, as posted (ADR-0050 §5):
  `ledger.ReceivableItem` now carries the line's `profit_center_id` and
  `segment_id`, read by T75b's reader for every consumer. A customer
  invoice posts its customer line without a center (its items carry
  them), so its item is in the row without; a manual or migrated entry's
  customer line carries what it names, the segment always its profit
  center's. No center is derived for the customer line here: that would
  be document splitting, which nothing has decided (listed for the product
  owner).
- **Grouping** is on read (`age`): each selected item falls in one row by
  its customer, profit center or segment (`groupKey`), the rows ordered by
  id with the row without a center (the nil id) first, paged by that key
  as the customers are; a row counts each customer once (the items come
  ordered by customer). The centers are named through the ledger's
  `CenterNames` (`References`), widened to segments; the customers
  through the partner context as before. An id that names no center
  selects nothing, an empty report rather than a refusal, as a customer
  filter does.
- **CSV** is the existing form of a report in the API (`format=csv` of
  `GET /v1/process-event-logs`, the next page in `Next-Page-Token`, now
  declared on this report's 200 for JSON too) with the workspace's
  spreadsheet-safe cells (ADR-0052 Built in T73j4): `safeCell`, the byte
  order mark and the row writer moved from `internal/web` to
  `internal/platform/csvcell` (`Safe`, `ByteOrderMark`, `Write`), which
  the workspace's downloads now call, rather than copied. A header row
  `row,id,code,name,currency,customer_count,item_count,amount` and a
  column per bucket (`not_due`, `overdue_<from>_<to>`,
  `overdue_<from>_plus`, `no_due_date`), a row per row of the page, and
  the totals' row (`total`) on the last page only, so the pages of an
  export add up to one file with one total; amounts are exact decimals in
  the company's currency, the currency in its own column.
- Walkthrough `step_aging_by_center` (step 134): Alex charges Harbour
  Tools CAD 200.00 on PC-MTL (segment CA) by a manual entry; Harbour Tools'
  aging by profit center is the row without (step 133's invoice) and
  PC-MTL's, by segment CA's, filtered by PC-MTL the charge; the company's
  groups add up to its totals; the CSV has the byte order mark, the
  header, PC-MTL's row and the totals.
- **Decided by default:** (1) the row without a center is a row of its
  own, first, so the groups always add up to the totals; (2) grouped
  rows are a new array rather than customer rows with a center, so a
  customer row keeps its required fields; (3) CSV cells are made safe for
  a spreadsheet, as the workspace's downloads are (the safer default for
  customer names typed by people), at the cost of an apostrophe a machine
  reader must strip from a text cell beginning as a formula; (4) the
  totals' row only on the last page; (5) no new permission or problem
  code.

### T75n A receipt in another currency than the items (migration 000398)
A variant of §6 (`plan-depth-3.md` §T75n): a receipt in one currency is
applied to a customer's items in another, as SAP's F-28 clears open items
in another currency than the payment (from memory, not checked against a
source in the lane). §6's and T75c's "in the receipt's currency" no longer
holds for the items; it still holds for residuals, the tolerance and what
is left on account.

- **Translation** (`ledger.Postings.ApplyReceipt`,
  `internal/ledger/receiptfx.go`): each item in another currency than the
  receipt is translated into the receipt's at the **average rate valid on
  the application's posting date** between the two currencies (the
  ledger's rate source, `ledger.exchange_rate`: the latest valid that day,
  either direction, the exact reciprocal when only the reverse pair is
  recorded), rounded once half away from zero, each item on its own and
  each currency read once. No rate is derived through a third currency:
  an item without a rate for its pair that day is refused, 422
  `ledger.no_exchange_rate` naming `items[i]`, and one that translates to
  nothing (a minor unit of a currency worth less than half a minor unit
  of the receipt's) 422 `ledger.item_rounds_to_zero`. The item is checked
  as the customer's item first, so a wrong item is refused as such before
  its rate is looked for.
- **Posting:** the clearing entry is in the receipt's currency, at its
  rates. It offsets each translated item at the translated amount in
  transaction currency and at the **local and group amounts it was booked
  at**, so the item is cleared at its own rate; what the receipt's and
  the items' local and group amounts leave is the realized exchange
  difference T75c already books on `fx_gain` / `fx_loss`. A residual of a
  translated item is in the receipt's currency (T75f's rule), at the
  item's local rate (its share of the item's booked local amount); it is
  an open item in the receipt's currency from then on. The tolerance,
  what is left on account and the later application of a credit on
  account (T75g, which takes items in another currency the same way)
  are in the receipt's currency.
- **The payment** keeps each item's `amount` in its currency, as the
  entry offset it, and adds `item_amount`, the item's own amount in its
  currency (the same as `amount` for an item in the payment's currency),
  and `exchange_rate`, the rate shown, absent for an item not translated
  (`incoming_payment_item`'s `item_currency`, `item_amount`,
  `exchange_rate`, all three or none, migration 000398; the down migration
  is refused while an item was translated). IncomingPaymentApplied is
  unchanged: it carries the payment's amounts, all in its currency.
- **Decided by default:** (1) the average rate type, as postings and the
  company's configured comparisons use (`Rates`), not a separate clearing
  rate type; (2) the rate of the posting date, not of the receipt's or the
  item's; (3) no cross rate through the company's currency, so a missing
  pair is refused rather than derived; (4) a residual in the receipt's
  currency, the entry having one transaction currency; (5) no opt-in: an
  item in another currency is translated whenever the request names it,
  the receipt's balance and the tolerance still holding the application
  to what was paid; (6) the ledger's `items_cleared` event gives a
  translated item's amount in the clearing entry's currency, as it gives
  every amount.

### T75o Payment-difference reason codes from configuration (migration 000399)
A variant of §6 (`plan-depth-3.md` §T75o): the reasons an item is paid in
part for are the company's configuration, as SAP's reason codes (OBBE)
are per company code, each with its "charge off difference" flag and the
account it is charged off to (OBXL) (from memory, not checked against a
source in the lane). T75f's decision (6), a fixed open enumeration, no
longer holds.

- **Configuration:** `companies[].receivables.payment_difference_reasons[]`:
  `code` (lower case letters, digits and `_`, to 40, once per company),
  `description` (1 to 80), `treatment` `residual` or `write_off`, and for
  `write_off` its `account`, an expense of the chart that reconciles
  nothing, which joins the accounts the company posts to automatically
  (`AutomaticAccounts`: default cost assignment, no secondary cost
  element, no asset reconciliation); a residual reason has none. A tier 3
  change, as the tolerance (it decides what reaches profit and loss and
  where). A company that configures none has T75f's four,
  `short_payment`, `disputed`, `deduction` and `other`, all `residual`
  (`config.DefaultDifferenceReasons`), so a tenant keeps what it had until
  it configures its own; one that configures any has exactly those.
- **Application:** an item's `residual.reason` is one of the company's
  reasons (422 `receivables.unknown_residual_reason`, the message listing
  them). A `residual` reason leaves the residual item as T75f. A
  `write_off` reason writes the amount off instead (`ledger.Residual.WriteOff`):
  a debit line of the clearing entry on the reason's account at the
  **receipt's rate**, as the payment difference is, the item cleared and
  nothing of it left open; the account is found by its number in the
  company when the payment is applied (422 `receivables.no_write_off_account`
  should the company's accounts no longer have it), and the ledger checks
  it can be posted (its refusal names it as "item i's write-off account").
  Each direction is held to its own tolerance, so that one never nets
  the other away: what reasons write off, with an underpayment the items
  leave, to the underpayment tolerance; an overpayment not left on
  account to the overpayment tolerance; the base being the items applied
  net of their residual items, as T75f's; beyond either, 422
  `ledger.clearing_not_balanced`. A write-off line takes its item's
  trading partner, as a clearing's difference lines do (ADR-0059 §4).
- **The payment** shows the item's `write_off` (amount, reason, text,
  account by id, number and name, its line in the clearing entry) in place
  of a residual; `difference` stays the payment difference alone, and the
  customers' parts are net of residuals and write-offs, so the parts less
  the difference and with what was left on account are still the receipt.
  The audit records count `write_offs`; IncomingPaymentApplied and
  IncomingPaymentOnAccountApplied are unchanged (their parts and amounts
  already net them). Schema (000399): the write-off columns of
  `incoming_payment_item`, all together or none, on a debit item, less than
  it and never beside a residual; the down migration is refused while one
  exists. The residual reason's column had no list check, so the codes need
  no migration and a stored reason stays as it was.
- **Decided by default:** (1) the four reasons stay the default of a
  company that configures none, rather than leaving it without any; (2)
  a write-off is held to the tolerance, as SAP holds charge-offs to the
  tolerance group (from memory), rather than allowed by the reason alone;
  (3) a write-off at the receipt's rate, as the difference; (4) an expense
  account only (an underpayment); (5) the request keeps `residual` as the
  one place an item's short payment is named, the reason deciding what
  becomes of it, rather than adding a second field the reason would have
  to agree with; (6) a tier 3 change.
- **For the specialist:** as for T75f, whether the output tax must be
  corrected on what is written off (the first version posts none).

### T76h Debit memos a dunning run enters (no migration of its own)

Recorded for ADR-0065 As built T76h, which decides it. Collections enters
and posts a customer debit memo per dunning notice charged a charge or
interest through `CreateInvoice` and `PostInvoice`, as sales enters its
billing documents: the notice is its source document, by id and with its
id as its number (a notice has none). Nothing in receivables changes but
the reversal's refusal (`reversible`): a debit memo entered from a
source is a dunning notice's (sales enters credit memos alone), and
`receivables.entered_from_source` names it so.

### T76i Reversing a debit memo for the context that entered it (migrations 000401, 000402)

Recorded for ADR-0065 As built T76i. `ReverseSourced` is T75k's reversal
for the context that entered a debit memo from its document (collections,
cancelling a dunning run): the caller names the source, which must be the
memo's (otherwise an error, not a refusal); there is no `If-Match`, the
caller checking the version of its own document; the reversal posts on
the memo's posting date with the caller's reason; everything else is
refused as T75k refuses it (not posted, cleared, a period not open).
`customer_invoice_reversed` now admits a reversed debit memo with a
source document; a credit memo with one (sales') still is not reversed.
The API's `reverseCustomerInvoice` refuses both, naming the dunning
notice and `cancelDunningRun` for a debit memo.

### Batch 88: the tax specialist's confirmations (po:2026-11-21)
Musto Ibrahim confirmed on 2026-11-21 (relayed by the product owner)
that a plain write-off or a short payment (T75f, T75o) makes no tax
correction, and that a Canadian cash discount makes no GST/HST
adjustment; a cash discount on a UAE invoice reduces output tax, which
receivables does not do yet (ADR-0049 As built batch 88, tracker leaf).

### Cash discount granted (po:2026-11-21, migration 000445)
The product owner's answer of 2026-11-21 (ADR-0049 As built batch 88): a
cash discount a UAE customer takes reduces the output tax, as T36d's
correction reduces the input tax on the supplier side; a Canadian cash
discount makes no GST/HST adjustment (Excise Tax Act s.161, confirmed by
Musto Ibrahim on 2026-11-21). T75f's "Not built: cash discount granted"
is built for the UAE.

- **The request.** An item of `POST /v1/incoming-payments` may say
  `cash_discount: true`: it takes the cash discount its payment terms,
  as it was posted with them (`ledger.open_item_terms`, T75b), grant for
  a payment received on the **receipt's posting date**, the day the bank
  statement line posted it (`ledger.ReceiptApplication.DiscountDate`),
  not the application's, which may be days later. The discount is the
  tier's percent of the item's amount in the receipt's currency, rounded
  half away from zero, one minor unit less where it would take the whole
  item (`ledger.ItemTerms.DiscountOn`, the rule the payment run takes a
  supplier's discount by). Only an invoice paid in full takes one: a
  credit of the customer's or an item with a residual is refused, 422
  `ledger.invalid_discount`, and an item whose terms grant none that day
  (no tier, the period over, no terms) 422 `ledger.no_cash_discount`,
  each naming `items[i].cash_discount`. So is an invoice under a code of
  which one component levies its lines' subdivision and another does
  not (422 `ledger.invalid_discount`): the line rule is per code
  (`journal_line_tax_jurisdiction_code`), while the other component's
  items name no emirate, so its share could not be kept in one; the
  discount is then written off by a reason (T75o). The items, net of
  their residuals, write-offs and discounts, must then equal the receipt
  but for the payment tolerance, as before.
- **The correction** (`internal/ledger/receiptdiscount.go`): the discount
  is a debit of the clearing entry split by the invoice's entry's tax
  items exactly as a supplier's is (T36d): `ledger.discountTax`, now
  reading the partner lines of the items' kind (supplier or customer),
  gives each tax item a share of the discount in proportion to its tax
  over the gross the entry owes, rounded once and shared out by largest
  remainder, and the net to each code's base and the untaxed rest. The
  split is keyed by the subdivision a component that levies its lines'
  names (the emirate, T61f), so each emirate's tax and base are lines of
  their own naming it, the trigger `journal_line_tax_jurisdiction_code`
  holds, and the tax items (`sourcedTaxItem`, copying the source item's
  code, component, jurisdiction and rate, at the payment's tax date)
  reduce that emirate's VAT 201 box; a zero-rated base is reduced at rate
  0 (box 4). The supplier side gains the same keying: an input code that
  levied its lines' subdivision would now have its correction's lines
  name it (none can: the configuration allows a component to levy its
  lines' subdivision on output codes alone). Lines and items are at the
  **receipt's rate**, as the payment difference is; the items' local amounts are
  shared out of each line's (`discountPlan.taxItemsAt`). Undoing the
  payment (T75i) reverses the clearing entry and its tax items with it.
- **The account** (decided by default, the brief's question): the net
  of the discount is posted to the account determination's
  `payment_difference_expense`, with each code and emirate of the base
  it reduces. It is the existing key for what a customer takes off, an
  expense, and the account T75f wrote the same amount to before, so the
  profit and loss account of a discount does not change with it; the
  `cash_discount` key is SAP's SKE, the discount *received* from
  suppliers (revenue, `4800` in the UAE pack), which a discount granted
  must not reduce. Without the account the application is refused, 422
  `ledger.no_payment_difference_account`. SAP's own key for a discount
  granted (SKT) stays the post-pilot tracker leaf.
- **Which companies** (`receivables.discountTaxCountry`): a company of
  the UAE grants the discount. Any other refuses one asked for, 422
  `receivables.cash_discount_not_granted` naming `items[i].cash_discount`:
  what its customer takes off stays a payment difference written off
  without a tax code, within the tolerance or by a residual's reason
  (T75f, T75o), which is the confirmed Canadian rule, and every other
  country's until its tax specialist answers; the company's country
  decides, as the ledger's `taxDocumentCountry` does. An application of
  a credit on account (T75g) grants none either (the same refusal): its
  items were not paid by a receipt within their period.
- **What the payment shows:** each item's `cash_discount` (absent for
  none), the payment's `cash_discount` (their total), each customer's
  part net of its discounts; IncomingPaymentApplied carries
  `cash_discount` (an optional field) and the audit record too.
- **Schema** (000445): `incoming_payment_item.cash_discount`, more than
  zero and less than the item, which is an invoice paid in full, never
  beside a residual or a write-off (`incoming_payment_item_cash_discount`,
  `TestIncomingPaymentCashDiscountRules`); the down migration is refused
  while an item took one.
- **Decided by default:** (1) the discount is asked for per item, not
  taken by itself: a customer paying the gross within the period takes
  none, and the clerk confirms what the remittance says, as SAP's F-28
  lets the clerk change the proposed discount (from memory); (2) the
  discount period is judged on the receipt's posting date; (3) only an
  invoice paid in full takes one (no residual, no credit); (4) the
  `payment_difference_expense` account (above); (5) the receipt's rate;
  (6) the UAE alone; (7) none on an application of a credit on account;
  (8) an item in another currency than the receipt takes its discount of
  its translated amount, the split being by proportion.
- **For the specialist** (stated from memory, not checked against a
  source in the lane): whether the UAE requires a tax credit note to the
  customer for the discount's VAT (the Executive Regulation's tax credit
  note rules), which the e-invoicing send side would then render; whether
  a zero-rated supply's base in box 4 is reduced by its share; and
  whether the discount period is judged on the day the funds were
  received or the day the customer paid.
- Tests: `TestUAECashDiscount` (a real UAE company on the pack 2026.9's
  codes and VAT 201: the clearing entry's lines and tax items, the boxes
  before, after and once the payment is undone), `TestUAECashDiscountRefusals`,
  `TestUAECashDiscountOfSome` (a dollar invoice taking its discount on
  its translated amount beside a dirham one taking none),
  `TestCanadianCashDiscount`, `TestCashDiscountDatabaseFailures`,
  `TestDiscountsOutOfRange` (ledger),
  batch 85's `TestDiscountOnLeviedJurisdiction` (a company of DE grants
  none). Walkthrough step 156 (`step_uae_cash_discount`).

### As built batch 90: bad debt relief (po:2026-11-21)

A customer invoice a dispute case's write-off wrote off has the output tax
of what was written off recovered by `createBadDebtRelief`
(`receivables.bad_debt_relief`, migration 000446): an adjustment entry
the VAT 201 or the GST34 reports, under the conditions of the company's
country. The rules, the return lines and every default are in ADR-0049
As built batch 90.

### As built batch 91: incoming payments of a relieved invoice (po:2026-11-21)

Applying an incoming payment, applying its credit on account and undoing
either now hold the payment's customers (advisory lock
`receivables.bad_debt`, before the ledger's entries) and charge again the
tax of bad debt reliefs of their invoices that the change recovers, or
charge back less (`bad_debt_recharges` on the payment; ADR-0049 As built
batch 91 has the rule). A payment of a customer without a relieved
invoice posts nothing more.
