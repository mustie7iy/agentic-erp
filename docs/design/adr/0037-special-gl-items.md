# ADR-0037: Special G/L items

**Status:** accepted (T46, product owner 2026-10-17)

## Context
A supplier's open items are the lines on its reconciliation account, and a
payment run nets them: an invoice less a credit memo is one payment, and a
supplier owed nothing after its debits is excluded (T34d). A down payment
to a supplier is a debit on the supplier, but it is not a debit to net: it
is an advance on an invoice still to come, kept on the balance sheet as an
asset until that invoice arrives and it is cleared against it. Posted on
the ordinary payables account it would net in the next run and pay the
supplier less than it is owed, and the balance sheet would show advances
inside trade payables. SAP keeps such items apart as special G/L
transactions: lines of the vendor on an alternative reconciliation account
(T074), marked by a special G/L indicator (A for down payments), and a
down payment request as a noted item (F), a statistical record outside the
general ledger that the payment program can pay.

## Decision
- A journal line may carry a special G/L indicator, `journal_line.special_gl`;
  `A` (down payment) is the only one for now. A line with an indicator has a
  partner, and it is on the legal entity's account for it, from the account
  determination (`down_payment`): an asset that is a vendor reconciliation
  account. A line on that account carries the indicator, and a line with the
  indicator is on that account; the account is fixed once determined, as the
  GR/IR account is, so a down payment's reversal finds it.
- Special G/L items are open items of their supplier, cleared like any, but
  the payments context's payable items leave them out: they never net in a
  run or count towards a debit balance. They leave the special account only
  by being cleared against the supplier's invoice (T46d), which moves them to
  the ordinary payables account, or by a reversal.
- A down payment request is a payables document, not a ledger posting: the
  ledger takes only balanced entries, and a request moves no value until it
  is paid. Paying it posts the special G/L item.

## Alternatives
- **Down payments on the ordinary payables account, flagged to be skipped by
  proposals:** keeps one account, but advances would sit inside trade
  payables on the balance sheet, and every reader of open items would need
  to know the flag.
- **Requests as statistical ledger postings:** SAP's noted items are one-sided
  entries; the ledger's balance rule would need an exception for them.

## Consequences
- Every consumer of supplier open items decides whether special items count;
  payment proposals and debit balances do not (`ledger.OpenItems.Payable`).
- The open-items listing, journal entries and `JournalEntryPosted` show the
  indicator.
- Migrated SAP down payments (BSIK with UMSKZ A) load with the indicator on
  the down payment account (a follow-up of the SAP load).
- A run pays a request as a payment of its own (kind `down_payment`), never
  netted, and posts it as the special item with its input tax (T46c).
- Clearing a down payment against the invoice reverses the input tax its
  payment claimed, since the invoice claims the whole tax, and debits the
  supplier under the invoice's terms, so a cash discount is taken, and its
  tax corrected, only on the remainder; the invoice is not reversed while
  the clearing stands (T46d).
- Special G/L items keep no payment terms, and the tax share of a cash
  discount counts only ordinary supplier lines as what an entry owes.

## Addendum: withholding tax (T46e-f, product owner 2026-10-18)
- Withholding tax is per code of the legal entity (rate, base net by
  default or gross, deducted at invoice or at payment, payable account) and
  a supplier's codes per legal entity (T46e). An invoice's supplier open
  item keeps each of its supplier's codes as they were when it posted (SAP
  WITH_ITEM, `ledger.open_item_withholding`), so a later change to a code
  or to the supplier does not change what its payment withholds.
- At invoice: the posting credits the code's rate of the base to its
  account and the supplier with the rest. At payment: the run withholds it
  from the paid item, pays the net and posts the withholding to the code's
  account in the release posting, once per payment, as the posting itself.
