# ADR-0047: Bank statements and reconciliation

**Status:** accepted (T49, product owner 2026-10-20)

## Context
A payment run releases payments that credit the house bank's clearing
account (ADR-0038); nothing yet tells the ledger what the bank actually
booked. SAP imports the bank's account statement (FF_5, FEBAN): each line
is matched by interpretation algorithms to what it clears, posted from the
clearing account to the bank's main account through posting rules (T028G)
keyed by the bank's transaction codes, and what cannot be matched is
worked by hand. Our pilots' banks in Canada and the United Arab Emirates
send ISO 20022 camt.053 (versions .001.02 and .001.08) or SWIFT MT940; a
Canadian bank may only offer BAI2.

## Decision
- **Owner.** The payments context owns statements, their lines and their
  matches (`payments.bank_statement`, `bank_statement_line`,
  `statement_match`; `tenant_id` leads every key, RLS on each, a schema
  test for each rule the database enforces). Postings go through the
  ledger's published ports, as payments already do.
- **Intake.** By upload, `POST /v1/bank-statements` naming the format and
  the house bank, and by SFTP fetch from the house bank's channel (the
  `bank_sftp` connector of ADR-0038): a workflow per bank and period
  (T49d) fetches new files, imports each and moves it to an archive directory on the
  server only after the import committed, so a lost acknowledgement fetches
  the file again and the import recognises it.
- **Formats.** camt.053.001.02 and .001.08, and MT940, parsed in pure Go
  into one statement model: amounts in minor units (`money.Minor`,
  ADR-0005), signed (credit positive, debit negative), booking and value
  dates, the bank transaction code (camt `BkTxCd` as `DOMN-FMLY-SUBF` or
  the bank's proprietary code; MT940 the `:61:` type such as `NCHG`), the
  end-to-end id, the payment information and message ids, the bank's and
  the account servicer's references, and the counterparty's name, IBAN and
  BIC (`internal/bankstatement`, T49b). Only booked entries are read,
  and a statement whose entries do not take its opening balance to its
  closing balance, in its one currency, is refused. A transaction's
  amount is read only in the statement's currency (the counter value
  where the bank instructed another). Files are read as UTF-8: MT940
  bytes that are not become U+FFFD and a camt.053 declaring another
  encoding is refused, until a pilot bank needs ISO-8859-1. Every parser has a
  fuzz target in `make fuzz`; no new per-PR CI.
  BAI2 comes later as its own small step, and only if the Canadian pilot
  bank cannot send camt.053.
- **Idempotency.** A file is imported once per tenant, house bank and
  content SHA-256: the same file again returns the first import. A
  statement is also identified by its key — camt: `Stmt/Id`, the account
  and `ElctrncSeqNb` (or `LglSeqNb`); MT940: `:20:`, `:25:` and `:28C:` —
  and a different file with the key of an imported statement is refused
  with 409.
- **Continuity.** A statement's opening balance must equal the previous
  statement's closing balance for the account; otherwise it is imported
  with status `gap` and automatic posting stops for the bank until the
  missing statement arrives or a person resolves the gap.
- **Accounts.** A house bank has, besides its clearing account
  (`gl_account`), a main bank account (`main_gl_account`, stored as the
  nullable `payments.house_bank.main_gl_account_id`, 000105) and an
  incoming payments clearing account (`incoming_gl_account`,
  `incoming_gl_account_id`). A bank without a main account has its
  statements imported but never posted. The accounts are plain asset
  accounts of the chart, take no tax and belong to one house bank.
- **Open items.** A bank's clearing accounts must be open-item managed
  (`chart_of_accounts[].open_items`, SAP SKB1-XOPVW) once the bank names a
  main account: a statement line clears the payment's line on them. An
  account becomes open-item managed only while it has no postings, and
  never stops being one (product owner 2026-10-20): a company whose old
  clearing account has postings names a new one, and transfers the old
  balance to it with a one-off manual entry, which is audited and
  approved as every manual entry is (T35).
- **Posting.** A line posts only when matched. An outgoing payment debits
  the clearing account and credits the main bank account, clearing the
  payment's line; a batch booking clears the payments of the bank file
  whose `PmtInfId` or `MsgId` it carries; charges and interest post by
  their transaction code to the account of the company's
  `bank_statements.posting_rules`; incoming payments post to the incoming
  clearing account until accounts receivable (Phase 3) clears them. The
  posting date is the booking date; in a locked or closed period the
  posting is refused with the period lock's error (T32), and a user may
  give a date in an open period instead, which is audited.
