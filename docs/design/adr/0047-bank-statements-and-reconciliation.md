# ADR-0047: Bank statements and reconciliation

**Status:** accepted (T49, product owner 2026-10-20); as built through
T49h (2026-10-21): the Consequences below describe what was built, step
by step, and the operator notes at the end how a bank is run.

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
  missing statement arrives or a person accepts the gap (as built below).
- **Accounts.** A house bank has, besides its clearing account
  (`gl_account`), a main bank account (`main_gl_account`, stored as the
  nullable `payments.house_bank.main_gl_account_id`, 000119) and an
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
  `bank_statements.posting_rules`, debits and credits alike (interest
  received); incoming payments post to the incoming clearing account until
  accounts receivable (Phase 3) clears them: a person matches such a credit
  to it (T49h), and matching credits to customers' open items waits for
  accounts receivable. The
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
  same approval as a person's: a person accepts one as the person's own
  match, which the person posts (T49g).
- **Statuses and events.** A statement is `imported`, `gap`, `reconciled`;
  a line `unmatched`, `proposed`, `matched`, `excluded`,
  `pending_approval`, `posted`; the
  reconciliation report lists lines and balances per bank. Imports, gaps,
  matches, postings and reconciliations publish events through the outbox
  (ADR-0007) and write audit records (ADR-0011).
- **Reconciled.** A statement is reconciled when every line of it and of
  the bank's statements before it is posted or excluded and its closing
  balance agrees with the ledger's balance of the main bank account on its
  closing date, once the lines excluded and those posted on a later date
  are taken out (T49h). It is set once and never undone by the server; its
  lines are then decided for good.

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
  `GET /v1/accounts/{id}`) when the configuration is activated; a bank
  statement line's clearing reads it there (T49f). **As built (review of
  T57a):** no journal posting's checks read it, so the activation needs no
  lock against a posting in flight (below); a clearing that reads it off
  while the activation runs is refused, as if it had run first.
- The ledger's check that an account turned open-item managed has no
  postings runs when the change is submitted, as the GR/IR account's
  does, and again when it is activated (coordinator 2026-10-20, the
  pattern of T46e's withholding codes): an entry posted to the account
  while the change awaited approval refuses the activation with 409
  `ledger.open_items_account_posted`, and the activation rolls back. An
  entry committed while the activation itself runs is indistinguishable
  from one posted just after it, and is an open item like it.
- Import (T49c, 000120): `POST /v1/bank-statements` takes the format
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
  `payments.statement.read`, to people and integrations only (`x-agent:
  false` since T49g: the file holds the accounts whole). Permissions: `payments.statement.import`
  (people and integrations) and `payments.statement.read` (agents too). An
  agent's import is refused with 403 `identity.agent_not_allowed` by the
  handler: `x-agent: false` would refuse integrations' service principals
  too.
- SFTP fetch (T49d, 000121, 000122): a `bank_sftp` connector names the
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
- Matching (T49e, 000123): the rules run when a statement is imported, in
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
  before 000123 have no message id and are not found by `batch_id`
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
- Posting (T49f, 000126): a `matched` line posts to the house bank's main
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
  rule). The posting goes to every ledger the company keeps (ADR-0045,
  as a settlement does): it clears the payments' copies in the parallel
  ledgers with its own copies, and a line is postable only on a date open
  in each of them (`Postable`). The ledger does not check the caller's
  companies: payments reads the statement within them, or the approval
  authorises the posting, as for approved manual entries. A person posts a line with
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
  incoming clearing account came with T49h (below).
