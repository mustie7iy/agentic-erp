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
  the company's run order that the supplier's company data allows and a
  ranked bank can pay (amended 2026-11-10 on the SAP specialist's answer
  A4(c): first this said "the first method of its company data", taking
  ZWELS's letter order as the supplier's preference, which it is not; see
  "As built: the run order of payment methods"), its items that fix a
  payment method by that method, in a payment of their own (product owner
  2026-11-10; "As built: an item's fixed payment method"), and the release
  writes one bank file per house bank and method. The run's `house_bank_id` becomes
  nullable in the API and in the schema (a breaking change, accepted in
  `api/breaking-accepted.txt`); a run proposed for one bank keeps it and
  pays everyone through it, as before, with no method on its payments or
  its one file. A run without a bank is proposed in a currency, which it
  names (`payments.currency_required` otherwise); a run proposed for one
  bank takes only that bank's currency (`payments.currency_mismatch`). In a
  run without a bank, a ranked bank whose account is in another currency
  than the run's is skipped and the ranking's next bank, or the next method
  of the run order that the supplier allows, is tried; a supplier, or a down payment request, no ranked
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
  transaction guarded by the row version its claim wrote (every write to
  the file moves it, so a claimant whose claim lapsed records nothing; the
  status and attempt count cannot tell claims apart, as a file failed
  while a stalled claim was out and asked for again has a new claim
  `sending` at attempt 1 too, whose outcome the stalled one recorded,
  leaving the new operation never completed: T53h review,
  `TestTransmitterStaleClaim`; a finish whose commit applied
  unacknowledged is recorded and published once, as the transmitted file
  is no longer due, and a finish run again on the same claim would find
  the version moved, `TestTransmitterUnacknowledgedFinish`),
  records the hash and marks the file `transmitted` together,
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
  (T012K) carried no currency, so an SAP foreign-currency house bank was
  loaded in its company's currency; it now takes the account's currency
  (As built: SAP house banks in their account's currency, below).
- Tested end to end on a CAD company with a USD bank: configuration, a
  USD run, its file, the USD statement imported, matched, posted with the
  realised loss, reconciled, and the bank account revalued at the month's
  end (`TestForeignCurrencyHouseBankEndToEnd`).

## Amendment: two-person release of large payment runs (T72m, product owner 2026-10-22)

The walkthrough found `payments.release_approvals` validated but never
enforced; T72l made validation refuse it above 1 until the product owner
decided how several people sign. Decision (2026-10-22): a payment run
above a per-company amount needs an approval request, decided by a second
person who is neither the proposer nor the releaser, **before** `:release`
is allowed; at or below the amount one releaser who is not the proposer
suffices, as before. `:reject` needs the release role too (T72l).

- **Configuration.** `companies[].release_dual_limit`, an amount in the
  company's currency, validated with its digits, posting-related (tier 3)
  like the other payment controls and `manual_entry_approval_limit`.
  Optional: a company without one releases every run with one person, so
  runs of documents from before are unaffected and no rate is needed for
  them. `payments.release_approvals` stays, exactly 1, so documents
  activated earlier still parse; its removal is the contract step of
  expand-migrate-contract, tracked.
- **Rollout.** The previous version reads the active document tolerantly
  (`config.ParseActive`, 06 §6.4): it ignores `release_dual_limit`, logging
  it, and its `:release` has no approval check, so on an old replica one
  person would release a run above the limit; the database cannot refuse
  it, since the limit is configuration. A change request that sets or
  lowers a limit is therefore activated only after the upgrade completes,
  as 06 §6.4 has configuration changes wait (an old replica already refuses
  to assess or store a change based on a document with the key, 503
  `config.unknown_keys`, but the first activation is the new replica's).
  06 §6.4 lists the rule; the cell has no switch that refuses
  activation while replicas of two versions run, which would be a
  platform item of its own (built since: "As built: activation refused
  while a replica lacks a key" below).
- **The amount measured.** The run's total, in its currency, converted
  exactly (unrounded, F13) into the company's currency at the average rate
  valid on the **run date** (`ledger.Rates.ExactToCompany`, as manual
  entries and requisitions are measured). The run date is the posting date
  of every payment the release posts, at that same rate (T53a), so the
  amount compared is what the ledger books; the proposal's or the
  invoices' dates would measure another amount than the one paid. A
  company with a limit and no rate on the run date is refused
  (`ledger.no_exchange_rate`), as its release would be. A total above the
  limit (strictly) needs the second person.
- **The request.** A new approvals object type, `payment_run_release`,
  through the existing mechanism: `POST /v1/payment-runs/{id}:submit`
  (human-only, `payments.run.release`) opens it; the caller is checked as
  a releaser (not the proposer, holding the release role) and is its
  requester. One step, one approval by a holder of `payments.release_role`
  with `payments.run.release` (its `x-object-permissions` entry); the
  mechanism keeps the requester and agents from deciding it, and the
  payments context bars the proposer (`Barrer`). Tiers (more approvers for
  larger runs) are not built: one second person is what was decided.
- **The approval does not release the run.** The callback records the
  approval on the run (`release_approval` approved, `release_approved_by`,
  `release_approved_at`) and nothing else: 04 §4.3 keeps the release a
  direct human step, and posting from the approver's transaction would make
  the approver the releaser, which the decision forbids. `:release` then
  reads the run it locks `FOR UPDATE` and refuses, for a run above the
  limit: 409 `payments.release_approval_required` without a request or
  after a rejected or withdrawn one, 409 `payments.release_approval_pending`
  while it waits, 409 `payments.release_approval_outdated` when the run's
  total is now above the total approved, and 403 `identity.sod_conflict`
  for the approver. A rejection of the request rejects the run (product
  owner 2026-10-25): the callback locks the run `FOR UPDATE` and, only
  while it is proposed and waits for that request, records the rejection
  and rejects the run as `:reject` does (its items freed here and in the
  ledger, `payments.payment_run.reject` audited, `payment_run_rejected`
  naming the request), so a new run is proposed rather than the same one
  put to approver after approver. The approver holds
  `payments.release_role` (the request's step), as `:reject` requires; the
  run's proposer, whom the workflow context does not bar from rejecting,
  is refused (403 `identity.sod_conflict`), as on `:reject`. The callback
  run again finds the run rejected by that request and changes nothing. A request withdrawn by
  its requester leaves the run proposed, to be asked for again or
  rejected. Rejecting the run, or releasing it once it no longer needs
  the approval (the limit raised), withdraws a pending request, so
  nothing is approved after the run is decided.
- **A resumed release is not measured again.** A release whose operation
  failed or was cancelled leaves the run `releasing`, some payments
  posted; `:release` resumes it. The release was decided when it started,
  with the approval it needed then, so the resumption checks only that
  its approver is not the one resuming it: measured again, a limit set or
  lowered since, or a moved rate, would strand a half-posted run that
  `:submit` and `:reject` (proposed runs only) cannot reach.
- **Lock order.** `:reject`, and `:release` of a run that no longer needs
  its pending request, lock the run and then the request (withdrawing
  it); a decision locks the request and then the run (its callback).
  Both at once may deadlock: PostgreSQL ends one and it is answered with
  the retryable 503, as for other contexts' withdrawals; nothing is left
  half done.
- **A proposed run's total never grows**: the release only drops payments
  whose items can no longer be paid. The approval is held to the total it
  was asked for anyway (`release_request_total`), so a run that changed
  upward by any path needs a new approval, and one that fell keeps it.
- **Schema (000153, validated by 000154).** `payments.payment_run` takes
  `release_request_id`, `release_approval` (`pending`, `approved`,
  `rejected`, `withdrawn`), `release_request_total`, `release_approved_by`
  and `release_approved_at`, nullable without a default and checked
  `NOT VALID` then validated (06 §6.4): the request's fields are set
  together, an approver exactly when approved, and the approver is never
  the proposer nor, once the run is releasing or released, the releaser
  (`payment_run_release_sod`). Every change is audited
  (`payments.payment_run.submit`, `release_approved`, `release_rejected`,
  `release_withdrawn`); the workflow context publishes `approval_requested`
  and `approval_completed` for the request, and `payment_run_released` and
  `payment_run_rejected` name it in `workflow_request_id`. The callbacks
  change the run only while it waits for that request, guarded in the
  update, so a decision is recorded once (a repeated rejection is a
  no-op, see above).

## As built: the SAP load of Canadian accounts (T72d)

The Canadian amendment (T45d) left the SAP load taking accounts as IBAN or
domestic. T72d loads Canadian ones with their routing, decided by default
under that amendment (ADR-0053 §d):

- **Vendor bank accounts.** An LFBK row with bank country CA becomes a
  scheme `ca` account: the institution and transit numbers from its bank
  key (BANKL) and the account number from BANKN, through the same partner
  checks as any `ca` account. SAP Help describes the vendor's bank as the
  bank country, a bank key ("this may be the bank number", a
  country-specific bank identification) and the account number ("Defining
  Business Partner Banks", SAP Library 4.6C). The bank key is read only in
  the Canadian Payments Association's electronic routing form this ADR
  already writes into pain.001, 0 + institution + transit
  (`cabank.SplitMemberID`, the inverse of `MemberID`): its leading zero and
  fixed order make it unambiguous, where eight bare digits do not say which
  number comes first. Which form a
  given SAP system keeps Canadian bank keys in is its bank master's, so it is
  on the specialist list rather than assumed; a key in another form is
  converted in the extract, not guessed at. **Answered by the SAP
  specialist (po:2026-11-09, A6, [M], to verify on the pilot system):** the
  routing form is the usual one, but legacy data also holds the paper
  (MICR) form `TTTTT-III` (`12345-001`) and sometimes eight digits without
  the leading zero; the hyphenated form converts without guessing
  (`12345-001` is `000112345`) and is to be accepted, ambiguous forms still
  refused (docs/migration.md). **As built (sapvend):**
  `cabank.SplitBankKey` reads either form, the paper one only as five
  digits, a hyphen and three (spaces around it trimmed), so that eight
  bare digits, the institution first or a space for the hyphen stay
  refused; the vendor load (LFBK), the quality gate and the configuration
  draft's house banks (T012-BANKL) all read through it, and the mock's
  Prairie Freight holds the paper form.
- **Quality gate.** A vendor's Canadian account (its first LFBK row, the
  one loaded) whose bank key is not that form, or whose account number is
  not 7 to 12 digits, is an error of the gate (table LFBK), so the run
  stops before it loads anything rather than refusing the supplier at
  load. The issue repeats the bank key, which names a bank and is kept in
  clear text, never the account number.
- **Reconciliation.** A control "Vendor bank accounts: vendors with
  differences" compares, per company code, each vendor's first LFBK row as
  it loads (scheme, bank country, routing, last four characters) with the
  supplier's payments account valid today (`partner.Suppliers.PaymentAccounts`);
  a vendor without one in SAP must have none. It covers every scheme, not
  only `ca`.
- **House banks.** A T012K row without an IBAN whose bank country is CA is
  generated into the configuration with `institution_number` and
  `transit_number` from the house bank's bank key, read as for LFBK, and
  `account_number` from BANKN. A key in another form is noted for the
  reviewers and validation refuses the bank until they enter the routing.
  The extract's T012K carries the house bank's bank country and key
  (T012-BANKS/BANKL) flattened in, as it carries IBAN and SWIFT. A house bank
  outside Canada without an IBAN is unchanged: validation refuses it.
- **OData.** LFBK-BANKL is mapped to `A_BusinessPartnerBank`'s
  `BankNumber`, to be checked against the system's `$metadata` as every
  mapping is.
- **Mock extract.** Company 3000, Maple Industries Canada in CAD, with six
  Canadian vendors and two of company 1000's, and a Canadian house bank, is
  generated after the others, so their data is unchanged; the pilot
  walkthrough migrates it instead of relabelling company 1000.

## As built: payments name their supplier, house banks their Canadian routing (review:T72d)

The API-consumer review of T72d (ADR-0053, REVIEW.md) found a run's payments
giving their payee as a partner id only, and the house bank list leaving out
a Canadian bank's routing. Decided by default:

- **The supplier's name.** `listPayments` gives `supplier_name` beside
  `supplier_id`, the business partner's `name` as it is now, read from the
  partner context through its published port (`partner.Payees.Names`, one
  call per page), never by payments reading partner tables. A payment whose
  supplier the partner context does not hold fails the page (500) rather
  than show a payment without its payee: payments are proposed only to
  partners the context returned, so that is an inconsistency, not a case to
  render. The run's exclusions still give the supplier by id only, beside
  the reason (a pre-existing gap of the same class, for the coordinator).
- **The routing.** `listHouseBanks` gives a Canadian bank's
  `institution_number` and `transit_number` in clear text, as a supplier's
  bank account does (they name the bank, as a BIC does, Amendment T45d), and
  absent for an IBAN; `account_last4` is the IBAN's or the Canadian account
  number's. The account number itself stays sealed.

## As built: a run's exclusions name their supplier (review:batch19)

The review of apinames left the run's exclusions giving their supplier by id
only, beside the reason (REVIEW.md, an identifier without the name its
consumer needs). Decided by default, as for payments above:

- **The supplier's name.** `listPaymentExclusions` gives `supplier_name`
  beside `supplier_id`, required, the business partner's `name` as it is now,
  through the same port (`partner.Payees.Names`, one call per page). The
  payments list and the exclusions list share one helper, so both fail the
  page (500) the same way when the partner context does not hold a supplier:
  a run excludes only suppliers whose open items it read, which the partner
  context holds, so a missing one is an inconsistency, not a case to render.
- **One response.** `PaymentExclusion` is returned only by
  `listPaymentExclusions`; no create or get returns it, so there is no other
  response to change. Its other fields already meet the API-consumer
  classes: the amount carries its currency, and the list is cursor-paged
  per run. A request's exclusion gave its request by id only, not by its
  document number; the amendment below adds the number.
- **The walkthrough.** Step 7 prints each excluded supplier's name from the
  API instead of looking each partner up (`name_partners`), and expects the
  posting-blocked supplier the mock extract carries into company 3000 to be
  left out under its name, Massive Dynamic.

## As built: a request's exclusion names its request (review:batch22)

The review of exclnames left a down payment request's exclusion giving its
request by id only (REVIEW.md, an identifier without the reference its
consumer needs). Decided by default:

- **The field.** `PaymentExclusion` gains `down_payment_request_number`, the
  request's `document_number`, named as the contract names a referenced
  document's number beside its id (`purchase_order_id` and
  `purchase_order_number`). It is optional, present exactly when
  `down_payment_request_id` is, so the change is additive (oasdiff: no
  breaking change).
- **The port.** The number is the payables context's, so payments reads it
  through its `DownPayments` port (`Numbers`, one call per page, none for a
  page of suppliers' exclusions alone), as it reads suppliers' names through
  `Payees.Names`, never by joining the payables schema. A request the
  payables context does not hold fails the page (500): a run excludes only
  requests it read from payables, so a missing one is an inconsistency.
- **Other responses.** `Payment.down_payment_request_id` and the open items
  of a payment (`PaymentItem.journal_entry_id`) gave their document by id
  only too; the amendment below adds their numbers.
- **The walkthrough.** It reaches no request's exclusion: its one down
  payment request is migrated already paid and no run proposes an open one,
  so the API tests (both shapes, and pages after the first) are the
  evidence; a fix to a response, not a new capability (ADR-0053).

## As built: payments and their items name their documents (review:batch23)

The review of exclreq left a down payment's payment giving its request, and
a payment's open item its journal entry, by id only (REVIEW.md, an
identifier without the reference its consumer needs). Decided by default:

- **The fields.** `Payment` gains `down_payment_request_number`, named and
  optional as on `PaymentExclusion`: present exactly when
  `down_payment_request_id` is. `PaymentItem` gains `document_number`, its
  journal entry's: the name the contract already gives that number beside
  the same item's key (`OpenItem` and `LedgerReconciliationEntry`, keyed by
  `fiscal_year`, `journal_entry_id` and `line_no`), so not
  `journal_entry_number`. Every entry has a number, so it is required; both
  are response fields, so the change is additive (oasdiff: no breaking
  change).
- **The ports.** The request's number comes through `DownPayments.Numbers`
  and the entry's through a new `OpenItems.Numbers` of the ledger context,
  through the `OpenItems` port payments already reads open items with, so
  no dependency direction changes and no ADR is needed. `listPayments` makes
  one call to each per page, none to payables for a page without a down
  payment. The exclusions' helper became `documentNumbers`, shared by both:
  a document its context does not hold fails the page (500), as for
  exclusions.
- **Other identifiers.** `Payment.journal_entry_id` (the payment's own
  posting), `bank_account_id` and `house_bank_id` are still ids alone;
  outside this change, reported to the coordinator; the amendment below
  names them.
- **The walkthrough.** Step 7 expects the item of TY-5001 in its payment to
  give the invoice entry's document number as `getJournalEntry` gives it.
  No run there pays a down payment request (its one request is migrated
  already paid), so the API tests are the evidence for the request's number:
  both kinds of payment, a request's payment on a page after the first, and
  the database failures of a run that pays one.

## As built: a payment names its posting and its bank (review:batch23 follow-up)

The review of paynums left a payment's own posting (`journal_entry_id`), its
supplier's bank account (`bank_account_id`) and its house bank
(`house_bank_id`) as ids alone (REVIEW.md, an identifier without the name or
reference its consumer needs). Decided by default:

- **The posting.** `Payment` gains `journal_entry_number`, the entry's
  document number, named `<x>_number` beside `<x>_id` as
  `down_payment_request_number` and `purchase_order_number` are. It is
  present exactly when `journal_entry_id` is (a proposed run's payments are
  not posted yet). It is read through `OpenItems.Numbers` in the same call
  as the open items' entries, so a page still makes one call to the ledger.
- **The house bank.** `Payment` gains `house_bank_code`, the `HouseBank`
  resource's `code` (its name in the configuration, unique within the legal
  entity, as `CashPositionBank` names a bank beside its id), present exactly
  when `house_bank_id` is. House banks are the payments context's own table,
  so it is one query per page, no port.
- **The bank account.** The supplier's account is the partner context's, so
  payments reads it through its `Payees` port (a new `BankAccounts`, masked,
  beside `Names`; payments already depends on partner, so no dependency
  direction changes and no ADR is needed). `Payment` gains the fields the
  partner's `BankAccount` resource gives a consumer to recognise an account,
  each named `bank_account_` + that resource's field: `bank_account_last4`,
  `bank_account_scheme` and `bank_account_holder_name`, required (every
  payment has an account); `bank_account_bic`, absent when the account has
  none; and a Canadian account's `bank_account_institution_number` and
  `bank_account_transit_number`, absent for the other schemes. The bank's
  country (`bank_country_code`) is left out: with the supplier's name, the
  last four characters and the bank (BIC or Canadian routing) recognise the
  account, and a consumer that needs the country reads the account. The account
  number stays sealed: `BankAccounts` reads the masked columns only, and
  `BankDetails` alone opens the number, for a bank file. All are response
  fields, so the change is additive (oasdiff: no breaking change).
- **One helper.** The exclusions' and payments' `documentNumbers` became
  `resolve`, generic in what it resolves, shared by the request numbers, the
  entry numbers, the house bank codes and the bank accounts: one call per
  page, none when nothing is referenced, and an object its context does not
  hold fails the page (500), as before.
- **The walkthrough.** Step 7 lists the released run's payments and expects
  every payment to give its posting's number and the RBC house bank's code,
  the payment of TY-5001 its posting's number as `getJournalEntry` gives it,
  and Laurentian Paper's account its scheme `ca`, institution 001 and
  transit 00010, the bank its pain.001 names.

## As built: every resource that names a house bank gives its code (review:batch24)

The review of paybank found `house_bank_id` alone on `PaymentRun`,
`BankStatement`, `BankReconciliation`, `MemoRecord` and `BankFile` (REVIEW.md,
an identifier without the name or reference its consumer needs): an
integration listing a run's bank files called `getHouseBank` once per file.
A search of `api/openapi.yaml` found no other response with the id alone
(`CashPositionBank` already gives `code`; the rest are request bodies and
query parameters). Decided by default, as for `Payment` above:

- **The field.** Each gains `house_bank_code`, the `HouseBank` resource's
  `code`, beside `house_bank_id`: optional and present exactly when the id is
  on `PaymentRun` (absent for a run proposed without one), `MemoRecord`
  (absent for a forecast-only record) and `BankFile`; required on
  `BankStatement` and `BankReconciliation`, whose id is required. Response
  fields only, so the change is additive (oasdiff: no breaking change).
- **Read once.** A page or a single read asks `payments.house_bank` once for
  the codes of the banks its rows name, and not at all when none does
  (`bankCodes`, `withBank`); the report reads the code with the bank it
  already loads. An id the table does not hold fails the response (500),
  never a row without its bank's code. Pages use a new
  `apitypes.PageWith`, `PageOf` with what the page's rows reference read for
  the page's rows alone, never the row past the page.
- **One helper, moved.** `resolve` and `idsOf` moved from payments to
  `internal/platform/idref` (`Resolve`, `Of`, `Kind`), since the cash
  context needs them too (CLAUDE.md: a helper two contexts need lives in
  platform, never copied).
- **Memo records.** They are the cash context's (ADR-0051 §7), which reads
  house banks only through its `Payments` port: the port gains
  `HouseBankCodes`, adapted from `payments.CashPosition.HouseBankCodes`, by
  id and active or not (a record may name a bank deactivated since). Cash
  already reads payments through that port, so no dependency direction
  changes and no ADR is needed.
- **Events.** The event payloads (`api/asyncapi.yaml`) that carry
  `house_bank_id` are not responses and are left as they are.
- **The walkthrough.** Step 7 expects RBC's code on the proposed run and on
  every bank file, step 8 on the reconciled statement and the reconciliation
  report, and step 13 on the memo record Tom creates.

## As built: a migration run names its connector; the cash position's bank code takes the common name (review:batch24)

The review of batch 24 found two more instances of the class above. A
`MigrationRun` gave `connector_id` alone, so a consumer listing runs called
`getConnector` once per run; and `CashPositionBank` named the house bank's
code `code` where every other resource that names a house bank calls it
`house_bank_code`. A search of `api/openapi.yaml` found no other response with
`connector_id` (`MigrationRunCreate` is a request body; `HouseBank` does not
expose its channel's connector). Decided by default, as above:

- **The connector's name.** `MigrationRun` gains `connector_name`, the
  `Connector` resource's `name` as it is now (a renamed connector shows its
  new name on its old runs), required as `connector_id` is: the foreign key
  to `integration.connector` and connectors never being deleted mean every
  run has one. The guarantee is the foreign key (no `ON DELETE`, so
  restrict, 000010): a step that adds a connector delete must keep it, or
  `idref.Resolve` would answer 500 for its runs. A page or a single read
  asks `integration.connector` once for the names of the page's connectors (`connectorsOf`, through
  `idref.Resolve` and `apitypes.PageWith`, in the request's transaction);
  the create answer uses the connector it has just checked, with no query.
  The connectors are integration's own, so no port is involved.
- **`house_bank_code` beside `code`.** Renaming a response field is a
  breaking change, which `/v1` does not take (04 §4.10, ADR-0041). So
  `CashPositionBank` gains `house_bank_code`, required, holding the same
  value, and `code` stays, deprecated as ADR-0041 deprecates an operation:
  `deprecated: true`, `x-deprecated-at: 2026-10-29` and `x-sunset:
  2027-12-01`, more than 365 days later with a month's margin for the batch
  to merge (oasdiff's `--deprecation-days-stable=365` counts from the day
  it runs and refuses a deprecated response property without a sunset), and
  a description pointing at `house_bank_code`. The field is given at least
  until the sunset and its removal is planned for `/v2`. ADR-0041's other
  parts are an operation's: no `Deprecation`/`Sunset` headers, which name a
  whole answer, and no `externalDocs` page, since the one-line move to
  `house_bank_code` fits the description. The banks are still ordered by the
  code; oasdiff reports no breaking change.
- **Events.** `MigrationRunCompleted` and `BankFileTransmitted` carry
  `connector_id` alone; event payloads are left as they are, as for
  `house_bank_id` above.
- **The walkthrough.** Step 3 lists the migration runs and expects every one
  to name the `SAP PRD` connector; step 13 reads the cash position's RBC
  bank by `house_bank_code`.

## As built: a run's cash discount lapses on its company's date (T72n3, product owner 2026-10-31)

- **Decision (product owner 2026-10-31).** The release's check that a
  payment's cash discount is still earned (T36d, 409
  `payments.discount_lapsed`) judges the last day on the paying company's
  date in its time zone (`companies[].time_zone`, T72n1; UTC in a document activated before
  the zone was required, 2026-10-31),
  not on the UTC date: a Toronto treasurer releasing at 22:00 on the last
  day keeps the discount the supplier grants until that day ends for the
  company, and a Dubai company's release after its midnight no longer
  takes it. The paying company is the run's legal entity's.
