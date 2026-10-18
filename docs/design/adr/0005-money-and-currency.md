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
