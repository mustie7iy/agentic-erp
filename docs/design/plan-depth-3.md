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
