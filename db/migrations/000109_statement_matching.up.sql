-- Matching statement lines to what they clear (T49e; ADR-0047; 03-data-model
-- §3.10). A match is made by a matching rule of the company's
-- configuration, by a person, or (T49g) proposed by an agent; it names its
-- targets: the payments it clears (with the bank file of a batch booking)
-- or the posting rule of a charge or interest. A rule that finds exactly
-- one candidate matches the line; one that finds several leaves the line
-- with an open match listing them for a person. A person may match a line
-- by hand or exclude it with a reason. Posting (T49f) adds to this.

-- A bank file names its pain.001 message id (GrpHdr/MsgId; its payment
-- information id is the message id with "-1"), which a batch booking on
-- the statement carries. Files written before have none and are matched by
-- their payments' end-to-end ids. A tenant has a file per run, house bank
-- and method: few enough that the lookup needs no index.
ALTER TABLE payments.bank_file ADD COLUMN message_id text CHECK (message_id <> '');

-- A line is matched, or excluded from reconciliation by a person.
ALTER TABLE payments.bank_statement_line DROP CONSTRAINT bank_statement_line_status_check,
  ADD CONSTRAINT bank_statement_line_status_check
    CHECK (status IN ('unmatched', 'proposed', 'matched', 'excluded', 'pending_approval', 'posted'));

-- 000106's statement_match was never written (matching starts here), so it
-- is replaced rather than altered column by column.
DROP TABLE payments.statement_match;
CREATE TABLE payments.statement_match (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  statement_id  uuid NOT NULL,
  line_no       integer NOT NULL,
  kind          text NOT NULL CHECK (kind IN ('rule', 'manual', 'agent')),
  rule          text CHECK (rule IN ('end_to_end_id', 'batch_id', 'transaction_code', 'amount')),  -- the matching rule that made it
  reason        text NOT NULL CHECK (reason <> ''),  -- what the rule found, or the person's words
  -- open: candidates for a person to choose from; matched: the targets
  -- decided; excluded: no target, by a person; rejected: replaced by a
  -- person's decision.
  status        text NOT NULL CHECK (status IN ('open', 'matched', 'excluded', 'rejected')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, statement_id, line_no) REFERENCES payments.bank_statement_line (tenant_id, statement_id, line_no),
  -- A rule's match names its rule; nothing else does.
  CONSTRAINT statement_match_rule CHECK ((kind = 'rule') = (rule IS NOT NULL)),
  -- Only a person excludes a line.
  CONSTRAINT statement_match_excluded CHECK (status <> 'excluded' OR kind = 'manual')
);
CREATE TRIGGER statement_match_touch BEFORE UPDATE ON payments.statement_match FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- One live match per line.
CREATE UNIQUE INDEX statement_match_live ON payments.statement_match (tenant_id, statement_id, line_no) WHERE status <> 'rejected';
SELECT platform.secure_table('payments.statement_match'::regclass);

-- What a match clears: per candidate (1 for a match decided), a payment,
-- with the bank file of a batch booking, or a posting rule. Written with
-- its match and never changed.
CREATE TABLE payments.statement_match_target (
  tenant_id     uuid NOT NULL,
  match_id      uuid NOT NULL,
  candidate     smallint NOT NULL CHECK (candidate >= 1),
  target_no     smallint NOT NULL CHECK (target_no >= 1),
  payment_id    uuid,
  bank_file_id  uuid,
  posting_rule  text CHECK (posting_rule <> ''),  -- the transaction code of companies[].bank_statements.posting_rules
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, match_id, candidate, target_no),
  FOREIGN KEY (tenant_id, match_id) REFERENCES payments.statement_match (tenant_id, id),
  FOREIGN KEY (tenant_id, payment_id) REFERENCES payments.payment (tenant_id, id),
  FOREIGN KEY (tenant_id, bank_file_id) REFERENCES payments.bank_file (tenant_id, id),
  -- A payment or a posting rule; a bank file only with its payment.
  CONSTRAINT statement_match_target_one CHECK (num_nonnulls(payment_id, posting_rule) = 1 AND (bank_file_id IS NULL OR payment_id IS NOT NULL))
);
-- The live matches of a payment: a payment matched is no other line's candidate.
CREATE INDEX statement_match_target_payment ON payments.statement_match_target (tenant_id, payment_id) WHERE payment_id IS NOT NULL;
SELECT platform.secure_table('payments.statement_match_target'::regclass);

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payments.statement.match', 'Match bank statement lines, by the rules or by hand, and exclude them', false);
