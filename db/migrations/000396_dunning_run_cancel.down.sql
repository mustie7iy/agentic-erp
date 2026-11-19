-- The revert is refused while a run was cancelled or a debit memo
-- entered from a source document reversed: the previous version knows
-- neither, and the reversals' entries stay in the ledger.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM collections.dunning_run WHERE status = 'cancelled')
      OR EXISTS (SELECT FROM receivables.customer_invoice WHERE status = 'reversed' AND source_document_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cancelled dunning runs or reversed dunning debit memos exist: 000396 cannot be reverted' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_reversed,
  ADD CONSTRAINT customer_invoice_reversed CHECK (
    (status = 'reversed') = (reversal_entry_id IS NOT NULL)
    AND (reversal_entry_id IS NULL) = (reversal_entry_year IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_date IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_reason IS NULL)
    AND (reversal_reason IS NULL OR (length(reversal_reason) BETWEEN 1 AND 200 AND btrim(reversal_reason) <> ''))
    AND (status <> 'reversed' OR (kind IN ('credit_memo', 'debit_memo') AND source_document_id IS NULL AND reversal_date >= posting_date)));
REVOKE UPDATE (reversal_entry_year, reversal_entry_id, reversal_date) ON collections.dunning_charge FROM erp_app;
DROP TRIGGER dunning_charge_reversed_once ON collections.dunning_charge;
DROP FUNCTION collections.check_dunning_charge_change();
CREATE TRIGGER dunning_charge_immutable BEFORE UPDATE OR DELETE ON collections.dunning_charge
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
ALTER TABLE collections.dunning_charge
  DROP CONSTRAINT dunning_charge_reversed,
  DROP COLUMN reversal_date,
  DROP COLUMN reversal_entry_id,
  DROP COLUMN reversal_entry_year;
REVOKE UPDATE (cancellation_reason) ON collections.dunning_run FROM erp_app;
-- check_dunning_run_change as 000277 created it.
CREATE OR REPLACE FUNCTION collections.check_dunning_run_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'proposed' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'dunning run %: only a proposed run changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dunning_run_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE collections.dunning_run
  DROP CONSTRAINT dunning_run_cancelled,
  DROP CONSTRAINT dunning_run_status_check,
  ADD CONSTRAINT dunning_run_status_check CHECK (status IN ('proposed', 'confirmed')),
  DROP COLUMN cancellation_reason;
