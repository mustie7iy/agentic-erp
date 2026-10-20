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
  `bank_sftp` connector of ADR-0038): a Temporal schedule per bank fetches
  new files, imports each and moves it to an archive directory on the
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
  a line `unmatched`, `proposed`, `pending_approval`, `posted`; the
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
  closing date not after the new one's opening date; when a statement
  arrives whose closing balance is the opening balance of the bank's next
  statement in `gap`, that one becomes `imported`
  (`BankStatementGapClosed`). Automatic posting (T49f) runs only while the
  bank has no statement in `gap`. The file itself is not kept: its hash,
  statements and lines are. Permissions: `payments.statement.import`
  (people and integrations) and `payments.statement.read` (agents too); the
  upload is not `x-autonomy: propose`, so an agent below level 2 cannot
  import.
- Steps: a ADR, configuration and the house bank's columns · b parsers ·
  c tables, upload, idempotency and continuity · d SFTP fetch · e rule
  matching · f posting, clearing, approval and period locks · g agent
  proposals · h report and documentation.
