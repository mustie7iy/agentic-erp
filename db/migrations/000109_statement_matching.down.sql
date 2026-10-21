DELETE FROM identity.sod_rule WHERE permission_a = 'payments.statement.match' OR permission_b = 'payments.statement.match';
DELETE FROM identity.role_permission WHERE permission_code = 'payments.statement.match';
DELETE FROM ref.permission WHERE code = 'payments.statement.match';
DROP TABLE payments.statement_match_target;
DROP TABLE payments.statement_match;
-- 000106's table, empty again: the matches above are lost with the step.
CREATE TABLE payments.statement_match (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  statement_id        uuid NOT NULL,
  line_no             integer NOT NULL,
  rule                text NOT NULL CHECK (rule IN ('end_to_end_id', 'batch_id', 'transaction_code', 'amount', 'manual', 'agent')),
  payment_id          uuid,
  bank_file_id        uuid,
  posting_rule        text,
  status              text NOT NULL CHECK (status IN ('proposed', 'pending_approval', 'posted', 'rejected')),
  proposed_by         uuid NOT NULL,
  decided_by          uuid,
  journal_entry_year  smallint,
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
  CONSTRAINT statement_match_sod CHECK (decided_by IS NULL OR decided_by <> proposed_by)
);
CREATE TRIGGER statement_match_touch BEFORE UPDATE ON payments.statement_match FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE UNIQUE INDEX statement_match_live ON payments.statement_match (tenant_id, statement_id, line_no) WHERE status <> 'rejected';
SELECT platform.secure_table('payments.statement_match'::regclass);
UPDATE payments.bank_statement_line SET status = 'unmatched' WHERE status IN ('proposed', 'matched', 'excluded');
ALTER TABLE payments.bank_statement_line DROP CONSTRAINT bank_statement_line_status_check,
  ADD CONSTRAINT bank_statement_line_status_check CHECK (status IN ('unmatched', 'proposed', 'pending_approval', 'posted'));
ALTER TABLE payments.bank_file DROP COLUMN message_id;
