# ADR-0038: House banks, payment methods and bank transmission

**Status:** accepted (T45/T44, product owner 2026-10-19)

## Context
A company has had one house bank, its ledger account the account
determination's `bank`, and a payment run paid every supplier through the
house bank its proposer chose. Companies pay through several banks: per
currency, per country, or to keep a bank's limits; SAP keeps several house
bank accounts per company code (T012K), each with its own G/L account, and
lets the payment program choose one per payment from the supplier's payment
methods (LFB1-ZWELS) and the bank ranking (T042E/T042I). Files went to the
bank only through `DirConnector`, a directory a transfer agent collects
from; banks also take files by SFTP and, in Germany, Austria, Switzerland
and France, by EBICS.

## Decision
- **House banks.** A company has a main house bank (`house_bank`) and
  further ones (`house_banks`). Each has its own ledger account, a bank
  clearing account: the payments the run releases credit it, and the
  statement's debit later clears it to the bank (T49). The main bank may
  name one (`gl_account`) and otherwise keeps the account determination's
  `bank`, so documents from before stay valid; a further bank must name
  one. No two house banks of a company share an account, so a clearing
  account's balance belongs to one bank: the configuration refuses it, and
  the schema enforces it for active banks (`house_bank_account_once`,
  000092-000097; T45c, 06 §6.4), every running version deactivating a
  replaced bank before it activates its replacement. A version from before
  T45c does not write `active_gl_account_id`, so its house bank changes are
  refused by the check while it still runs: configuration changes wait
  until the upgrade completes (06 §6.4). The rule is a `DEFERRABLE
  INITIALLY DEFERRED` unique constraint, so one configuration may swap two
  banks' accounts. PostgreSQL attaches a constraint only to a non-partial
  index, so a nullable `active_gl_account_id` (the account while the bank
  is active, null otherwise; added without a default, written by the
  application, backfilled, then tied to `gl_account_id` and `status` by a
  check added `NOT VALID` and validated) carries it: indexed `CONCURRENTLY`
  on `(tenant_id, legal_entity_id, active_gl_account_id)` and attached with
  `ADD CONSTRAINT … UNIQUE USING INDEX … DEFERRABLE`, so no step rewrites
  the table or holds a lock while it scans. No house bank's account takes
  tax.
  A house bank is identified by its code; its IBAN and BIC never change
  under that code, and a code the document drops becomes inactive.
- **Payment methods.** The methods a supplier may be paid by come from its
  company data (SAP LFB1-ZWELS), not from the run. The configuration ranks,
  per method and currency, the house banks that pay with it. A proposal
  takes no house bank: it pays each supplier through the first method of
  its company data that a ranked bank can pay, and the release writes one
  bank file per house bank and method. The run's `house_bank_id` becomes
  nullable in the API and in the schema (a breaking change, accepted in
  `api/breaking-accepted.txt`); a run proposed for one bank keeps it and
  pays everyone through it, as before, with no method on its payments or
  its one file. A run without a bank is proposed in a currency, which it
  names; a supplier, or a down payment request, no ranked bank can pay is
  excluded with `no_house_bank`, not failed.
  The previous version reads a run without a bank as the zero id and
  cannot release it, so proposals without a bank wait until the upgrade
  completes, as configuration changes do (06 §6.4).
  Per-bank amount limits and cheques are out of scope.
- **Transmission.** SFTP (`pkg/sftp`) and EBICS connectors behind
  `payments.Connector`, one per house bank, with credentials by secret
  reference. EBICS is built now but refused in production until it has
  been certified with a bank: the server fails to start (fail closed). A
  file whose content hash was already sent counts as sent, so a retry after
  a lost acknowledgement never sends a payment twice. The sweep that removes
  bank files a cancelled release left in object storage stays.

## Alternatives
- **One ledger account for all house banks:** simpler setup, but the
  account's balance could not be matched to any one bank's statement.
- **The run's proposer chooses the bank, as before:** keeps the proposal
  simple, but a supplier paid in another currency or country would need a
  run of its own, and the choice would be the proposer's, not policy.
- **Duplicate detection by the file's message id:** the bank checks it, but
  a file rendered again after a lost acknowledgement gets the same content
  and hash, and a hash also covers connectors that never see the id.

## Consequences
- Configuration: `companies[].house_banks` and `gl_account` (T45a); payment
  methods and the ranking (T45b); suppliers' methods in partner company data
  (T45b); proposals and releases per bank and method (T45c).
- Payment methods are the payments context's (`payment_method`,
  `bank_ranking`, the ranking's banks by code); a supplier's methods are a
  list on its partner company data. The partner context checks them through
  a `PaymentMethods` port the application wires to the payments context, as
  it checks accounts through the ledger's, so no package import changes
  direction; like withholding tax codes, a configuration cannot take away a
  method a supplier or a pending partner change has.
- Each bank clearing account must be in the chart of accounts as a plain
  asset account; the account determination's `bank` stays the main bank's
  account where the main bank names none.
- Transmission is per house bank (T44); a release still completes without
  a connector, and its files wait to be transmitted.