- Proposals (T49g, 000127): the reconciliation agent (`internal/reconagent`,
  agent type `reconciliation`, `erp agent reconcile -statement ID`) takes
  the statement's lines that are `unmatched` or that a rule left open,
  except one whose proposal a person rejected, and runs one session per
  line on read-only tools (`getBankStatement`, `listBankStatementMatches`,
  `getBankStatementRules`, `listPaymentRuns`, `listPayments`,
  `getBusinessPartner`, `listBankAccounts`) and the one that proposes,
  `POST /v1/bank-statements/{id}/lines/{line_no}:propose`
  (`payments.statement.propose`, `x-autonomy: propose`): the payments the
  line clears, a posting rule, or its exclusion, with a reason, in the
  caller's running session (refused as a recommendation's session is,
  through the agents context's `Sessions`, wired in `internal/app`). The
  proposal is a match of kind `agent`, `open`, naming its session and
  `proposal`; it rejects a rule's open match, makes the line `proposed`,
  reserves no payment, never posts and is never taken up by the rules.
  Its targets are checked as a person's match's are. A session proposes
  once per line (`statement_match_session`): the same proposal again
  answers 200 with the first, whatever the If-Match, so a retry whose
  answer was lost finds it; another is 409 `payments.statement_line_proposed`,
  as is a proposal for a line with another session's waiting. A person
  decides it with `payments.statement.match` (`x-agent: false`):
  `:acceptProposal` matches or excludes the line by it as the person's own
  decision (kind `manual`, its targets checked again), so the person posts
  it with `:post` and, as whoever matched it by hand, cannot approve the
  posting; `:rejectProposal` with a reason makes the line `unmatched`
  again; a person's own `:match` or `:exclude` overrides it. The proposal
  is then `rejected` with its `outcome` (`accepted`, `rejected`,
  `overridden`). Proposals publish `BankStatementLineProposed`, decisions
  `BankStatementLineProposalDecided`; both are audited. Agents read
  statements with the accounts masked to their last four characters
  (`****3000`, `counterparty_account_last4`), as partner bank accounts are
  shown (a request that names no principal is shown masked too), never
  download the statement's file, and the gateway redacts IBANs besides. The agent reads the
  company's rules through `GET /v1/bank-statements/{id}/rules`, and a
  statement import finds its house bank through `GET /v1/house-banks`
  (`payments.statement.read`, the account by its last four), which no
  operation listed before. Proposals are a domain operation rather than
  a free-text recommendation (`createRecommendation`): a person accepts
  one as it stands, which needs its targets, as a change request or a
  draft invoice is proposed. The evaluation suite is
  `evals/reconciliation` (ADR-0039): charges under the bank's own code, a
  debit and its reversal excluded, instructions in the bank's text
  ignored, an incoming payment nothing explains left to a person. It has
  no case of a proposal of payments: a paid run needs the proposal and
  release workers (the release a Temporal workflow) and a second person
  to approve it, which the replay cell does not run; the API's own tests
  cover such proposals. T49h recorded the case as part of the live
  certification run of the reconciliation agent, on a cell that runs its
  workers (tracker, under T49), rather than give the replay cell workers.
  Conservative choices for the product owner: the agent does not propose
  again for a line whose proposal a person rejected (the agent's rule,
  which the server does not enforce: another agent's session may); accepting needs no
  reason (the proposal's is the match's, a comment is added); a person
  proposes nothing (the session must be the caller's); agents see
  statements' accounts masked; the agent proposes at most once per line
  and session and may propose nothing.
- Incoming payments (T49h, 000129-000131): a person matches a credit that
  reverses nothing to the house bank's incoming payments clearing account
  (`:match` with `incoming: true`; 422 `payments.statement_match_target`
  for a debit, a reversal, or a bank without `incoming_gl_account`); the
  match's one target says so (`statement_match_target.incoming`, one of a
  payment, a posting rule or it: `statement_match_target_one`). Its
  posting debits the main bank account and credits the incoming account,
  where it stays an open item until accounts receivable clears it (409
  `payments.statement_incoming_gone` when the bank dropped the account
  since the match). Accounts receivable clears it since T75c: an AR
  accountant applies it to a customer's open items in full
  (`POST /v1/incoming-payments`, ADR-0063 §6); until then it is listed by
  `GET /v1/unapplied-receipts`. Posted, approved and reconciled as any match. The
  rules still read debits only, and the agent proposes no incoming match:
  credits that a posting rule takes (interest) are matched by the
  `transaction_code` rule and posted as charges are; matching a credit to
  a customer's open items, by rule or agent, waits for accounts
  receivable (Phase 3).
- Reconciliation (T49h, 000129-000131): on a statement's closing date the
  bank's closing balance and the ledger's balance of the main bank account
  (the leading ledger, in the house bank's currency: its local amounts
  for a bank in the legal entity's currency, the transaction amounts of
  the entries in the bank's currency for a foreign-currency bank, T53a and
  ADR-0038's amendment; `ledger.Accounts.Balances`) differ by the lines of the
  bank's statements up to that date the ledger does not hold yet: those
  `excluded`, those `in_transit` (unmatched, proposed, matched or waiting
  for approval) and those `posted_later` (on a date a person gave after the
  closing date, a locked period's lines; a date before the booking date is
  refused, 422, so no line reaches the ledger before an earlier statement
  closes). What they leave is
  `unexplained`: an opening balance the ledger never received, or an entry
  on the bank account no statement line made. A statement `imported` (never
  one in `gap`) becomes `reconciled` once nothing is in transit or
  unexplained on its closing date: checked under the bank's statement lock
  whenever a line is excluded or posted (by a person, the rules' automatic
  posting or an approval) and on every run of the rules
  (`POST /v1/bank-statements/{id}:match`), which takes up what changed in
  the ledger since (an opening entry posted later), for all the bank's
  statements whose lines are decided. It records `reconciled_at` and
  `reconciled_by` (`bank_statement_reconciled`; 000129 adds the checks
  `NOT VALID`, 000130 validates them, 000131 builds `statement_match_line`
  concurrently), is audited
  (`payments.bank_statement.reconcile`) and publishes
  `BankStatementReconciled` once: the update takes only a statement still
  `imported`. It never reopens by itself (conservative, no rule asked
  otherwise): its lines' decisions are refused (409
  `payments.statement_reconciled`), a statement imported before it later
  leaves it reconciled, and an entry reversed in the ledger afterwards
  shows in the report, not in the status. The ledger is compared at the
  end of the closing date, so of several statements closing on one day
  only the day's last can reconcile (accepted until a pilot bank sends
  intraday statements).
  `GET /v1/house-banks/{id}/reconciliation` (`payments.statement.read`,
  agents too: it names no account) reports, per statement closing in the
  period (`closing_date_from`, `closing_date_to`), its balances, the
  figures on its closing date, its lines' counts by status and its lines
  (or those of `line_status`) with their live match (kind, rule, status,
  incoming, posting date, entry, approval request) and the outcome of the
  latest agent proposal a person decided; `summary` is the period's (its
  first opening balance, the figures on its last closing date, its lines by
  status; another `line_status` than a line's is 422). Paged by statement,
  by closing date, as the statement list is. Each check reads the bank's
  statements and lines from its first: fine for the pilot, bounded later
  (from the earliest statement not reconciled; tracked).
  A bank without a main bank account has no ledger balance and nothing
  unexplained. Conservative choices for the product owner: excluded lines
  and lines posted on a later date are reconciling items, so a statement
  with an excluded line reconciles (the exclusion is a person's decision
  that the booking clears nothing in the books); an agent's run of the
  rules may reconcile a statement, as it is a fact of the books rather than
  a decision; a gap is resolved by importing the missing statement, or
  by a person accepting it (below).
