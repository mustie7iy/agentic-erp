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
  000093-000098; T45c, 06 §6.4), every running version deactivating a
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
  names (`payments.currency_required` otherwise); a run proposed for one
  bank takes only that bank's currency (`payments.currency_mismatch`). In a
  run without a bank, a ranked bank whose account is in another currency
  than the run's is skipped and the ranking's next bank, or the supplier's
  next method, is tried; a supplier, or a down payment request, no ranked
  bank can pay is excluded with `no_house_bank`, not failed (product owner
  2026-10-19).
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
  a connector, and its files wait to be transmitted. T44 is split: the SFTP
  transport and the `bank_sftp` connector's settings (T44a: host, port,
  user, the server's `host_key` in authorized_keys form, pinned, and an
  absolute `directory`; the private key by `secret_ref`, settings never
  hold a password; a file is written under a hidden name and renamed, so
  the bank never collects half a file, and the rename never overwrites a
  file already there, so a retry after a lost acknowledgement is told
  apart by T44c's hash, not by the server; the host key is asked for by its
  type, so a server with keys of several types presents the pinned one, and
  a certificate is refused), a house bank's channel (T44b: the house
  bank's `connector` names a `bank_sftp` connector, checked on submission
  and stored as `payments.house_bank.connector_id`, 000099, with no foreign
  key across contexts; transmission resolves it per bank through the
  integration context's `BankChannels`, which the application wires to
  ports of the payments context, and fails closed: a connector that is
  disabled, not a bank channel, without a `secret_ref` or whose key does
  not resolve is refused and nothing is sent; the key is resolved on each
  use, so a rotated secret needs no restart; `bank_file_transmitted`
  names the connector; a bank that names none keeps the server's
  `DirConnector`, its event's `connector_id` null. The previous version
  sends every file to the directory, so a configuration naming a connector
  waits until the upgrade completes),
  duplicate detection by content hash (T44c: `payments.sent_bank_file`,
  000100, keyed by tenant, house bank and SHA-256, insert-only, records each
  file a bank accepted with the connector it went through, and is the
  upload's audit record (ADR-0011); it is written in
  a transaction of its own committed right after the upload, before the
  request's transaction marks the file transmitted, so a lost commit of that
  state cannot lose it; before sending, a file whose hash is recorded for
  its bank is not uploaded, moves to transmitted, and
  `bank_file_transmitted` says `duplicate: true`. If the record itself
  cannot be committed, or not within its bound (it needs a second pool
  connection while the request holds one), the transmission fails and the
  file stays created:
  the retry uploads it again and the bank refuses the repeated message id,
  as before T44c. The file row is locked `FOR NO KEY UPDATE`, so the
  record's foreign key check does not wait for the request holding it),
  EBICS refused in production (T44d: the `bank_ebics` connector's settings,
  checked on submission: the bank's `url`, https only, and the EBICS 3.0
  `host_id`, `partner_id` and `user_id`, the subscriber's keys by
  `secret_ref`; in production the API refuses to create or enable one
  (`integration.ebics_not_certified`), and the server refuses to start while
  any tenant has an active one, read as `erp_control`, 000101, so a
  connector from before or a change by hand fails closed; a production
  import refuses a tenant with one, as it refuses one without an identity
  provider. No
  house bank can name one yet: `BankChannels` resolves only `bank_sftp`.
  The protocol, T44d2, key initialisation (INI, HIA, HPB) and its state,
  and T44d3, upload (BTU) signed with A006 over HTTPS against an in-process
  stand-in bank, is deferred until a pilot in an EBICS country (product
  owner, 2026-10-19: the pilots are Canada and the UAE, which do not use
  EBICS); what is built stays, the settings validated and the kind refused
  in production. Runbook: a production cell that refuses to start for an
  active `bank_ebics` connector names each one; with no cell serving the
  API, the operator disables each with `erp-server disable-connector
  -tenant <id> -connector <id> -by <operator> -reason <text>`, which needs
  no running server, as `register-provider` (ADR-0031) is the remedy for a
  tenant without an identity provider: it disables the connector as
  `POST /v1/connectors/{id}:disable` does, locked, whatever its version,
  audited as `integration.connector.disable` with the operator's handle and
  the reason (a handle, not an email address: the audit trail refuses
  personal data, ADR-0026), its `connector_disabled` event waiting in the
  outbox until the cell runs; run again on a connector already disabled,
  it reports so and succeeds, so the runbook can be repeated; then the
  cell is started again), and transmission out of the request
  transaction (T44e): `:transmit` takes a `created` or `failed` file and
  marks it `to_send` with its operation and who asked, in the request's
  transaction, as the outbox records an event (ADR-0007), and answers 202;
  the bank file transmitter (`payments.Transmitter`, a platform worker on
  every replica, with or without the workflow engine) finds the tenants
  with due files as `erp_worker`, then per file, in a short transaction of
  the tenant as its own system principal, claims it (`FOR UPDATE SKIP
  LOCKED`), resolves the bank channel, checks the sent-hash record (a
  file its bank took is transmitted without an upload, `duplicate: true`),
  reads the content, and moves it to `sending` (attempt counted, the claim
  lasting five upload bounds, 10 minutes by default); it uploads with no
  transaction open, bounded (2 minutes by default), then, in a second
  transaction guarded by the claim (still `sending` with the same attempt
  count), records the hash and marks the file `transmitted` together,
  publishes `bank_file_transmitted` and completes the operation. The hash
  record and the status now commit together, so T44c's transaction of its
  own is gone; if that commit is lost, the claim lapses and the file is
  sent again, and the bank refuses the repeated message id. A failed
  attempt (a problem of 500 or above, or an error that is no problem) is
  tried again after 1, 5 and 15 minutes, 1 and 4 hours; a refusal (a
  problem below 500: a disabled or unusable channel, a file that no longer
  matches its hash) or the sixth attempt's failure moves the file to
  `failed`, publishes `bank_file_transmission_failed` with the code and
  fails the operation; a failed file may be asked for again, its attempts
  counted afresh. The sweep: a `sending` file whose claim lapsed (the
  replica stopped) is queued again at once, or fails if that was its last
  attempt. A request cancelled before the claim is withdrawn (the file
  returns to `created`, nothing sent); one cancelled while the bank has
  the file keeps the file's outcome; one cancelled while a claim lapsed,
  the outcome unknown, fails (`payments.transmission_outcome_unknown`),
  to be checked with the bank. The outcome of an upload is recorded even
  when the worker is stopping (within 10 seconds), and a stopping worker
  claims no further file. Each step is audited
  (`payments.bank_file.queue`, `withdraw`, `requeue`, `retry`, `fail`,
  `transmit`). The previous version transmits only `created` files and
  reports only `transmitted` ones, so a file it reads in a new status is
  left to the new version's transmitter (06 §6.4; 000102-000104).
  The sweep that removes bank files a cancelled release left in object
  storage is T44f.

## Amendment: Canadian bank accounts (T45d, product owner 2026-10-20)

The pilot countries are Canada and the UAE. The UAE uses IBANs; Canadian
accounts have none, which blocked paying Canadian suppliers. A Canadian
account is an institution number (3 digits), a transit (branch) number
(5 digits) and an account number (7 to 12 digits).

- Supplier bank accounts take the scheme `ca` beside `iban` and
  `domestic`, with `institution_number` and `transit_number`, required for
  `ca` and refused for the others (`partner.invalid_institution_number`,
  `partner.invalid_transit_number`, `partner.invalid_account_number`,
  `partner.routing_not_allowed`). They go through the bank change, its
  segregation of duties and approval, and the payment hold as an IBAN
  does. Only the format is checked (`platform/cabank`): an unknown
  institution is the bank's to refuse, as an unknown BIC is.
- The account number is sealed and only its last four digits shown or
  published, as an IBAN's are. The institution and transit numbers name
  the bank, as a BIC does, so they are kept, returned and published
  (`bank_account_changed`) in clear text. A `ca` account's bank is in
  Canada (`partner.bank_country_mismatch` otherwise). The fingerprint that
  finds one account on two partners is of the account number alone, as
  sealed, so key rotation, which recomputes it from the sealed value,
  keeps it; the same number at two banks is warned about, a look rather
  than a missed duplicate.
- A house bank in the configuration gives either `iban` or
  `institution_number`, `transit_number` and `account_number`, and keeps
  its BIC. Its account number is sealed in `iban_cipher`; the routing sits
  in `payments.house_bank` beside it and, like the IBAN, never changes
  under the same name.
- Bank files stay pain.001.001.03, so one writer serves both: a Canadian
  account is written in `Othr`, and its bank as the clearing system member
  `CACPA` with the member id in the Canadian Payments Association's
  electronic routing form, `0` + institution + transit (9 digits); the
  debtor agent keeps its BIC beside it. Files to IBAN accounts are
  unchanged (a BIC is now left out when empty, which configuration
  validation already refuses for a house bank). CPA-005, the 1464-byte AFT format, is not built: it is a
  fixed-width format of its own (originator id, file creation number,
  header, detail and trailer records) and is a tracker item for banks
  that do not take pain.001.
- The model gateway's redaction (ADR-0036) replaces, for a tenant in
  Canada, accounts written institution-transit-account or
  transit-institution-account with dashes (`[BANK_ACCOUNT]`); undashed
  runs of digits are invoice and order numbers far more often and pass.
- Schema: 000117 adds the columns and checks NOT VALID (the scheme check
  widened to `ca`; `bank_account_ca_routing`, `house_bank_ca_routing`),
  000118 validates them (06 §6.4). The previous version reads `ca` as an
  unknown value of the open enumeration and would write its account in
  `Othr` without the clearing member, which the bank refuses; an old
  binary applying a `ca` change a new one requested fails on
  `bank_account_ca_routing` rather than writing an account without its
  bank. No Canadian account is entered until the upgrade completes. The
  SAP load (vendor accounts, T012K) still takes accounts as IBAN or
  domestic: loading Canadian ones with their routing is a tracker item.

## Amendment: foreign-currency house banks (T53a, ADR-0051 §3, product owner 2026-10-21)

A house bank was always in its company's currency, so a Canadian
company's USD account, or a UAE company's, could not be a house bank: its
statements were refused as not of the bank's currency. The product owner
decided (ADR-0051, P2) that a house bank may hold another currency.

- **Configuration.** `companies[].house_bank(s)[].currency` (ISO 4217;
  empty is the company's currency) is stored in the existing
  `payments.house_bank.currency`; no migration. It is posting-related
  (tier 3), as every house bank field is. Validation refuses a currency
  the reference data does not know, a foreign-currency bank whose
  clearing, main or incoming account is not flagged
  `revalue_foreign_currency` (ADR-0042: so the period-end valuation
  covers its balance, and the flag needs the account's valuation
  accounts), and a ranking (`payment_methods[].ranking[]`) that names a
  bank for another currency than its own: such a ranking never paid,
  since the routing skips banks of another currency, and is now refused
  rather than ignored. No account is shared between house banks at all,
  so none is shared between banks of different currencies.
- **A currency never changes under a code**, as an IBAN does not: the
  check refuses a document that gives an existing bank another currency
  ("already has another currency, EUR; give the new bank account a new
  name"), and the apply never rewrites a bank's currency. A bank of a
  document from before T53a has no currency and keeps its company's.
- **Payments.** A run proposed for the bank takes its currency; a run
  without a bank pays from the ranked banks in the run's currency (T45c,
  unchanged). The release posts in the run's currency at the run date's
  rate, as for any document currency (ADR-0042). The bank file's debtor
  account names its currency (`DbtrAcct/Ccy`) when the bank's currency is
  not its company's, so a bank keeping several currencies under one
  account number debits the right one; files of local-currency banks are
  unchanged, byte for byte.
- **Statements and reconciliation** (ADR-0047). A statement must be in
  the bank's currency (unchanged); its lines post at the posting date's
  rate with the realised difference on `fx_gain`/`fx_loss` (T49f,
  unchanged). The reconciliation compares in the bank's currency: the
  ledger's balance of the main bank account is, for a bank in the local
  currency, its local amounts (as before), and for a foreign-currency
  bank the transaction amounts of the entries in the bank's currency
  (`ledger.Accounts.Balances` takes the currency), so exchange
  differences and the period-end valuation, both in local currency, are
  not unexplained. The amount rule's tolerance still applies only to a
  statement in the company's currency; a foreign-currency statement is
  matched exactly (ADR-0047, accepted 2026-10-20).
- **Canadian accounts** (T45d) work alike in any currency: a Canadian
  company's USD account is written in `Othr` with the bank's `CACPA`
  member id.
- **Known limits** (review 2026-10-21, for the product owner): the
  valuation of a flagged clearing account posts on the account itself,
  so at a month end with payments in transit its valuation and reset
  lines stay open items there beside the payments' lines; the statement
  posting clears only the payments' lines, and readers of in-transit
  payments (T53c) take payment lines only. An entry in another currency
  posted by hand to a foreign-currency bank's main account (a CAD fee on
  the USD account) is not in the bank's USD balance, so the
  reconciliation report does not show it as unexplained; refusing such
  postings would be a database rule of its own, not built. The SAP load
  (T012K) carries no currency, so an SAP foreign-currency house bank is
  loaded in its company's currency until the extract maps it (tracked);
  since a currency never changes under a code, check it before go-live.
- Tested end to end on a CAD company with a USD bank: configuration, a
  USD run, its file, the USD statement imported, matched, posted with the
  realised loss, reconciled, and the bank account revalued at the month's
  end (`TestForeignCurrencyHouseBankEndToEnd`).