- **Matching.** `bank_statements.matching_rules` in priority order, each
  kind once: `end_to_end_id`, `batch_id`, `transaction_code` (a posting
  rule), and `amount` (within `amount_tolerance` in the company's
  currency, the payment's date within `date_window_days` of the booking
  date, and with `counterparty` the payee's IBAN or name). The first rule
  that finds candidates decides; a line is posted automatically only when
  it finds exactly one.
- **Approval.** A match is routed by the line's amount in the company's
  currency through `bank_statements.approval_tiers`
  (`approvals.BankClearing`); a tier without steps posts without approval.
  There is no default: until a company configures tiers, every match needs
  approval (fail closed, product owner 2026-10-20). Whoever matched a line
  by hand cannot approve its posting (segregation of duties).
- **Agents.** The reconciliation agent proposes matches for unmatched lines
  (`x-autonomy: propose`) and never posts; its proposals go through the
  same approval as a person's.
- **Statuses and events.** A statement is `imported`, `gap`, `reconciled`;
  a line `unmatched`, `proposed`, `matched`, `excluded`,
  `pending_approval`, `posted`; the
  reconciliation report lists lines and balances per bank. Imports, gaps,
  matches and postings publish events through the outbox (ADR-0007) and
  write audit records (ADR-0011).

## Alternatives
- **Post every line at import and correct later:** quicker, but a wrong
  guess reaches the books, and a correction is a second posting to audit.
- **Match on the main bank account directly, without clearing:** one
  posting fewer, but the bank's balance and the payments in transit could
  no longer be told apart, and the run's postings would have to wait for
  the statement.
- **Turn open-item management on for a posted account and treat its old
  lines as cleared:** no new account, but its balance would hold lines no
  statement can clear, and a reconciliation that starts dirty never ends
  clean.
- **A default approval tier:** less setup, but a company that never looked
  at the setting would post bank matches unchecked.

## Consequences
- Configuration: `companies[].house_bank(s)[].main_gl_account` and
  `incoming_gl_account`, `chart_of_accounts[].open_items` (refused by the
  ledger's check for an account with postings, and once on it stays) and
  `companies[].bank_statements` (posting rules, matching rules, approval
  tiers); all posting-relevant changes except a posting rule's
  description (T49a).
- The ledger records the open-item flag on the company's account
  (`ledger.company_account.is_open_item_managed`, shown by
  `GET /v1/accounts/{id}`) when the configuration is activated; posting
  and clearing read it there (T49f).
- The ledger's check that an account turned open-item managed has no
  postings runs when the change is submitted, as the GR/IR account's
  does, and again when it is activated (coordinator 2026-10-20, the
  pattern of T46e's withholding codes): an entry posted to the account
  while the change awaited approval refuses the activation with 409
  `ledger.open_items_account_posted`, and the activation rolls back. An
  entry committed while the activation itself runs is indistinguishable
  from one posted just after it, and is an open item like it.
- Import (T49c, 000106): `POST /v1/bank-statements` takes the format
  (`camt.053` or `mt940`), the house bank and the file (at most 4 MB) as a
  form; nothing of the upload names a path. A statement must be of the
  house bank's account (its IBAN, or account number) and currency (422
  `payments.statement_not_of_bank`); a file that cannot be read is 422
  `payments.statement_unreadable`, its message starting with the line of
  the file where what is wrong starts (`bankstatement.ParseError`).
  Imports of one house bank wait for each other (an advisory lock), so the
  file hash, the key and the continuity are checked against every
  committed import. The statement before is the bank's with the latest
  closing date not after the new one's opening date. A statement imported
  between two others (the missing one, or an older one imported later)
  becomes the next one's previous statement, which is compared again: it
  becomes `imported` when the balances meet (`BankStatementGapClosed`) and
  `gap` when they do not (`BankStatementGapFound`). Automatic posting (T49f) runs only while the
  bank has no statement in `gap`. The file itself is kept as accounting
  evidence (coordinator 2026-10-20): in the object store under the tenant
  and its SHA-256 (`tenants/<id>/bank-statements/<sha256>`, linked from
  `bank_statement.file_ref`), written once (the same bytes again are the
  same object), encrypted by the store as every object is (SSE-KMS where
  configured, ADR-0040), and locked in compliance mode for
  `ERP_OBJECT_RETENTION` (default 10 years) like the audit anchors and
  exports (ADR-0021). Offboarding's erasure deletes the tenant's prefix and
  keeps what is locked until its retention ends, as for the other financial
  records that outlive the tenant: a statement file outliving the tenant's
  erasure until then is accepted as the retention of financial records
  requires (coordinator 2026-10-20); a re-onboarded tenant's statements
  (ADR-0029) name the same keys. The file is written once it is known to be
  imported (every statement of the bank's account and currency, every key
  new) and before the statements are inserted, so a refused file leaves no
  locked object and a statement never names a missing file; without object
  storage, or when the store does not take it, nothing is imported (503).
  `GET /v1/bank-statements/{id}/file` downloads it, byte for byte, with
  `payments.statement.read`. Permissions: `payments.statement.import`
  (people and integrations) and `payments.statement.read` (agents too). An
  agent's import is refused with 403 `identity.agent_not_allowed` by the
  handler: `x-agent: false` would refuse integrations' service principals
  too.
- SFTP fetch (T49d, 000107, 000108): a `bank_sftp` connector names the
  directory the bank leaves statements in and the one they are moved to
  (`statements_directory`, `statements_archive`: both or neither,
  absolute, not the same). The workflow dispatcher starts, once per
  `ERP_STATEMENT_FETCH_INTERVAL` (default 1h, 0 none) and active house bank
  on such a channel, the `FetchBankStatements` workflow under an id naming
  the bank and the period, so replicas start each once; this is the repo's
  workflow pattern rather than a Temporal Schedule, which would need its
  own lifecycle per bank as channels change. The workflow lists the
  directory (hidden files are files still being written), and per file
  reads it (at most 4 MB), imports it through the upload's path as the
  fetcher (a system principal acting for the bank's company; the format
  told by the content: XML is camt.053), and once the import committed reads
  it again and moves it to the archive under its name prefixed with its
  hash. A file that holds other content when read again (the bank rewrote
  it) is not moved: its record becomes `changed_after_import`, with the new
  content's hash as the reason, `BankStatementFileChanged` is published,
  and the next fetch takes the new content as a new file (coordinator
  2026-10-20): imported when its statements are new, refused as
  `payments.statement_conflict` and left for a person when the bank kept
  the statement id and sequence. A rewrite between that read and the move
  is not seen. What became of
  each file is recorded once per house bank and content
  (`payments.statement_fetch`: `imported`, `archived`, `refused` with the
  import's reason), so a file imported and not yet moved when a fetch was
  cut short is moved by the next one, never imported again; a move whose
  acknowledgement was lost finds its target there; the same file
  delivered again is removed once its archived copy is there. A file the
  import refuses (unreadable, too large, a statement of another file) is
  recorded once with `BankStatementFileRefused` and left in the directory
  for a person to take up with the bank; a statement of the account of
  another active house bank on the same channel (a directory several banks
  share) is left for that bank, and one of an account no house bank on the
  channel has is refused as the others are
  (`payments.statement_of_no_bank`; coordinator 2026-10-20), never skipped
  unseen.
  Each activity is retried (5 attempts); a file's failure does not stop the
  others, and the workflow then fails naming the files the next fetch
  takes again. The channel resolves as for a transmission and fails
  closed: an inactive bank, a disabled connector, a key that does not
  resolve or a channel without statement directories fail the fetch
  without retries. The gauges `erp_bank_statement_files_refused` and
  `erp_bank_statement_files_unarchived` (imported over an hour ago and not
  yet moved) show what needs a person.
- Matching (T49e, 000109): the rules run when a statement is imported, in
  the import's transaction, and again on `POST /v1/bank-statements/{id}:match`,
  over the lines still `unmatched` and those a rule left `open`, in line
  order, under the house bank's statement lock. An open line is compared
  with what the rules find now (rule, status and targets): the same leaves
  it, anything else rejects its open match (audited) and records the new
  one, or makes it `unmatched` again (the rejection is audited; no event
  says the line is unmatched, as none says so of a new line); matched and
  excluded lines, and an agent's proposals, are never touched (product
  owner 2026-10-20). A line matched in a pass takes its payments from
  open lines before it, so passes repeat until one changes nothing, and a
  second run then changes nothing. The statement-level run is open to agents
  with the permission: it only applies the rules, and what they cannot
  decide stays open for a person; the line decisions are people's
  (`x-agent: false`, product owner 2026-10-20). The rules on payments
  read debits only (outgoing
  payments; incoming ones wait for accounts receivable) and never
  reversals, and take a payment that is posted and of the statement's
  house bank and currency. `end_to_end_id` takes the
  transactions' end-to-end ids (an MT940 owner's reference holds 16
  characters, too few for ours); `batch_id` the transactions' payment
  information and message ids, against the message id a bank file now
  records (`payments.bank_file.message_id`; its payment information id is
  that with `-1`; files written before have none), a file standing with
  all its payments; `transaction_code` the line's and its transactions'
  codes, compared exactly; `amount` the run date within the window and,
  with `counterparty`, the payee's IBAN or holder's name (case aside), the
  tolerance applying to a statement in the company's currency (another is
  matched exactly; accepted, coordinator 2026-10-20). Bank files written
  before 000109 have no message id and are not found by `batch_id`
  (accepted before the pilot). A rule that finds candidates records a match
  (`payments.statement_match`, kind `rule`, its rule and a reason saying
  what it found; its targets in `statement_match_target`, by candidate):
  `matched` when it found exactly one candidate whose payments pay the
  line's amount and that no other line's match holds, else `open` with
  the candidates for a person, the line `proposed`. An open match reserves
  nothing. A payment a line's match holds is no candidate of the amount
  rule, and the id rules still find it, so a booking the bank repeats goes
  to a person rather than to the rules after; matching again finds the
  same. A person
  (`payments.statement.match`, `x-agent: false`) matches a line by hand to
  payments or a posting rule, or excludes it with a reason
  (`POST .../lines/{line_no}:match`, `:exclude`): the line's live match
  becomes `rejected`, both audited; a line `pending_approval` or `posted`
  keeps its match (409). Each takes the statement's ETag as If-Match and
  answers with its new one, the If-Match of the next decision (product
  owner 2026-10-20). `BankStatementLineMatched` and
  `BankStatementLineExcluded` are published. Lines become `matched`,
  `proposed` or `excluded`; posting a match is T49f.
