-- Bank statements, their lines and the lines' matches (T49c; ADR-0047;
-- 03-data-model §3.10). A statement is imported from a bank's file
-- (camt.053 or MT940) for one house bank: once per file, by the content's
-- SHA-256 and the statement's place in the file, and once per statement
-- key (the bank's statement id, account and sequence), so the same file
-- again is the same import and another file with the key of an imported
-- statement is refused. Its opening balance is compared with the closing
-- balance of the house bank's statement before it (previous_statement_id):
-- a difference imports it as 'gap', and automatic posting stops for the
-- bank until it is resolved. Amounts are minor units of the statement's
-- currency (ADR-0005), signed: a credit to the account is positive.
-- Statements and lines are never deleted; a line's status and a
-- statement's are the only changes, made by matching and posting (T49e-f).
CREATE TABLE payments.bank_statement (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  house_bank_id          uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,         -- the house bank's, for company access
  format                 text NOT NULL CHECK (format IN ('camt.053.001.02', 'camt.053.001.08', 'mt940')),
  file_sha256            bytea NOT NULL CHECK (octet_length(file_sha256) = 32),
  file_position          integer NOT NULL CHECK (file_position >= 1),  -- the statement's place in its file
  statement_key          text NOT NULL CHECK (statement_key <> ''),
  message_id             text,                  -- camt GrpHdr/MsgId; null in MT940
  statement_ref          text NOT NULL,         -- camt Stmt/Id; MT940 :20:
  sequence               text NOT NULL,         -- camt ElctrncSeqNb or LglSeqNb; MT940 :28C:
  account                text NOT NULL,         -- the IBAN, or the bank's account number
  currency               char(3) NOT NULL REFERENCES ref.currency,
  opening_balance        bigint NOT NULL,
  opening_date           date NOT NULL,
  closing_balance        bigint NOT NULL,
  closing_date           date NOT NULL,
  line_count             integer NOT NULL CHECK (line_count >= 0),
  previous_statement_id  uuid,                  -- the statement continuity was checked against; null for the bank's first
  status                 text NOT NULL CHECK (status IN ('imported', 'gap', 'reconciled')),
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  version                integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT bank_statement_key_once UNIQUE (tenant_id, house_bank_id, statement_key),
  CONSTRAINT bank_statement_file_once UNIQUE (tenant_id, house_bank_id, file_sha256, file_position),
  FOREIGN KEY (tenant_id, house_bank_id) REFERENCES payments.house_bank (tenant_id, id),
  FOREIGN KEY (tenant_id, previous_statement_id) REFERENCES payments.bank_statement (tenant_id, id),
  CONSTRAINT bank_statement_dates CHECK (closing_date >= opening_date),
  -- A gap is a difference from the statement before.
  CONSTRAINT bank_statement_gap CHECK (status <> 'gap' OR previous_statement_id IS NOT NULL)
);
CREATE TRIGGER bank_statement_touch BEFORE UPDATE ON payments.bank_statement FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The house bank's statements by closing date: the continuity check's
-- previous statement and the list.
CREATE INDEX bank_statement_by_date ON payments.bank_statement (tenant_id, house_bank_id, closing_date);
SELECT platform.secure_table('payments.bank_statement'::regclass);

-- A booking on the account. details are the transactions the bank gives
-- for it, as the parser read them (bankstatement.Detail: references,
-- counterparty, amount in minor units).
CREATE TABLE payments.bank_statement_line (
  tenant_id         uuid NOT NULL,
  statement_id      uuid NOT NULL,
  line_no           integer NOT NULL CHECK (line_no >= 1),
  amount            bigint NOT NULL,
  is_reversal       boolean NOT NULL DEFAULT false,
  booking_date      date NOT NULL,
  value_date        date NOT NULL,
  transaction_code  text NOT NULL,
  bank_reference    text NOT NULL,
  owner_reference   text NOT NULL,
  text              text NOT NULL,
  details           jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(details) = 'array'),
  status            text NOT NULL DEFAULT 'unmatched' CHECK (status IN ('unmatched', 'proposed', 'pending_approval', 'posted')),
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        uuid NOT NULL,
  version           integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, statement_id, line_no),
  FOREIGN KEY (tenant_id, statement_id) REFERENCES payments.bank_statement (tenant_id, id)
);
CREATE TRIGGER bank_statement_line_touch BEFORE UPDATE ON payments.bank_statement_line FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('payments.bank_statement_line'::regclass);

-- What a line clears (T49e-f): a payment, a bank file's payments (a batch
-- booking), or a posting rule (charges, interest); an incoming payment
-- names none and posts to the incoming payments clearing account. One
-- live match per line; whoever proposed it cannot decide it.
CREATE TABLE payments.statement_match (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  statement_id        uuid NOT NULL,
  line_no             integer NOT NULL,
  rule                text NOT NULL CHECK (rule IN ('end_to_end_id', 'batch_id', 'transaction_code', 'amount', 'manual', 'agent')),
  payment_id          uuid,
  bank_file_id        uuid,
  posting_rule        text,                     -- the transaction code of companies[].bank_statements.posting_rules
  status              text NOT NULL CHECK (status IN ('proposed', 'pending_approval', 'posted', 'rejected')),
  proposed_by         uuid NOT NULL,
  decided_by          uuid,
  journal_entry_year  smallint,                 -- ledger context, the posting
  journal_entry_id    uuid,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  version             integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, statement_id, line_no) REFERENCES payments.bank_statement_line (tenant_id, statement_id, line_no),
  FOREIGN KEY (tenant_id, payment_id) REFERENCES payments.payment (tenant_id, id),
  FOREIGN KEY (tenant_id, bank_file_id) REFERENCES payments.bank_file (tenant_id, id),
  CONSTRAINT statement_match_one_target CHECK (num_nonnulls(payment_id, bank_file_id, posting_rule) <= 1),
  CONSTRAINT statement_match_posted CHECK ((status = 'posted') = (journal_entry_id IS NOT NULL)
    AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)),
  -- Segregation of duties (ADR-0047): whoever matched a line cannot approve its posting.
  CONSTRAINT statement_match_sod CHECK (decided_by IS NULL OR decided_by <> proposed_by)
);
CREATE TRIGGER statement_match_touch BEFORE UPDATE ON payments.statement_match FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE UNIQUE INDEX statement_match_live ON payments.statement_match (tenant_id, statement_id, line_no) WHERE status <> 'rejected';
SELECT platform.secure_table('payments.statement_match'::regclass);

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payments.statement.read', 'View bank statements and their lines', false),
  ('payments.statement.import', 'Import bank statements', false);
