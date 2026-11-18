-- The revert is refused while a clearing was reset: its items' history
-- would be lost, and a receipt, item or credit applied again after a reset
-- could not be held to the uniques the revert restores.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM ledger.clearing_reset) OR EXISTS (SELECT FROM receivables.incoming_payment_reset) THEN
    RAISE EXCEPTION 'clearings were reset: reverting would lose their history and the applications made since'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP INDEX receivables.incoming_payment_application_by_credit;
ALTER TABLE receivables.incoming_payment_application
  ADD CONSTRAINT incoming_payment_application_credit_once UNIQUE (tenant_id, credit_entry_id, credit_line_no);
DROP INDEX receivables.incoming_payment_item_by_line;
ALTER TABLE receivables.incoming_payment_item ADD CONSTRAINT incoming_payment_item_once UNIQUE (tenant_id, journal_entry_id, line_no);
DROP INDEX receivables.incoming_payment_by_receipt;
ALTER TABLE receivables.incoming_payment ADD CONSTRAINT incoming_payment_receipt_once UNIQUE (tenant_id, receipt_entry_id);
DROP TABLE receivables.incoming_payment_reset;

REVOKE DELETE ON ledger.clearing_item FROM erp_app;
DROP TRIGGER clearing_item_reset_only ON ledger.clearing_item;
CREATE TRIGGER clearing_item_immutable BEFORE UPDATE OR DELETE ON ledger.clearing_item FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
DROP FUNCTION ledger.clearing_item_reset_only();
DROP TABLE ledger.clearing_reset_item;
DROP TABLE ledger.clearing_reset;
