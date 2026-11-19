-- A confirmed dunning run cancelled (T76i; ADR-0065 As built; 03-data-model
-- §3.27, §3.26): the run moves from confirmed to cancelled once, with the
-- reason given; the levels its confirmation recorded stay in dunned_item
-- and no longer count, since only confirmed runs' records are read; and
-- each debit memo it posted its notices' charges and interest with
-- (T76h) is reversed through receivables, the charge naming the
-- reversal's entry and date. Receivables reverses a debit memo entered
-- from a source document (a dunning notice) for the context that entered
-- it: until now only a memo without a source was reversed.
--
-- Expand (06 §6.4): a status more, nullable columns, the update grants
-- cancelling needs, the triggers admitting it, and checks widened or
-- added NOT VALID, reading no row under the lock (every row satisfies
-- them: none is cancelled or reversed so), which 000402 validates. The
-- previous version confirms and posts as before, and never writes the
-- new status or columns.
ALTER TABLE collections.dunning_run
  ADD COLUMN cancellation_reason text,
  DROP CONSTRAINT dunning_run_status_check,
  ADD CONSTRAINT dunning_run_status_check CHECK (status IN ('proposed', 'confirmed', 'cancelled')) NOT VALID,
  -- A cancelled run names its reason, of 1 to 200 characters that are not
  -- white space alone; no other run has one.
  ADD CONSTRAINT dunning_run_cancelled CHECK (
    (status = 'cancelled') = (cancellation_reason IS NOT NULL)
    AND (cancellation_reason IS NULL OR (length(cancellation_reason) BETWEEN 1 AND 200 AND btrim(cancellation_reason) <> ''))) NOT VALID;

-- Confirming and cancelling are the only changes: a proposed run moves to
-- confirmed, a confirmed one to cancelled, each once (the grants name the
-- columns they set), and a cancelled one never changes.
CREATE OR REPLACE FUNCTION collections.check_dunning_run_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT ((OLD.status = 'proposed' AND NEW.status = 'confirmed') OR (OLD.status = 'confirmed' AND NEW.status = 'cancelled')) THEN
    RAISE EXCEPTION 'dunning run %: only a proposed run changes, and only by being confirmed, or a confirmed one by being cancelled', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dunning_run_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
GRANT UPDATE (cancellation_reason) ON collections.dunning_run TO erp_app;

-- A charge names the reversal of its memo once its run is cancelled: the
-- reversal's entry (ledger context) by year and id and the date it
-- posted on, not before the memo's, all together or none.
ALTER TABLE collections.dunning_charge
  ADD COLUMN reversal_entry_year smallint,
  ADD COLUMN reversal_entry_id   uuid,
  ADD COLUMN reversal_date       date,
  ADD CONSTRAINT dunning_charge_reversed CHECK (
    (reversal_entry_id IS NULL) = (reversal_entry_year IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_date IS NULL)
    AND (reversal_date IS NULL OR reversal_date >= posting_date)) NOT VALID;
-- What was posted is fixed; the reversal is set once and then fixed too.
-- Nothing deletes a charge.
CREATE FUNCTION collections.check_dunning_charge_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.reversal_entry_id IS NULL AND NEW.reversal_entry_id IS NOT NULL
      AND (NEW.dunning_notice_id, NEW.dunning_run_id, NEW.customer_invoice_id, NEW.customer_invoice_number, NEW.journal_entry_year,
           NEW.journal_entry_id, NEW.posting_date)
        IS NOT DISTINCT FROM (OLD.dunning_notice_id, OLD.dunning_run_id, OLD.customer_invoice_id, OLD.customer_invoice_number,
           OLD.journal_entry_year, OLD.journal_entry_id, OLD.posting_date) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'dunning charge of notice %: only its reversal is recorded, once', OLD.dunning_notice_id
    USING ERRCODE = 'check_violation', CONSTRAINT = 'dunning_charge_reversed_once';
END $$;
ALTER FUNCTION collections.check_dunning_charge_change() OWNER TO erp_owner;
DROP TRIGGER dunning_charge_immutable ON collections.dunning_charge;
CREATE TRIGGER dunning_charge_reversed_once BEFORE UPDATE OR DELETE ON collections.dunning_charge
  FOR EACH ROW EXECUTE FUNCTION collections.check_dunning_charge_change();
GRANT UPDATE (reversal_entry_year, reversal_entry_id, reversal_date) ON collections.dunning_charge TO erp_app;

-- A reversed document is a credit or debit memo a person entered, or a
-- debit memo another context entered from its document (a dunning
-- notice's, T76i), reversed on or after the date it posted.
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_reversed,
  ADD CONSTRAINT customer_invoice_reversed CHECK (
    (status = 'reversed') = (reversal_entry_id IS NOT NULL)
    AND (reversal_entry_id IS NULL) = (reversal_entry_year IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_date IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_reason IS NULL)
    AND (reversal_reason IS NULL OR (length(reversal_reason) BETWEEN 1 AND 200 AND btrim(reversal_reason) <> ''))
    AND (status <> 'reversed' OR (kind IN ('credit_memo', 'debit_memo') AND (source_document_id IS NULL OR kind = 'debit_memo')
      AND reversal_date >= posting_date))) NOT VALID;
