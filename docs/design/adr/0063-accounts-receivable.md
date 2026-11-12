# ADR-0063: Accounts receivable

**Status:** accepted (T75a, 2026-11-11; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T75a's last commit). It decides
the receivables context and the documents of T75a-T75e
(`docs/design/plan-breadth-week1.md`, Lane A). Each slice records what it
built under "As built"; T75a's and T75b's are there now. The defaults
taken and the questions left are listed at the end for the product
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

### 7. Credit memos (T75d)
A customer credit memo is the same document with the kind `credit_memo`,
posted with the signs turned. One naming an invoice may not exceed that
invoice's gross, checked at posting under the invoice's row lock, as the
product owner decided for supplier credit memos (2026-11-10) — plan
default 5, to confirm (question 7 of the plan). Its tax is at its own
date.

### 8. Permissions and agents
`receivables.invoice.read`, `receivables.invoice.write` (entering) and
`receivables.invoice.post` (T75b); T75c adds `receivables.payment.apply`.
None is an approval permission. Company access applies as everywhere: a
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
