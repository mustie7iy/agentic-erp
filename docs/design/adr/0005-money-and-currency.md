# ADR-0005: Integer minor units; three currencies on ledger lines

**Status:** proposed

## Context
Money must be exact. Ledger reporting needs transaction, local and group
currency views (SAP ACDOCA: WSL, HSL, KSL).

## Decision
- Amounts are `bigint` minor units with an explicit currency code; the
  currency's exponent (ISO 4217) lives in the reference table `ref.currency`.
  `bigint` holds ±9.2×10^18 minor units, enough for any single amount.
- In the API, amounts are decimal strings with currency:
  `{"amount": "1234.56", "currency": "EUR"}`.
- Journal lines store three amounts: `amount_tc` (transaction currency),
  `amount_lc` (legal entity currency), `amount_gc` (group currency), each
  with its currency code and the exchange rates used.
- Exchange rates are `numeric(28,12)` with rate type and validity date.
- Quantities are `numeric(20,6)` with a unit of measure.

## Alternatives
- **numeric(23,4) amounts:** simple, but scale fixed for all currencies and
  slower arithmetic; still a valid choice — rejected for consistency with the
  Go domain model (integer minor units, no floats).

## Consequences
- Every amount column has a sibling currency column; CHECK constraints ensure
  balancing per currency.
- Conversions are done once at posting and stored, never recomputed.

**Amended 2026-10-16 (T42, ADR-0036).** A model call costs fractions of a
minor unit (a few thousand tokens at a few dollars per million). Such
costs are `money.Micro`: integer millionths of the currency's minor unit,
summed exactly and shown in minor units rounded half up. They are never
mixed with `money.Minor` amounts without that conversion, and never
floats.