- Accepting a gap (2026-11-01, 000177-000178): `POST
  /v1/bank-statements/{id}:acceptGap` with a reason (`ReasonInput`),
  If-Match and Idempotency-Key, permission
  `payments.statement.gap.accept` (not an approval permission), people
  only (`x-agent: false`: agents and integrations get 403, as for the
  other people-only decisions on statements). Under the bank's statement
  lock and the statement's row lock, an update guarded by `status = 'gap'`
  makes the statement `imported` and records `gap_accepted_at`,
  `gap_accepted_by` and `gap_reason` (shown on the statement); a statement
  not in gap (imported, reconciled, or a gap an import closed meanwhile) is
  409 `payments.statement_not_in_gap`. Audited
  (`payments.bank_statement.accept_gap`, with the reason and both
  balances) and published as `BankStatementGapAccepted` (opening balance,
  the previous statement's closing balance, reason). Decided by default:
  one person decides, no second approver: the ADR says "a person resolves
  the gap", and the acceptance posts nothing itself (each line still goes
  through its approval tier, and what the missing statement held shows as
  `unexplained` in the reconciliation until a person posts it, so nothing
  reconciles on a wrong ledger). The acceptance does not post either: as
  when an import closes a gap, the lines post by hand or on the next run
  of the rules (`:match`), which posts what the rules matched while the gap
  stood. When the missing statement arrives later it becomes the previous
  statement and is compared again: balances that meet leave the statement
  `imported` with its acceptance; balances that do not are a new gap
  (`BankStatementGapFound`), and the acceptance is dropped, as it covered
  another previous statement (kept in the audit trail). A new permission
  reaches existing tenants' administrators only when granted, as every
  permission added since provisioning.