- **Where.** `recheckDiscounts` in the `:release` request; the company's
  date is read only when a payment takes a discount. A release being
  resumed is still not refused. Details, and why the proposal (judged on
  the run date) needs no change, are in ADR-0051's "As built (T72n3)".
- **The problem.** Its detail names the discount's last day, the company's
  date that judged it and the zone; the contract's descriptions of
  `releasePaymentRun` and `discount_until` say so.
- **Walkthrough.** Not extended: the step changes the time base of an
  existing rule (T36d) rather than adding a capability, and what it
  changes depends on the hour of the release, which the walkthrough
  cannot choose, as its server runs on the host's clock. A lapse at any
  hour would need a back-dated invoice and run competing with step 7's
  run for the same supplier's items, for no evidence of the zone; the
  package tests (`TestDiscountLapseTimeZone`) fix the clock at those
  hours instead.

## As built: a release's refusals name the supplier (review:batch26)

- **Decision.** A release refused because of one supplier's payment names
  that supplier by its number and name, with the id the run's payments
  give: `payments.discount_lapsed`, and both of
  `payments.proposal_outdated`'s refusals (the supplier can no longer be
  paid as proposed, or an item its payment settles is reversed or no
  longer open). Before, the detail gave the supplier's UUID alone, so the
  consumer told to reject the run had to list the payments to learn whose
  payment was refused (REVIEW.md, API-consumer classes: an identifier
  without the name its consumer needs). Settled by the class and by the
  payments' `supplier_name` (batch 22); the number is the business
  partner's, as the order document names its supplier.