- With a cash discount, a code deducted at payment withholds on the base
  after the discount: the base scaled by what the item pays after its
  discount over what it owes (SAP's base reduction by the discount),
  rounded once. A code deducted at invoice is not changed by the discount.
- The run records what it withholds per item and pays out the rest; the
  release posts only that: it recomputes the withholding and refuses a
  payment whose items now withhold another amount (409
  `ledger.withholding_changed`). A withholding that, rounded after the
  discount, would take all the item owes fails the proposal (422
  `payments.withholding_too_large`). `PaymentRunProposed` carries the run's
  withholding total. Every input of the recomputation is fixed with the
  item and the payment, so a mismatch is a defect, not a business case:
  the run stays `releasing` with that payment unposted until it is fixed.
- None on down payments (requests or their payment), and no withholding
  tax certificates; manual clearing withholds nothing, only payment runs do.

## As built: SAP's open down payments (T72c, 2026-10-28)
Decided by default under ADR-0053 §d, each from this ADR or T46's code:
- **What loads.** A vendor line of the extract (ACDOCA) with special G/L
  indicator A (UMSKZ) that SAP has not cleared is an open down payment. The
  extract reads it from the Universal Journal, as it reads every open item
  (by AUGBL), rather than from BSIK, the open-item index. The down payment
  account is the configuration's `down_payment`, which the configuration
  agent takes from T074 (vendors, indicator A; a second account is noted).
- **As the platform's own.** Its SAP document posts as an entry of its own,
  in documents mode and in balances mode alike (the opening entry leaves the
  document out), with the down payment as the supplier's special G/L item A
  on its tax code and its tax lines on that code, as `PayDownPayment` posts
  one; no other migrated line carries a tax code. Payables records it as a
  paid down payment request naming the entry (`TakeOverDownPayment`; number
  from the request range of its posting date's year, due date its posting
  date, reference the source system and document, or the document alone
  when both exceed the request reference's 40 characters; the external
  reference keeps the system), so it is listed with the supplier's
  requests, never paid again, and cleared against the final invoice, which
  finds a down payment by its request, reversing its input tax (T46d). The
  take-over is audited and publishes no event, as the quantities of
  migrated invoices (T72b2): nothing was requested or paid on the platform,
  and the entry's `JournalEntryPosted` carries the item.
- **Never guessed.** The quality gate refuses another indicator than A, A
  off the down payment account or a line on it without A (the indicator is
  not inferred from the account), more than one open down payment in a
  document (one request per entry, as a run posts them), a credit, a tax
  code that is not one deductible input tax with a rate on the date, a tax
  line not on the code's account, and a tax that is not the code's rate of
  the down payment: the platform keeps a down payment net of tax, SAP's net
  display; its gross display, where the down payment includes the tax and a
  tax clearing account offsets it (SAP Help, "Net or Gross Display of Down
  Payments"), is refused. A down payment SAP cleared is refused in
  documents mode, since SAP's clearing of it is not taken over (a migrated
  clearing refuses special items); balances mode leaves cleared items out.
  In balances mode the down payment's document may hold no cleared vendor
  line and needs a number range for its type; the request needs the
  down payment request range of the year. A vendor line without an
  indicator on a vendor reconciliation account that T074 names, or that no
  vendor has as its reconciliation account (LFB1-AKONT), is refused as a
  possible special G/L item whose indicator the extract lacks (the OData
  mapping does not carry UMSKZ yet), rather than loaded as a payable a run
  would net. An open item SAP left on a vendor's former reconciliation
  account is refused by the same rule until a specialist confirms it.
- **Reconciled apart.** The migration's open payables leave special G/L
  items out on both sides, as a payment run does, and the open down
  payments are a check of their own, in total and per vendor, shown when
  either side has one; a down payment loaded as an ordinary item fails both.
- **Tax returns.** The migrated entry's tax items carry SAP's posting date:
  SAP's returns reported that tax. The returns still to be built must leave
  out the items of migrated entries (source context `integration`) or of
  dates SAP reported, so the claim is not made twice; the clearing's
  negated items, dated on the platform, are the platform's to report.
  Built with the return engine (T61h, ADR-0049 §11 As built): a draft
  return reads no item of an entry whose source context is `integration`.
- **Document currency (review:batch20).** A down payment SAP made in a
  foreign document currency (ACDOCA RWCUR other than the company's), or
  one whose extract has no document currency, is refused by the quality
  gate with every other such open item, until they are migrated in their
  own currency (ADR-0005, As built).

## As built: down payments listed by their indicator (T73e4, 2026-11-02)
`listOpenItems` takes `special_gl`: with `A` it lists a supplier's down
payments alone, which the web workspace's clearing against the final
invoice chooses from (ADR-0052, Built in T73e4). Listing the down payment
account's items (`account_id`) does the same, but no operation names that
account to a client.

## As built: each request cleared once (batch 39, 2026-11-02)
Clearing finds a chosen down payment's request by its payment entry
(`clearedRequests`), so two down payment lines of one entry, which the
ledger's shared-tax rule makes a person choose together, named the same
request twice: it counted twice in the invoice's gross check and in the
cleared total. A request now counts once however many of the items its
entry holds. A choice that is not exactly the requests' down payments, one
line of a request's two or an entry paying two requests, is refused 422
`payables.not_a_down_payment_request` (`requireWholeRequests`): the
transfer's payables debit, what the ledger cleared, must equal the
requests' total, and the refusal rolls the transfer back. This decision
follows from this ADR's "one request per entry, as a run posts them" (As
built, T72c) and from clearing only whole down payments (T46d): rather
than guess which request a line belongs to, the clearing refuses a shape
the ADR rules out. Refusing such an entry, rather than splitting it, is
the default; no path posts one today (review:batch39).

## As built: SAP's noted items are not migrated (bstat, review:batch63, 2026-11-10)
SAP keeps a down payment request as a noted item: document status BSTAT
`S`, special G/L indicator F, posting nothing to the G/L and shown among
the vendor's open items only (the SAP specialist, po:2026-11-09, A14 [H]
and A15 [M]). The migration read such a line as a journal line: a
one-sided request stopped the run as an unbalanced document and as a
special G/L indicator that is not migrated, and a noted document that
balanced, in an extract without the indicator, was posted as a journal
entry with an open payable. Decided by default from this ADR: a request
is a payables document outside the ledger ("Requests as statistical
ledger postings" is the alternative refused above), so a noted item is
never a journal entry or an open item, and since nothing in SAP is wrong
the run is not refused for one.
- The scope sets the leading ledger's lines with BSTAT `S` aside
  (`sap.FilterScope`, `Extract.Noted`), so nothing that reads the journal
  lines posts, sums or counts one: the quality gate's document checks, the
  load in both modes, the opening entry, the open items and the
  reconciliation's SAP side. The report's `extract.acdoca` counts the
  journal lines without them.
- The quality gate reports them as `warning`s (`qualityCheck.noted`): per
  company code how many lines in how many documents were left out and how
  many are open, and each open one (AUGBL blank) by document and line with
  its vendor (number and name), indicator, posting date and amount, in
  its document currency too where that is not the company's. The open requests still
  wanted are entered on the platform as down payment requests; they are
  not migrated as payables requests (no ADR or tracker step asks for it:
  T72c takes over paid down payments, special G/L A, only).
- Only `S` is read. Which other values of BSTAT the pilot's ACDOCA holds,
  and whether it holds noted items at all, is asked of the SAP specialist
  (docs/migration.md, "What to check on a real SAP system").