- Posting (T49f, 000110): a `matched` line posts to the house bank's main
  bank account against what its match clears, through the ledger's
  `Postings.PostBank` (document type SA, source context payments, the
  match as source document): a match of payments debits the bank's
  clearing account with each payment's line on it at its original amounts
  in all three currencies and clears those lines with the posting
  (`ItemsCleared`), as a settlement clears supplier items; the clearing
  account must be open-item managed in the legal entity (422
  `ledger.not_open_item_managed`), the lines open and not reversed, and
  together the line's amount (the payments' amounts are compared first:
  409 `payments.statement_amount_differs`). The main bank line is
  converted at the posting date's rates and what that leaves in local or
  group currency is a realized exchange difference on fx_gain or fx_loss.
  A posting rule's match posts against the rule's account (409
  `payments.statement_posting_rule_gone` once the company dropped the
  rule). The ledger does not check the caller's companies: payments reads
  the statement within them, or the approval authorises the posting, as
  for approved manual entries. A person posts a line with
  `POST /v1/bank-statements/{id}/lines/{line_no}:post`
  (`payments.statement.post`, `x-agent: false`), on its booking date or on
  `posting_date` with a `reason`, both recorded on the match and audited;
  a locked or closed period refuses it (409 `ledger.period_not_open`),
  checked before an approval is asked for. The posting is routed through
  the company's `approval_tiers` by the line's amount, converted exactly
  into the company's currency on the posting date
  (`approvals.BankClearing`, `bank_clearing` requests, approved with
  `payments.statement.clearing.approve`); a tier without steps posts at
  once, and a company without tiers has every posting approved by the
  `controller` role (fail closed, as the period unlock and the manual
  entry do). Whoever matched the line by hand cannot approve its posting
  (403 `workflow.segregation_of_duties`, asked of the owner through
  `approvals.Barrer` before each approval), nor can the requester. A line
  waiting is `pending_approval`; the approval posts it, checked again (a
  refusal refuses the decision and the request stays pending), and a
  rejection or withdrawal returns it to `matched`, keeping the request on
  the match so the rules' runs do not submit it again: a person posts it
  again, on another date, or matches it again. The lines the rules match
  are posted the same way when the rules run, on import and on
  `:match`, as whoever ran them, unless an agent ran them (agents never
  post; the next person's or integration's run does), a person or
  integration without `payments.statement.post` ran them (the statement
  fetcher, a system principal, posts what it imports), or the bank has a
  statement in `gap`; each line posts under a savepoint, so a refusal (a
  locked period, a missing rate) is rolled back with what it recorded,
  audited as refused, and leaves the line `matched` for a person (T49f's
  conservative choices, for the product owner to confirm). While the bank has a
  statement in `gap` a person's posting is refused too (409
  `payments.statement_gap`), and a bank without a main bank account never
  posts (409 `payments.statement_bank_not_posting`). A posted match stays
  `matched` and is not decided again (`statement_match_posted`); one
  journal entry posts one match (`statement_match_journal_entry`); the
  approval's callback acts only while the line waits for that request, so
  a second call posts nothing. `BankStatementLinePosted` is published. The
  statement's `reconciled` status and incoming payments posted to the
  incoming clearing account are T49h and later steps.
- Steps: a ADR, configuration and the house bank's columns · b parsers ·
  c tables, upload, idempotency and continuity · d SFTP fetch · e rule
  matching · f posting, clearing, approval and period locks · g agent
  proposals · h report and documentation.