- **Where.** `refuse` in `release.go` reads the number and name from the
  partner context's `Names` port (which now gives both; the payments'
  `supplier_name` keeps the name), one call, only when the release is
  refused. The partner context failing, or not holding the supplier, fails
  the release (500) rather than refusing it with an unnamed supplier, as
  a page of payments does (`idref.Resolve`).
- **Contract.** A `detail` text change only: `releasePaymentRun`'s
  description says the detail names the supplier. No new field; oasdiff
  reports no breaking change.

## As built: a proposal too large names the supplier (review:batch27)

- **Decision.** A proposal failing because one supplier's open items, or
  their cash discounts or withholding, add up to more than an amount can
  hold (422 `payments.amount_too_large`, the operation's error) names that
  supplier as a release's refusals do: by its number and name, with its
  id. Before, the detail gave the UUID alone (REVIEW.md, API-consumer
  classes: an identifier without the name its consumer needs). A run
  whose total is too large names the run, as before. Settled by the
  release's refusals above.
- **Where.** The proposal computes without a context, so `propose` and
  `pay` return `supplierTooLarge` with the supplier's id, and
  `paymentsFor` names it (`nameTooLarge`): one partner `Names` call, only
  when the proposal fails, formatted by `supplierLabel`, which `refuse`
  shares. The partner context failing is an error, so the proposal job is
  retried. A supplier it does not hold (a journal line names its partner
  without a foreign key) still fails the run with 422
  `payments.amount_too_large`, naming the supplier by its id: an error
  there would retry the job first in its tenant's queue forever, as
  `build` says of a missing legal entity (review:batch28 round 1).
- **Contract.** A `detail` text change only: `proposePaymentRun`'s
  description names the problem and says its detail names the supplier.
  No new field; oasdiff reports no breaking change.

## As built: payments events name the company; a withholding too large names its item (review:batch28)

- **Decision (events).** Every payments event that gives `legal_entity_id`
  now gives `legal_entity_code` beside it, the company code as
  `LegalEntityCreated` names it `code`: `PaymentRunProposed`, `Released`,
  `Rejected` and `Failed`, `PaymentExcluded`, and the twelve
  `BankStatement*` events (imported, gap closed, found and accepted, file
  refused and changed, line matched, excluded, proposed, proposal decided
  and posted, reconciled). `PaymentExcluded` gives `supplier_number`
  beside `supplier_id`. Before, a consumer alerting a treasurer looked both
  up (REVIEW.md, API-consumer classes: an identifier without the name its
  consumer needs). Decided by default from procurement's expiry events
  (ADR-0034, review:batch27), which name the company and the supplier the
  same way. The fields are optional, as asyncapi's v1 rule and
  `api/eventcompat.go` require of a new payload field; an event published
  before them has none.
- **Where.** `companyCode` reads the code through the `LegalEntities` port
  (tenancy's `Directory`), never once a line: once a run event, once a
  statement import (`statementBank.Company`), once a statement's matching
  (`matcher.company`), once a person's or an agent's decision on a line,
  once a reconciliation that reconciles something, once a fetched file
  refused or changed. A change that does several of these (a decision that
  posts and reconciles, an approval that posts and reconciles) reads it for
  each, a primary-key read at most three times a request. The proposal names its company by the
  legal entity it already read (`build`), so the proposal reads nothing
  more (TestProposalBudget's queries are unchanged). `RefuseFetched`,
  `ChangedFetched` and `AcceptGap` became methods of `Endpoints` to reach
  the port; `fail`, `writeMatch`, `rejectLive`, `postMatch` and
  `reconcileStatements` take the code.
- **A legal entity the tenancy context does not hold.** Payments'
  `legal_entity_id` columns have no foreign key into tenancy. There the
  event goes out without `legal_entity_code` rather than failing the
  change it records: a proposal failing because its legal entity is
  missing (`noEntity`) must still record its failure, or the job is
  retried first in its tenant's queue forever. Procurement's round fails
  instead, because a round is retried idempotently the next time; the
  tenancy context failing is an error in both. `apitest.CompanyNamed`
  checks every payments event the payments and workflows tests read
  against the directory's code.
- **The supplier's name is not in `PaymentExcluded`: a question of
  security posture for the product owner.** The event is marked
  `x-webhook`, and a webhook subscriber passes no permission check: what
  the event carries goes to every subscribed endpoint. ADR-0041 keeps
  partner master data out of webhooks because a partner may be a person
  (`BusinessPartnerCreated` and `Updated` are not offered). The product
  owner's answer of 2026-11-02 keeps supplier names on payments and in the
  payment problems as the record's own content, but for API callers under
  `payments.run.read`; it does not speak of webhooks. Procurement's
  `ContractExpired` and `ContractExpiring` (offered to webhooks) have
  carried `supplier_name` since batch 27 without the question being
  weighed. So nothing settles whether a supplier's name may go into an
  event delivered by webhook; the number, an identifier, is given and the
  name left out. If the answer is yes, `PaymentExcluded` gains an optional
  `supplier_name`; if no, procurement's expiry events should stop giving
  theirs. Answered no by the product owner on 2026-11-05: see ADR-0041's
  As built "no supplier's name in a webhook event", which gives
  `ContractExpired` and `ContractExpiring` a `.v2` without the name;
  `PaymentExcluded` stays as built, with the number alone.
- **Decision (problem).** `payments.withholding_too_large` gave the
  withholding, the cash discount and the amount owed as raw minor units and
  the item as `fiscal year/entry id/line`. Its detail now gives the amounts
  in the run's currency at its scale (`0.97 EUR`; a JPY run's without
  decimals), as the release's refusals give theirs, the item by its
  journal entry's document number, fiscal year and line with the entry's
  id, and the supplier by number and name with its id, as
  `payments.amount_too_large` names it. The supplier's name is the
  payment's own content in a problem (product owner 2026-11-02, above), so
  no permission is checked, as `supplierNamed` checks none. The proposal
  computes without a context, so `takeDiscounts` returns
  `withholdingTooLarge` and `paymentsFor` names it (`nameRefusal`, which
  `supplierTooLarge` shares): a supplier or an entry the other context does
  not hold is named by its id and the run still fails; either context
  failing is an error, so the job is retried.
- **Swept.** `payments.statement_amount_differs` gave the two amounts
  without their currency; it gives the statement's currency now. Left as
  they are and listed for the coordinator (the same classes, no consumer
  asked): the payments events give `house_bank_id` without the house
  bank's code; the bank file events give `payment_run_id` without the
  run's document number and no company; `payments.no_statement_channel`
  names the house bank by its id.
- **Contract.** asyncapi: the optional fields and the examples.
  openapi: `proposePaymentRun`'s description says what the problem's detail
  names. No breaking change (eventcompat, oasdiff).

## As built: SAP house banks in their account's currency (T53a review)

The SAP load of house banks gave every bank its company's currency, so an
SAP foreign-currency account would have been created in the wrong one, for
good: a currency never changes under a bank's code (amendment T53a). Decided
by default, from the T53a amendment and the configuration draft's pattern
(`sapconfig`: a value SAP does not give is noted and, where a guess could be
wrong, left in a form validation refuses):

- **The currency.** The extract's T012K carries the account's currency key,
  WAERS (check table TCURC; the field as the ABAP dictionary's published
  mirror shows it, https://leanx.eu/en/sap/table/t012k.html, to be confirmed
  by an SAP specialist). House banks enter the platform through the
  configuration generated from SAP (`erp config generate`, then provisioning
  or a change request), not the `erp migrate` run, so the draft is where
  the currency is taken: an account in the company's currency leaves the
  bank's `currency` empty (the company's, as before); another is taken as
  it is, with a note for the reviewers.
- **Refused, not guessed.** An account without a currency is drafted with
  `currency: UNKNOWN` and a note, which validation refuses ("not in the
  currency reference data") until a reviewer sets it, as `net_days: -1`
  marks a payment term SAP's days cannot give; a currency the reference data
  does not know is refused the same way. What an empty WAERS means in SAP is
  for the specialist; the company's currency would be a guess that could
  never be corrected. **Answered by the SAP specialist (po:2026-11-09,
  A5):** WAERS is the account's currency [H]; an empty one means no
  currency was maintained, so SAP does not restrict the account, and the
  binding currency is its G/L account's (T012K-HKONT, SKB1-WAERS), usually
  the company code's [M, to verify on the pilot system]. **As built
  (sapvend):** an account without WAERS is drafted in its G/L account's
  currency, the extract's SKB1 WAERS of T012K-HKONT (sap.SKB1.WAERS,
  optional), noted for the reviewers with the answer's confidence and
  FI-BAM (FCLM_BAM_AMD) as a possible cross-check, which the draft does
  not read; an account in the company's currency then needs nothing more,
  one in another is noted as T012K's would be. `UNKNOWN` remains, noted,
  where neither T012K nor SKB1 gives a currency.
- **Revaluation.** A foreign-currency bank's ledger accounts must be
  flagged `revalue_foreign_currency` with their unrealised gain and loss in
  `fx_valuation.accounts` (T53a, ADR-0042). SAP's valuation accounts are not
  in the extract, so the draft does not flag them: its note says so, and the
  draft validates once a reviewer has.
- **No reconciliation control.** The configuration is checked by
  validation and approved through its change request; the migration run's
  controls cover the data it loads (vendor bank accounts among them, T72d),
  not configuration, so no control was added. The run's report still counts
  the extract's T012K rows.
- **Mock and walkthrough.** The mock's company 3000 has a second house bank
  account, MUSD, in USD on its own ledger account 102000, beside exchange
  gain and loss accounts 480000 and 680000. Walkthrough step 1 shows the
  draft refused until the reviewer revalues 102000, then lists the
  provisioned house banks through `listHouseBanks`: MAIN in CAD, MUSD in USD.
- **Still open** (tracked, if the product owner asks): entries in another
  currency posted by hand on a foreign-currency bank's accounts.

## As built: SAP payment methods, bank ranking and suppliers' methods (T45b follow-up, pilot)

The configuration draft generated from SAP (`erp config generate`,
`delivery/sapconfig`) had no payment methods, and the partner migration
loaded suppliers without theirs, so a migrated company could only pay
through a run proposed for one house bank. Decided by default, from the
decision above and the draft's pattern (a value SAP does not give is noted
and left in a form validation refuses; what the configuration has no
counterpart for is noted):

