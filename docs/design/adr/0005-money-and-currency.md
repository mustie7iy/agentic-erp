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
- **Not built.** Migrating foreign-currency open items in their document
  currency (WSL, with their rate) is not this step, and no tracker item asks
  for it yet.