- Searching what a line may clear (T73g2, 2026-11-01, 000187): `GET
  /v1/bank-statements/{id}/payments` lists the payments `:match` by hand
  accepts as `payment_ids` (posted, from the statement's house bank, in
  its currency, held by no line's `matched` match), so a person can choose
  one the rules did not find; it reuses the matching's definition rather
  than adding a target. Decided by default: the statement scopes it
  (bank, currency and company access come from the statement; a house
  bank's search would need them as parameters); the payments' clearing
  lines are the open items a line clears, so "a payment or open item"
  is a payment here, and customers' open items wait for accounts
  receivable as above; it filters by the amount paid (bounds with exactly
  the currency's digits) and the run's date, latest first by when the
  payment was made (not by end-to-end id, whose run document number is
  neither padded nor unique across fiscal years); it requires
  `payments.statement.match`, the permission of the decision it serves,
  and is `x-agent: false`, so the agent's reach is unchanged (it proposes
  from the rules' candidates). The page is chosen on the ids before the
  rows are read whole, over two branches (a payment naming its bank, read
  by `payment_posted_by_bank`; one leaving it to its run): 40 ms for a
  first page and 10 ms for a search finding nothing at 20,000 posted
  payments (`TestSearchPaymentsBudget`, 01's list budget 150 ms).
- Steps: a ADR, configuration and the house bank's columns · b parsers ·
  c tables, upload, idempotency and continuity · d SFTP fetch · e rule
  matching · f posting, clearing, approval and period locks · g agent
  proposals · h report, reconciled status, incoming payments and
  documentation.

## Operator notes

- **Set a bank up for fetching statements.** Create a `bank_sftp`
  connector (`POST /v1/connectors`, ADR-0038) with the bank's `host`,
  `port`, `user`, pinned `host_key`, the outbound `directory`, and both
  `statements_directory` (where the bank leaves statements) and
  `statements_archive` (where they are moved after import), absolute and
  different; the private key goes in `secret_ref`, never in settings.
  Name the connector as the house bank's `connector`, and give the house
  bank its `main_gl_account` (and `incoming_gl_account` for incoming
  payments), with its clearing accounts open-item managed
  (`chart_of_accounts[].open_items`, on an account without postings: an
  account that already has postings is replaced by a new one, its balance
  moved with a manual entry). Activate the configuration. The dispatcher
  starts `FetchBankStatements` per active bank once per
  `ERP_STATEMENT_FETCH_INTERVAL` (default 1h; 0 stops fetching). Before the
  first statement, post the account's opening balance to the main bank
  account on the day before the first statement opens, or the report shows
  it as unexplained and nothing reconciles.
- **Rules and approval tiers.** Set `companies[].bank_statements`:
  `posting_rules` by the bank's transaction codes (charges, interest) to
  an expense or income account, `matching_rules` in priority order, and
  `approval_tiers` by amount in the company's currency. Without tiers every
  posting waits for the `controller` role (fail closed); a tier without
  steps posts at once, so give one only to amounts the company accepts
  unreviewed (charges, small differences), e.g. `[{up_to: "50.00"},
  {steps: [{role: controller, approvals: 1}]}]`. Whoever matched a line by
  hand never approves its posting. Agents propose, never post.
- **A gap.** A statement whose opening balance is not the previous
  closing balance is `gap` (BankStatementGapFound), and the bank's lines
  are not posted, automatically or by hand (409 `payments.statement_gap`),
  until the missing statement is imported: ask the bank to deliver it again
  (into `statements_directory`, or upload it with
  `POST /v1/bank-statements`); it is placed between the two and the gap
  closes (BankStatementGapClosed), and the next run of the rules
  (`:match`) posts what waited. When the bank cannot deliver it, a person
  holding `payments.statement.gap.accept` accepts the gap with the reason
  (`POST /v1/bank-statements/{id}:acceptGap`, BankStatementGapAccepted),
  runs the rules (`:match`) so what waited posts, and posts what the
  missing statement held (it shows as `unexplained` in the reconciliation
  report) with a manual entry. A statement the bank corrected under the
  same id and sequence is refused (409 `payments.statement_conflict`) and
  left in the directory for a person to take up with the bank.
- **What needs a person.** `erp_bank_statement_files_refused` (a file
  the import refused: `BankStatementFileRefused` names why) and
  `erp_bank_statement_files_unarchived` (imported over an hour ago and not
  moved: the archive directory is not writable). Daily, the reconciliation
  report with `line_status=unmatched` (and `proposed`) lists the lines to
  decide, `pending_approval` the postings waiting for approvers, and a
  statement whose `unexplained` is not zero an entry missing from, or
  wrong in, the main bank account.