- **Which tables.** The leaf named the ranking "T042E/T042I". The ABAP
  dictionary's published mirror shows the ranking elsewhere: T042E is the
  company code's specifications per payment method ("Company Code-Specific
  Specifications for Payment Methods": ZBUKR, ZLSCH, the minimum and
  maximum amount VONBT/BISBT, …), T042A the bank selection ("Bank selection
  for payment program": ZBUKR, ZLSCH, WAERS, the ranking order RANGF, HBKID)
  and T042I the account determination ("Account determination for payment
  program": ZBUKR, HBKID, ZLSCH, WAERS, the account HKTID and the bank
  subaccount UKONT); T042Z holds the methods per country (LAND1, ZLSCH,
  TEXT1) (https://leanx.eu/en/sap/table/t042e.html, …/t042a.html,
  …/t042i.html, …/t042z.html). SAP Help ("Selecting the House Bank",
  https://help.sap.com/saphelp_globext607_10/helpdata/en/01/a9bf27455711d182b40000e829fbfe/content.htm)
  says the payment program finds the house bank by method and currency,
  falls back to the entry without a currency, finds the account by bank,
  method and currency, and pays from the bank of highest priority in the
  ranking. So the extract carries T042Z, T042E, T042A and T042I, and the
  ranking is T042A's. All four, and that a lower RANGF ranks first, are on
  the specialist list. **Answered by the SAP specialist (po:2026-11-09,
  A4):** the tables and fields are as stated, T042I included [H]; a lower
  RANGF ranks first [H]; a T042A row without a currency applies to every
  currency without a row of its own [M].
- **The draft.** A company's methods are its T042E rows, each named by the
  T042Z row of the company's country (T001 LAND1) and ranking, per currency,
  the banks T042A ranks for it, in RANGF order (an order that is not a
  number ranks last, noted). A T042A row without a currency, which SAP uses
  for any currency the method has no row for, ranks its bank for its
  account's currency, the only one a house bank pays in (ADR-0051 §3),
  unless the method ranks that currency with rows of its own, which SAP
  would take instead; both noted.
- **Refused, not guessed.** A method T042Z does not name is drafted without
  a description, a method T042A ranks no bank for without a ranking, a
  ranked bank the company does not have (no T012K account) or ranked for
  another currency than its account's is kept: each noted, and validation
  refuses the draft until a reviewer corrects it. T042A rows of a method
  T042E does not set up are left out, and a company without T042E rows has
  no methods, each noted.
- **No counterpart, noted.** T042E's minimum and maximum amount per method
  (per-bank and per-method amount limits stay out of scope, above), and a
  T042I bank subaccount (UKONT) that is not the house bank's ledger
  account: SAP posts a payment by the method to the subaccount, the
  platform credits the bank's one clearing account for every method.
  T042I's account ID (HKTID) is not used: the extract takes one account per
  house bank (T012K).
- **Suppliers' methods.** LFB1-ZWELS ("List of the Payment Methods to be
  Considered", DZWELS, CHAR 10; https://leanx.eu/en/sap/table/lfb1.html),
  one character per method, blanks skipped, loads in its order as the
  supplier's `payment_methods` in the company, on creation and when an
  earlier run's supplier is set up in a further company. The quality gate
  refuses a run whose vendor lists a method the company's active
  configuration does not have, or one twice, before anything loads, as it
  refuses unknown payment terms; the partner context checks the same again
  (`PaymentMethods` port). That ZWELS's order is SAP's order of preference
  is on the specialist list. **Answered (po:2026-11-09, A4(c), [M], to
  verify on the pilot system): it is not.** SAP's payment run tries the
  methods in the order of its own parameters per company code, restricted
  to those ZWELS allows, and an item's fixed method (BSEG/ACDOCA-ZLSCH)
  wins over both; T042I's account (HKTID) tells which T012K account a bank
  pays from by method and currency. The Decision's "first method of its
  company data" is changed ("As built: the run order of payment methods",
  below); an item's fixed method and the draft's one account per house
  bank are to change (a tracker leaf each). The OData source maps it to
  `A_SupplierCompany`'s `PaymentMethodsList` (SAP Cloud SDK,
  https://help.sap.com/doc/505cd8d2cbd340a38eeae8b63b5df753/1.0/en-US/interfaces/_sap_cloud_sdk_vdm_business_partner_service.suppliercompanytype.html);
  the T042 tables, like T012K, come from file extracts.
- **Lock order (review rounds 1-2).** A supplier given methods takes the
  partner context's method lock, and the load then draws numbers from
  configuration ranges; an activation locks the ranges first and the method
  lock after (ADR-0008). So a run holds the active configuration
  (`configuration.Settings.Hold`) before it reads the configuration: the
  tenant's activation lock, an advisory lock it takes shared and every
  activation takes exclusively before it locks any range or applies any
  section. An activation under way is waited for; an activation while a
  run extracts and loads waits for the run's transaction up to the
  requests' lock timeout and is then refused with the retryable 409
  `request.in_progress`, instead of one of the two being aborted as a
  deadlock. Only activation is held back: drafting a change request,
  applying a country pack or building a regression suite takes no part
  of the lock. A run also loads under the configuration it checked.
- **No contract change.** The run's report counts the tables a run reads;
  the T042 tables are read only by the draft, so its extract counts are
  unchanged, and the API already returns a supplier's methods
  (`supplier_companies[].payment_methods`) and the configuration's.
- **Mock and walkthrough.** Every mock company pays by bank transfer T
  through each house bank in its account's currency; company 3000's vendors
  list T in ZWELS. Walkthrough step 1 shows the T042 rows and the drafted
  method, then the method's ranking in the active configuration through
  `listConfigVersions` (T: MAIN for CAD, MUSD for USD); step 3 shows the
  migrated supplier Laurentian Paper's methods through `getBusinessPartner`.


## As built: the run order of payment methods (A4(c), first part, pilot)

The SAP specialist answered (po:2026-11-09, A4(c), [M], to verify on the
pilot system) that the letter order of LFB1-ZWELS is not the vendor's
order of preference: the payment run (F110) tries the methods in the order
entered in the run's parameters per company code, restricted to those
ZWELS allows. The proposal took the first method of the supplier's own
list. Decided by default, from that answer and the Decision above (the
methods a supplier may be paid by come from its company data; the
configuration holds what the company pays by):

- **The order is the company's, in the configuration.** A company's
  `payment_methods[]` each take an optional `order`, a number from 1, each
  once in the company; a run tries the methods with an order first, lowest
  first, then the others as the document lists them
  (`config.Company.MethodsInRunOrder`). The order is a field, not only the
  list's position, because the configuration's diff identifies methods by
  code: a reordered list alone would be a change request without a change.
  `order` is a posting-related change (tier 3), as a ranking is: it decides
  which bank account pays. A run carries no order of its own: SAP enters
  it per run, but nothing in the designs asks for a per-run choice, and a
  run's parameters would let one person reorder what the approved
  configuration set; a per-run order is new scope if the pilot wants it.
- **Stored.** `payments.payment_method.run_order` (000251, checked from 1,
  validated by 000252), the method's place in that order, written by
  `payments.PaymentMethods` on every activation. It is nullable for good:
  a replica of the release before writes none, and a method without one is
  tried after those with one, by code, so the order is total at every read.
- **The proposal.** A run without a house bank pays each supplier, and
  each down payment request, by the first method of the company's order
  that the supplier's `payment_methods` hold and that a house bank ranked
  for the method and the run currency can pay; the supplier's list only
  restricts, and its own order decides nothing (it is still stored and
  returned as given). A supplier none of whose methods a ranked bank can
  pay is excluded with `no_house_bank`, as before. The liquidity forecast
  expects an item on the bank the same rule chooses. A run proposed for
  one house bank is unchanged.
- **Existing tenants: the default order.** 000251 gives each method its
  position among its company's `payment_methods` in the active
  configuration, which is what every later activation writes for a
  document without an `order`: the default order is the order the active
  configuration lists the methods in, and it does not move at the next
  activation. A supplier with one method, or whose list was in that order,
  is paid as before; a supplier whose list ranked a later method of the
  company first is now paid by the company's earlier one. A replica of
  the previous version keeps choosing by the supplier's list until the
  upgrade completes (06 §6.4), as with the rollouts above, and an
  activation whose document gives a method an `order` is refused while
  such a replica runs (409 `config.replica_lacks_key`, the key
  `companies[].payment_methods[].order`; As built, activation refused
  while a replica lacks a key). A document that only lists its methods in
  another order sets no new key, so such a replica may activate it during
  the upgrade: it writes no `run_order`, the methods keep the places they
  had, and the run order follows the document again only at the next
  activation by an upgraded replica. So methods are not reordered during
  an upgrade, or the configuration is activated again after it
  (docs/migration.md, check list).
- **The draft from SAP.** The run's parameters are not in the T042 tables
  the extract carries (the specialist names the F110 parameters, no table;
  where SAP stores them is not stated here and is on the pilot check list),
  so the draft lists the methods in T042E's order as extracted and, for a
  company with more than one, notes that the reviewer sets the order the
  company's runs name. ZWELS still loads in its order as the supplier's
  allowed methods.
- **No contract change beyond descriptions.** A payment already names its
  method and bank, an exclusion its reason, and the configuration document
  is returned as stored; the descriptions of the supplier's
  `payment_methods`, the run and the forecast's bank now state the rule.
- **Walkthrough.** Step 2 gives the pilot company a wire method W with
  order 1, before SAP's bank transfer T; step 7 gives Laurentian Paper the
  methods T and W, in that order, proposes a run without a house bank and
  shows it paid by W through RBC while the suppliers allowing T alone are
  paid by T through MAIN; the run is rejected and the run from RBC follows.
- **Still open** (tracker leaves, split from this one): an item's fixed
  payment method (BSEG/ACDOCA-ZLSCH) honoured over both, and T042I's
  account (HKTID) choosing the house bank's account. The first is built
  on the platform's side in "As built: an item's fixed payment method",
  below; taking ZLSCH over from SAP stays open.


## As built: an item's fixed payment method (A4(c), second part, platform side; product owner 2026-11-10)

The SAP specialist answered (po:2026-11-09, A4(c), [M], to verify on the
pilot system) that an item can fix its payment method (BSEG/ACDOCA-ZLSCH),
which wins over the run's order. The product owner decided the money rule
on 2026-11-10 (po:2026-11-10):

- **One payment per supplier per method.** A supplier's items that fix a
  payment method form their own payment by that method; its items that fix
  none go by the company's run order, as "the run order of payment
  methods" built.
- **A credit memo nets only within the group it falls in**: its own fixed
  method's group, or the group of the items that fix none. Nothing nets
  across methods.
- **A group left in debit is excluded**, as a supplier in debit is today
  (`debit_balance`); the supplier's other group is paid.

The leaf is split once more (split:T45b): this section is the platform's
side, an invoice fixing a method and runs honouring it; the SAP extract and
load filling it from ACDOCA/BSEG ZLSCH stays a leaf of its own. Decided by
default, from the Decision above, the run order's As built and existing
behaviour:

- **Where it is fixed.** A supplier invoice, credit memo or subsequent
  debit or credit takes an optional `payment_method`
  (`payables.supplier_invoice.payment_method`, 000253): at entry, and
  changed or cleared by the invoice's PATCH while it is parked, under its
  version and the row's lock as every change of a parked invoice, the
  audit record of the change naming the method before and after. Posting
  keeps it with the open item, as the item's terms are kept
  (`ledger.open_item_payment_method`, insert-only; `ledger.Posting`'s
  `PaymentMethod`), and a posted document is not changed: no document asks
  for changing an open item's method after posting, and the ledger's item
  data is history. SAP allows changing ZLSCH on an open item (FB02); doing
  so here is new scope if the pilot wants it. A captured document fixes
  none (a person may, before posting).
- **What it may be.** An active payment method of the company, checked by
  the payables context through the payments context's port
  (`payments.PaymentMethods.IsPaymentMethod`), as a supplier's methods are
  checked by the partner context: 422 `payables.unknown_payment_method`
  otherwise. The database holds the code's form (one upper-case letter or
  digit, as every method reference); no foreign key crosses contexts. The
  check is made at entry and at each change, not again at posting.
