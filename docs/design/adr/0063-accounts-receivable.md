# ADR-0063: Accounts receivable

**Status:** accepted (T75a, 2026-11-11; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T75a's last commit). It decides
the receivables context and the documents of T75a-T75e
(`docs/design/plan-breadth-week1.md`, Lane A). Each slice records what it
built under "As built"; T75a's, T75b's, T75c's and T75d's are there now. The
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
post correctly. This is a gap for the UAE company, listed below.

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
`ledger/aging.go`, exported, not copied; no table.

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
  invoices (422 `config.no_number_range`). The released country packs
  bring none; a tenant adds it by a change request.
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
  lines; not built, no leaf added.

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

### T75b Customer invoice posted: the customer's open item (migration 000267)
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
- Migration 000267: `payment_term_code` (optional at entry),
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
  terms. Cleared lines and lines of a reversed entry or a reversal are
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

### T75d Customer credit memo (migration 000269)
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
- Migration 000269: `reference_invoice_id` on
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
- Walkthrough `step_customer_credit` (step 50): Ruth enters and posts a
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

### T75c Incoming payment applied to a customer's invoices (migration 000268)
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
- Migration 000268: `receivables.incoming_payment`
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
- Walkthrough `step_incoming_payment` (step 49): RBC's third statement of
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
