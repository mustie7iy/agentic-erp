# ADR-0042: Foreign-currency revaluation

**Status:** accepted (T48, product owner 2026-10-19)

## Context
An open item in a foreign currency is booked in local currency at the rate
of its posting date. At a period end the balance sheet must show it at the
rate of that date (IAS 21.23: monetary items at the closing rate), and the
difference is an unrealised gain or loss until the item is paid, when
payments realise the actual difference (`fx_gain`/`fx_loss`, already
built). SAP does this with the foreign currency valuation run (FAGL_FCV):
at a key date it values open items of reconciliation accounts and the
balances of G/L accounts marked for valuation, posts the differences to
adjustment and unrealised gain/loss accounts (T030S/T030H) and reverses
them on the first day of the next period. ADR-0005 already gives every
journal entry its document currency (`journal_entry.currency_tc`) and
every line `amount_tc`, `amount_lc` and `amount_gc`, converted once at
posting.

## Decision
- **Document currency stays on the entry header.** An entry has one
  transaction currency (`journal_entry.currency_tc`); lines do not get a
  currency of their own (no `journal_line.currency_tc`). Every posting path
  (other contexts' postings, reversals, manual and parked entries,
  clearings, down payment clearings, settlement, GR/IR) posts a document in
  a currency other than local currency, converted at the posting date's
  rate (`TestDocumentCurrencyOnEveryPostingPath`, `TestForeignExchange`,
  `TestGRIRForeignCurrency`).
- **What is revalued.** Open items on supplier and customer reconciliation
  accounts, and the balance per currency of G/L accounts flagged
  `revalue_foreign_currency` (`ledger.company_account`; assets and
  liabilities that reconcile no partner, such as a foreign-currency bank
  account; equity is non-monetary, IAS 21.16, and not revalued). Special G/L items (down payments, ADR-0037) and the GR/IR
  account are not revalued: down payments are advances, non-monetary under
  IAS 21.16, and GR/IR is valued with its order items.
- **Rates.** One valuation rate type per legal entity
  (`companies[].fx_valuation.rate_type`, default `average`), kept in
  `ledger.fx_valuation`. The rate is the latest of that type valid on the
  key date; one whose `valid_from` lies before the key date's period is
  stale and refused (422 `ledger.rate_stale`), as is a missing one. Rates
  are loaded by operators through `/v1/exchange-rates`; feeds come later.
- **Accounts.** Per legal entity and revalued account
  (`companies[].fx_valuation.accounts`, `ledger.fx_valuation_account`): an
  unrealised gain (a revenue) and an unrealised loss (an expense), separate
  settings from the realised `fx_gain`/`fx_loss` though they may name the
  same accounts, and for a reconciliation account a balance-sheet
  adjustment account that reconciles nothing, since a reconciliation
  account is posted only through its partners' items. A flagged G/L
  account takes its difference itself. The database keeps these rules
  (`fx_valuation_account_*`, `company_account_revalue_foreign_currency*`);
  configuration validation reports them first.
- **The run.** Per legal entity and ledger (`ledger_id` is a key from the
  start, ready for parallel ledgers in T52) at a key date that is the last
  day of a posting period. It values every item at the key date's rate
  against its booked local amount, posts one valuation entry dated the key
  date and one reversing entry dated the next day, the first of the next
  period, both periods open (T32), and records the run with its items
  (`ledger.revaluation_run`, `ledger.revaluation_item`). The reversal is a
  separate entry linked by the run, not a `reversal_of` reversal, so the
  original items stay open and the next period starts from their booked
  amounts (SAP's reset). A run without differences posts no entries.
- **Valuation (T48b).** An item is open at the key date when posted on or
  before it and neither cleared by a clearing nor reversed by a reversal
  dated on or before it; only
  items in a currency other than local currency are valued, and a flagged
  account's balance only where its transaction-currency balance is not
  zero. Each is valued `round(amount_tc × rate)` against its booked local
  amount, rounded once; a positive difference is a gain, a negative one a
  loss, and its group currency amount is the local difference at the key
  date's average rate, the rate the valuation entry posts at (not checked
  for staleness, unlike the valuation rate). The key date must be the last day of a (non-special)
  posting period of the ledger (422 `ledger.key_date_not_period_end`). An
  account with items in scope and no active valuation fails the run and
  the preview closed (422 `ledger.revaluation_account_missing`, naming
  it), rather than leave its differences unposted. Items are read from
  their journal lines, so an item's account and partner are its line's.
  `POST /v1/revaluation-runs/preview` returns the items and their totals
  per account, currency and direction.
- **One posted run per key date.** A partial unique index on (tenant,
  legal entity, ledger, key date) where `status = 'posted'`; a second run is
  409 `ledger.revaluation_exists`. To correct a run, cancel it (which
  reverses both its entries) and run again.
- **Transactions and access.** A run is synchronous, one transaction:
  items are read, valued and posted together, and the run's open items are
  read so that a concurrent clearing waits (ADR-0008). Running and
  cancelling are `x-agent: false`; the preview, which values without
  posting, is open to agents. Each run and cancellation writes an audit
  record (ADR-0011) and publishes `RevaluationRunPosted` or
  `RevaluationRunCancelled` through the outbox (ADR-0007).
- **Valuation in local currency.** The run values local currency; the
  valuation entry's group currency amounts follow from local at the
  posting's rate, as for every posting.

## Out of scope
- Delta mode (valuing against the previous run instead of resetting).
- Separate debit and credit rate types (SAP's rate types per balance sign).
- Rate feeds (ECB and others); rates are loaded by operators.
- Balance sheet regrouping of receivables and payables by sign (SAP F101).
- Translation for consolidation (group currency financial statements).
- Valuation areas per accounting principle, until parallel ledgers (T52).
- Batching large runs as Temporal workflows (ADR-0012); a run is one
  transaction until volumes need otherwise.

## Alternatives
- **A currency per line (`journal_line.currency_tc`):** allows mixed-currency
  entries as ACDOCA does, but every balance check, clearing and report would
  group by line currency; no posting path needs it.
- **Reversal with `reversal_of_id`:** would mark the valuation entry
  reversed and reuse the existing machinery, but a reversal on the next day
  is not a correction of an error; the run links both instead.
- **Delta valuation:** fewer postings, but each run then depends on the
  previous one, and cancelling one would invalidate those after it.

## Consequences
- Steps: a ADR and schema (000091) · b valuation engine and preview · c
  posting run · d cancel and close-out.
- The unrealised result of a period is visible as the balance of the
  unrealised gain and loss accounts and is gone from the next period after
  the reversal.
- A clearing or payment after a run realises the difference against the
  booked amount, as before; the run's adjustment has already been reversed
  by then.