- **Groups.** A proposal splits each supplier's due items by the method
  they fix (`byMethod`): those that fix none stay one candidate, then one
  per fixed method, by code. Each group is judged apart for every existing
  reason (the supplier blocked, no bank account, a bank change, a debit
  balance) and then routed. A supplier none of whose items fixes a method
  is proposed exactly as before: one payment by the run order, the same
  end-to-end numbering.
- **The same method twice.** A group is defined by what its items fix, not
  by the method that ends up paying it, as the product owner's rule names
  the groups (a credit memo's "own fixed method's group, or the unfixed
  group"). So when the run order pays a supplier's items that fix none by
  T and other items of it fix T, the run makes two payments by T, in the
  same bank file, and nets each apart. Each payment says why it is paid by
  its method (`payment_method_source`: `run_order` or `item`). The
  product owner confirmed that the two stay apart (2026-11-10), as that
  a run proposed for one house bank leaves fixed-method items out
  (below).
- **A fixed method that cannot pay.** The group is left out of the run
  with a reason, never paid by another method (`router.fixed`):
  `method_not_active`, the method is no longer an active method of the
  company (it was when the document was entered); `no_house_bank`, no
  active house bank in the run currency is ranked for it. A reason the
  supplier itself has, or a debit balance, comes first.
- **The supplier's methods do not restrict it** (product owner
  2026-11-10, matching SAP as the specialist describes it, A4(c): ZLSCH
  "wins over both" the run's order and ZWELS). An item fixing a method
  that is not among its supplier's `payment_methods` in the company is
  paid by that method, as any other fixed item: the supplier's list
  restricts the run order alone (`router.route`), so the Decision's
  "the methods a supplier may be paid by come from its company data"
  holds for the items that fix none. This replaces the first build, which
  left such a group out with `method_not_allowed`. Decided by default
  with it: the reason `method_not_allowed` is removed from the contract
  and the code, since no run can write it and it was never released (it
  was added in the same batch, so `oasdiff` against `main` shows no
  removal, and the enumeration is open in any case). The check
  constraint of `payments.method_exclusion.reason_code` (000253) still
  admits the value: a migration is not changed for a value nothing
  writes, and no row carries it. A supplier without any method in the
  company is still paid for an item that fixes one; its items that fix
  none stay `no_house_bank`, as before.
- **A run proposed for one house bank** pays by no method (the Decision:
  no method on its payments or its one file), so it cannot honour a fixed
  one: it leaves every group fixing a method out with
  `fixed_method_in_bank_run` and pays the supplier's other items from its
  bank as before. Paying them in the run's one file would be paying them
  another way. A run without a house bank pays them.
- **Down payment requests** are unchanged: each is a payment of its own,
  routed by the run order; a request fixes no method.
- **Stored.** `payments.payment.method_fixed` (000253) marks a payment of
  items fixing its method; one items payment per run and supplier for the
  items that fix none (`payment_items_once_unfixed`, 000255) and one per
  fixed method (`payment_items_once_per_method`, 000254) replace
  `payment_items_once` (dropped by 000257, after both are built). A group
  fixing a method that is left out has its exclusion in
  `payments.method_exclusion`, one row per run, supplier and method, apart
  from `payment_exclusion`, which keeps the supplier's items that fix none
  one row per run and supplier as before (as `request_exclusion` keeps the
  requests): no key of an existing table changes. A payment is dropped by
  a release when its own group has an exclusion, so excluding one group
  never hides the other's payment.
- **The release** drops a payment whose item was reversed or cleared
  since the proposal as before, excluding the group that payment paid;
  the step run twice drops nothing twice (the dropped payment is skipped).
- **The liquidity forecast** routes an item that fixes a method by that
  method alone, on the bank ranked for it, whether or not its supplier's
  methods include it, and nets a supplier's items fixing a method apart
  from its others, per method, as a run does (ADR-0051 §5; one router,
  `router.items`, serves both); an item whose fixed method cannot pay it
  (not active, or no bank ranked) has no bank and no method, as an item
  no ranked bank pays, and is still forecast on its day (the forecast does not judge a run's exclusions other than a blocked
  supplier's).
- **Contract** (additions only): `payment_method` on a supplier invoice
  (read, create, update), on an open item (`listOpenItems`) and on a
  payment exclusion; `payment_method_source` on a payment; the exclusion
  reasons `method_not_active` and `fixed_method_in_bank_run`; 422
  `payables.unknown_payment_method`. No event changes: `payment_excluded` names its payment, as before.
- **During an upgrade.** A replica of the release before neither writes
  nor reads a fixed method: an invoice it posts keeps none on its open
  item, and a run it proposes pays every item by the run order. So fixed
  methods are entered once the upgrade completes (06 §6.4), as proposals
  without a bank waited in T45c. Rows written before mean what they
  meant: no method fixed.
- **Walkthrough.** Step 7: Alex fixes bank transfer T on Laurentian
  Paper's freight invoice; the run without a house bank pays Laurentian
  twice, the freight invoice by T through MAIN (`item`) and its other
  items by W through RBC (`run_order`); the run from RBC leaves the
  freight invoice out with `fixed_method_in_bank_run`, and the invoice is
  reversed.
- **Still open** (a tracker leaf): the SAP extract and load taking ZLSCH
  over onto migrated open items (ACDOCA/BSEG), with the quality gate's
  check of it against the company's methods.


## As built: activation refused while a replica lacks a key (T72m rollout, pilot)

The rollout bullet of the T72m amendment left the mixed cell to the
release runbook. The cell now refuses such an activation itself.

- **How a replica knows the others.** Every `erp-server` replica records
  itself in `config.replica` (000181): an id of its own, its version and
  revision (`buildinfo`), and the keys its binary knows. It does so before
  it serves (a replica that cannot record itself does not start), then
  every 30 seconds (`configuration.Replica`, as `erp_worker`), and deletes
  its row when it has stopped: after the API drained its requests and the
  workers returned, never when the shutdown begins, so a replica is
  counted for as long as it reads the configuration (review:T72m round
  1). No registry existed; a table read in the
  activation's own transaction is the smallest mechanism that needs no new
  infrastructure, and the database is the one place every replica reaches.
- **The workflow worker waits for its activities** (review:T72m, decided
  by default). A Temporal worker stopped without `WorkerStopTimeout`
  cancels its running activities and returns at once (the SDK's
  `baseWorker.Stop` waits for its task processing for that long only:
  go.temporal.io/sdk@v1.49.0 `internal/internal_worker_base.go`), so a
  release activity that had read the configuration could commit after
  the record was deleted. The worker now waits up to
  `ERP_SHUTDOWN_TIMEOUT` (`workflow.Config.StopTimeout`; 20 s, set in
  the chart), the API's drain budget: no new setting, since both start
  when the shutdown cancels the server's context and run side by side,
  and the record is deleted only after `run` has waited for every
  worker. The pod's grace (30 s) leaves time for the deletion; the
  chart's checks (`scripts/helm-checks.sh`) refuse a budget within 5 s of
  the grace. An activity still running at the timeout has its context
  cancelled, so its transaction rolls back unless its commit was already
  sent; the worker returns without it, and the activity is retried
  (activities are idempotent, ADR-0012). The SDK's own steps (its shutdown
  call to the engine, the workflow worker's wait) come before the
  activity worker's wait, so an activity's context stays live for at
  least the budget and the stop as a whole can take longer. A pod killed
  at the end of its grace leaves its record, which then counts until it
  expires: the safe side.
- **The API's requests end before the record goes** (review:T72m,
  decided by default). `http.Server.Shutdown` returns at its deadline
  without waiting for the handlers still running and cancels none of
  them, and the request deadline (`ERP_REQUEST_TIMEOUT`, 30 s) is longer
  than the drain (20 s), so a request that read the configuration could
  commit after the record was deleted; the upload path (five minutes,
  ADR-0020) and the model route (ten, ADR-0036), outside that deadline,
  could run on for longer still. Bounding the request deadline inside the drain
  alone would leave those two, so `erp-server` itself ends what the drain
  left (`cmd/erp-server` `requests`): both listeners' requests take their
  context from one the shutdown cancels once the drain's deadline passed,
  their connections are then closed (a body still arriving stops being
  read), and the server waits for every handler to return before it
  stops the workers and deletes the record. A cancelled request's unit of
  work rolls back unless its commit was already sent, as an activity's
  does; the router answers 502 `platform.cell_unreachable` (or the
  cell's 503 reaches it) and the client sends it again with the same
  Idempotency-Key, and an upload not yet received stays open for the same
  signed URL. A request starting after the cancellation cannot
  begin a transaction. A handler that ignored its context would hold the
  record until the pod is killed at the end of its grace: the safe side.
  The request deadline and the chart are unchanged: a request that ends
  within the drain still finishes, and only one longer is cut short.
- **A dead replica stops counting.** A row counts while its last record is
  under 5 minutes old (`ReplicaExpiry`), measured on the database's clock
  alone (`seen_at = now()` on record, compared with `now()` on
  activation), so no replica's clock can make a live replica look dead or
  keep a dead one counted. A crashed pod blocks an activation it lacks a
  key for at most 5 minutes; the next record removes its row. A replica
  whose records fail for 2.5 minutes stops (a critical worker), so a
  replica never serves uncounted.
- **The keys come from the code.** `config.Keys()` walks the `Config` type
  (the YAML name of each field, `[]` for a list's items, `{}` for a map's
  values: `companies[].release_dual_limit`); no list is kept by hand.
  `Config.Used()` walks a document the same way and names the keys it sets
  to a value other than their zero value: a key at its zero value means
  what its absence means, so a binary without it reads the document the
  same, and only a set key can be lost. Free-form data (a rule case's
  `row`) holds no keys.
- **The refusal.** Every activation (`:activate`, and a tenant's first
  configuration at provisioning) compares the document's keys with those
  of every replica that counts, before anything is written: a key some
  replica lacks refuses it with 409 `config.replica_lacks_key`, whose
  detail names the keys and whose `errors` name each key (`field`) with
  the versions lacking it (`message`, e.g. `v1.4.0 (0123abcd)`). Once every
  such replica is upgraded or gone the same request activates. An
  activation that sets no such key, ordinary ones in a cell of one version
  included, is unaffected. Not retryable by itself: the caller waits for
  the upgrade, so no `Retry-After`.
- **What it does not cover** (decided by default, 06 §6.4): the upgrade
  that brings this release, whose older replicas record nothing, so the
  runbook rule (activate after the upgrade completes) holds for that one
  upgrade; and an older replica started after the activation committed (a
  rollback, or the old ReplicaSet replacing a pod mid-rollout), which
  reads the document tolerantly and logs the ignored key, as before.
  Refusing to start such a replica would block a rollback; that is a
  question of security posture for the product owner, not built here.
- **Walkthrough.** None: the walkthrough runs one server, so no replica
  lacks a key and no step can show the refusal through the public API;
  `configuration`'s tests show it through `:activate` with a recorded
  older replica, and the contract names the code.

## As built: a payment's bank details only with partner.supplier.read (T45e, product owner 2026-11-02)

The batch 44 review left one question of security posture (ADR-0052,
"Expansions and partner names within permissions"): a payment gives its
supplier's bank account under `payments.run.read`, though reading that
account (`getBankAccount`) needs `partner.supplier.read`. The product
owner decided on 2026-11-02:

- **What is gated.** A payment's `bank_account_last4`,
  `bank_account_scheme`, `bank_account_holder_name`, `bank_account_bic`,
  `bank_account_institution_number` and `bank_account_transit_number`, on
  a run's payments (`listPayments`) and on a statement's payment search
  (`listBankStatementPayments`), the two responses that carry a `Payment`.
  They are given only to a caller who may use `partner.supplier.read` as
  `Authorize` decides it (`authz.MayUse`: held, and not barred by a
  segregation-of-duties rule in force), person or agent.
- **Left out, never refused.** Any other caller gets the payments with
  `bank_account_id` alone, the payment's own reference, as an expansion is
  left out (04 §4.4). The handler decides once a page, and only for a
  page with payments (`mayShowAccounts`); without the permission the
  partner context's accounts are not read at all (`paymentRefs`).
- **The contract.** `bank_account_last4`, `bank_account_scheme` and
  `bank_account_holder_name` were required; they are now optional, which a
  consumer that relied on them notices: an accepted break before v1, listed
  under T45e in `api/breaking-accepted.txt` (oasdiff reports it as a
  warning). The other three were optional already. Each description says
  when the field is absent.
- **Unchanged, as decided.** Supplier names on payments and exclusions, the
  problems that name a supplier, and the migration reconciliation report.
  Exclusions carry no bank details; bank files are sent to the bank, not
  read by people, and their resource (`BankFile`) names the house bank
  only.
- **The workspace.** A found payment on a statement line already showed
  "paid to the account ending …" only when the API gave the last four
  characters; no page shows the other fields.
- **Tests.** `TestBankDetailsNeedPartnerRead` lists a run's payments as a
  person and an agent and searches a statement's payments as a person,
  with the permission held, barred by a segregation-of-duties rule and not
  held, on a Canadian account so the bank and branch numbers are checked
  too; `TestPaymentRefs` shows a page whose accounts are not shown reads
  none. The payments package's tenant holds `partner.supplier.read`
  (`perms`), so every other test still checks the details and the failure
  tests (`TestDatabaseFailures`, `TestSearchPaymentsDatabaseFailures`)
  fail the permission's two reads (permissions, then SoD rules) in turn:
  removing the grant from `perms` would silently drop that coverage.
  No walkthrough step: the walkthrough's caller holds every permission, and
  this is a security fix, not a capability.

## As built: the treasury connector on the SFTP transport (T53h, ADR-0051 §14)
The cash context's exports of snapshots to a treasury system travel on
this transport: an integration connector of kind `treasury_export` names
its SFTP server with the settings a `bank_sftp` channel names (host, port,
user, pinned host key, directory; the private key by `secret_ref`), checked
by the same code (`integration.sftpServer`), and resolves into its endpoint
failing closed as a bank channel does (`integration.resolve`, which
`BankChannels.Endpoint` now calls; a disabled bank channel's detail reads
"The bank channel … is disabled", its code unchanged). `sftpclient.Upload`
now tells a caller when the rename finds a file of the name there
(`ErrExists`), leaving it as it is and removing its own hidden copy: a
bank file, named by its id, still counts that a failed attempt, as
before; an export, named by its content's hash, counts it delivered. The
hidden copy is now named `.partial-<random>-<name>`, the upload's own, so
two uploads of one name at once never write into the same copy.
`sftptest` starts outside a test too (`Start`, `Close`, `Stored`), for
the walkthrough's stand-in SFTP server (`sftptest/sftpstub`).
