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
- **Which documents.** In `documents` mode a document posts in its foreign
  currency when every document SAP cleared together with it, directly or
  through others, is in that currency: a posting has one currency and a
  clearing clears items of one (`ledger.mixed_items`), and the rule gives a
  document the same currency in every run, so a final delta run clears with
  a USD payment the USD item an earlier run loaded. A set of documents that
  mixes currencies (a USD invoice paid in CAD) posts in company currency, as
  before. In `balances` mode the documents holding an open item in a
  foreign currency post whole on their own beside the opening entry, as
  down payments do (T72c); the rest are opening balances.
- **What follows.** The open item is the supplier's item in its own
  currency: a payment run in that currency pays it, a payment or final
  invoice in it clears it, and a revaluation values it as one posted on the
  platform (tests in ledger and integration). A down payment's paid request
  is in its document currency and its tax is checked in it, where SAP
  calculates it.
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
  rounds to zero.
- **Reconciliation.** The controls in company currency are unchanged and
  cover these items through HSL. Open items in each foreign currency are
  compared too, per vendor and in total in that currency (`Open items in
  USD total`), where either side has any: the local controls cannot tell an
  item loaded with the wrong document amount.
- **For an SAP specialist.** That an exchange difference SAP posts when it
  clears foreign items is a line with HSL and no WSL, and that it values the
  vendor line of a clearing so the cleared items net to zero in company
  currency, are from memory and are what the load takes over (a clearing
  that does not net in both currencies is refused by the ledger, an issue of
  the run); and which field carries the document's exchange rate.