## As built: SAP open items in a foreign document currency (review:batch20, 2026-10-29)
Decided by default under ADR-0053 §d, from the load as built (T72, T72c, T72f)
and T72b's never-guess rule:
- **What the extract carries.** Each Universal Journal line (ACDOCA) carries
  its document currency and amount in it, RWCUR and WSL, beside the company
  code amount HSL, in the SAP file extract and the mock (every mock document
  is in its company's currency). SAP names WSL "Amount in Transaction
  Currency" (SAP KBA 3788925); that RWCUR is its currency key is from the
  ABAP dictionary as published mirrors show it, for an SAP specialist to
  confirm. The OData mapping does not carry them until the API's properties
  are confirmed against a system's `$metadata`.
- **Refused, not converted.** The load posts every migrated line in company
  code currency (transaction currency = local currency), so an open item in
  another document currency would become a local-currency item that a
  payment or final invoice in its own currency could not clear
  (`ledger.mixed_items`). The quality gate refuses an open item whose RWCUR
  is not the company's currency, naming its document and line: a vendor
  line SAP has not cleared on a supplier reconciliation account (an open
  invoice, credit or down payment) and a line of a GR/IR position the load
  takes over. An open item without RWCUR is refused too (an OData extract's
  are), rather than assumed to be in the company's currency. Lines SAP
  reports as cleared (AUGBL), lines of no amount and lines on other
  accounts are not checked: they post in local currency, as before, and a
  cleared line the run's clearing completes is cleared on the platform too.
- **Not built then.** Migrating foreign-currency open items in their
  document currency was not that step; the product owner asked for it on
  2026-10-31 (as recommended), built as the next section says.

## As built: SAP open items loaded in their document currency (product owner 2026-10-31)
Decided by default under ADR-0053 §d, from this ADR's three amounts per line
and the load as built:
- **What the platform stores.** An SAP document whose lines are all in one
  document currency (RWCUR) other than the company's posts in that currency:
  `amount_tc` is SAP's document amount (WSL) and `amount_lc` SAP's company
  code amount (HSL), kept as given and not converted again ("conversions are
  done once at posting and stored, never recomputed": SAP did the
  conversion). `amount_gc` is converted from the local amounts at the
  average rate into group currency, as every posting's, all lines together
  so the group amounts balance. The entry's `rate_tc_lc` is the rate SAP's
  amounts imply, HSL over WSL of the line with the largest document amount,
  to 12 decimals (`ledger.Posting.Rate`): the extract carries no document
  rate (BKPF-KURSF would be SAP's, which the extract does not hold; for an
  SAP specialist to confirm), and the platform's own average rate of that
  day would describe amounts it did not produce. No platform rate of the
  posting date is needed. A line of local amount only (`amount_tc` 0), an
  exchange difference SAP posted in company currency alone, posts as such
  (`PostingLine.LC`; the ledger refuses a line of no amount in either
  currency).
- **Which documents.** In `documents` mode the documents SAP cleared
  together, directly or through others, post in one currency, since a
  posting has one currency and a clearing clears items of one
  (`ledger.mixed_items`): the currency earlier runs posted those of them
  they loaded in, read from their entries (an entry is never posted again),
  so a final delta run clears with a USD payment the USD item an earlier
  run loaded; else their document currency when they all share one; else
  company currency, as before (a USD invoice paid in CAD). The gate refuses
  a document that would have to clear with earlier runs' documents in a
  currency it is not in (a USD item an earlier run loaded, paid in CAD
  since), naming the currency, rather than leave the clearing to fail in
  the load. In `balances` mode the documents holding an open item in a
  foreign currency post whole on their own beside the opening entry, as
  down payments do (T72c); the rest are opening balances. A document with
  no document amount on any line posts in company currency.
- **What follows.** The open item is the supplier's item in its own
  currency: a payment run in that currency pays it, a payment or final
  invoice in it clears it, and a revaluation values it as one posted on the
  platform (tests in ledger and integration). A down payment's paid request
  is in its document currency and its tax is checked in it, on the
  assumption that SAP calculates tax in the document currency (for an SAP
  specialist, below).
- **The gate.** It refuses an open item without RWCUR (an OData extract's,
  until RWCUR and WSL are mapped against the pilot system's `$metadata`;
  they then load as a file extract's do), one whose document's lines are not
  all in its currency or whose document SAP cleared together with documents
  in another currency, and a line of an open GR/IR position in a foreign
  currency (not migrated yet: the take-over posts SAP's balance in company
  currency against an order in its currency, ADR-0034 T72f; tracked). For a
  document that posts in a foreign currency it refuses a currency the
  platform does not know (`ref.currency`), a WSL that does not parse, a line
  with a WSL and no HSL or the two of opposite signs, a vendor line without
  a WSL, a document that does not balance in WSL, and an implied rate that
  is not recordable: zero at 12 decimals, or 10^16 and above, which
  `numeric(28,12)` cannot hold (`ledger.RecordableRate`, checked by the
  ledger too); and document amounts that add up, unsigned, to more than an
  amount holds.
- **Reconciliation.** The controls in company currency are unchanged and
  cover these items through HSL. Open items in each foreign currency are
  compared too, per vendor and in total in that currency (`Open items in
  USD total`), where either side has any: the local controls cannot tell an
  item loaded with the wrong document amount.
- **For an SAP specialist.** That SAP calculates a document's tax in its
  document currency, which the down payment's tax check assumes; that an
  exchange difference SAP posts when it clears foreign items is a line with
  HSL and no WSL; and that it values the vendor line of a clearing so the
  cleared items net to zero in company currency: all from memory, and what
  the load takes over (a clearing
  that does not net in both currencies is refused by the ledger, an issue of
  the run); and which field carries the document's exchange rate.

## As built: no open item with a document amount alone is left out (review:batch31)
Decided by default under ADR-0053 §d: the quality gate refuses the line,
naming its document and line, rather than load it. An open vendor item, or
a line of an open GR/IR position, with an amount in its document currency
(WSL) and none in company currency (HSL) was neither refused nor loaded
when its document posts in company currency (its lines not all in one
currency, SAP's clearing mixing currencies, or its currency the company's):
the gate counted an item by its HSL alone, and the load leaves out a line
of no company code amount, in the journal posting (`documents`) and the
opening entry (`balances`) alike, so a small foreign item disappeared.
- **Refused, not loaded.** Loading it is not settled: the section above
  already refuses such a line in a document that posts in a foreign
  currency (a WSL and no HSL), so treating its document as foreign-currency
  would refuse it there; and loading it in company currency would make an
  item of zero, which the ledger refuses. Either way it needs a money rule
  (an item of no local value, a rate of zero) that this ADR does not give.
- **Which lines.** A line counts as an open item when it has an amount in
  either currency, as `openDocuments` already counts them. WSL is read
  without its currency, which may be missing or unknown: not zero, or not
  a number, is an amount. A line that carries no WSL has none when it is in
  company currency (should an extract leave WSL empty on a line in
  company currency); in another currency or none (an OData extract carries
  neither yet) it cannot be told, so the line is refused for its missing
  document currency, T72b's never-guess rule, rather than left out. A line
  whose WSL is zero, or a cleared line, is still not an item.
- **What the gate says.** In company currency: "open item … has no amount
  in company currency (HSL), but its document amount (WSL) is …"; in a
  document not all in its currency, without one, or cleared together with
  documents in another currency, the refusals of the section above apply
  to it as to any open item there; in a document that posts in a foreign
  currency, `foreignLine`'s. Both modes, tested with real PostgreSQL.
- **Its sibling.** A line with HSL and no WSL loads as before: in company
  currency from HSL, and in a foreign-currency document as an exchange
  difference, except a vendor line, refused (section above).
- **For an SAP specialist.** Whether SAP can hold an open item with a
  transaction-currency amount and a company code amount of zero (a small
  amount that rounds to zero at the document's rate, say), and whether a
  line in company currency always has WSL equal to HSL: neither is assumed
  here, which is why the line is refused rather than loaded.

## As built: no line with a document amount alone is left out (review:batch31)
Decided by default under ADR-0053 §d, extending the section above from
open items to every line. In `documents` mode a line that is no open item,
with a document amount (WSL) and no company code amount (HSL), in a
document that posts in company currency (a G/L line of a document whose
lines mix currencies, a cleared vendor line) was left out of the journal
posting: company code balances were right, but its document amount was
lost without a word. A cleared vendor line left out was then named as line
0 in the clearing the load takes over, which the ledger refused as "not a
line on a reconciliation account", failing the whole run after the gate
had passed it; so did a cleared vendor line of no amount at all.
- **Refused, not loaded.** As for open items: loading it needs a money rule
  (a line of no local value) this ADR does not give. The quality gate
  refuses it (`leftOutLines`), naming document and line: "line of SAP
  document … has a document amount (WSL …, RWCUR …) and no amount in
  company currency … (HSL)". So no SAP line with an amount in either
  currency is left out unreported in a document the load posts: an open
  item has its refusal above, a line of a foreign-currency document
  `foreignLine`'s, and every other line this one.
- **Which documents.** Those that post as an entry of their own in company
  currency: in `documents` mode every document not in a foreign currency;
  in `balances` mode a down payment's document not in one (T72c). The other
  documents of `balances` mode make up the opening entry, which carries
  company code balances per account and no line of its own (T72c), so no
  document amount is carried there for any line: not refused. A document
  an earlier run posted is not posted again, and one refused as a clearing
  conflict is refused whole: not refused line by line.
- **Which lines.** WSL read as for open items (not zero, or not a number).
  A line that carries no WSL has no document amount to lose and is not
  refused, unlike an open item without a document currency (T72b's
  never-guess rule is about which currency an item is in, and an OData
  extract, which carries no WSL yet, would otherwise be refused for every
  line of no company code amount).
- **Line 0.** A cleared vendor line the posting leaves out now has no
  amount in either currency, so it has nothing to clear: the clearing
  leaves it out (`clearingItems`), and a clearing none of whose vendor
  lines has an amount clears nothing. Tested through the load with real
  PostgreSQL.
- **For an SAP specialist.** Whether SAP posts lines with a WSL and an HSL
  of zero in documents that are not open items (rounding of a small
  foreign amount, an exchange difference posted in transaction currency
  alone), and whether a vendor line of no amount in either currency occurs
  in a clearing: neither is assumed.
