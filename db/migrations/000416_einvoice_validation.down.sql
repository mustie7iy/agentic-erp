-- Processed e-invoices the previous version cannot hold are refused rather
-- than dropped (06 §6.4: a down migration loses no data). The guard reads
-- every tenant's rows, under a role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM einvoicing.einvoice WHERE processed_at IS NOT NULL) THEN
    RAISE EXCEPTION 'processed e-invoices exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP INDEX einvoicing.einvoice_unprocessed;
CREATE OR REPLACE FUNCTION einvoicing.check_einvoice() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'received' THEN
      RAISE EXCEPTION 'an e-invoice is recorded received, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status THEN
    RAISE EXCEPTION 'e-invoice % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_transition';
  END IF;
  IF (NEW.tenant_id, NEW.id, NEW.connector_id, NEW.message_id, NEW.receiver, NEW.legal_entity_id, NEW.sha256, NEW.size, NEW.content_ref,
      NEW.fetch_operation_id, NEW.received_at, NEW.created_at, NEW.created_by) IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.connector_id,
      OLD.message_id, OLD.receiver, OLD.legal_entity_id, OLD.sha256, OLD.size, OLD.content_ref, OLD.fetch_operation_id, OLD.received_at,
      OLD.created_at, OLD.created_by)
      OR (OLD.acknowledged_at IS NOT NULL AND NEW.acknowledged_at IS DISTINCT FROM OLD.acknowledged_at)
      OR (OLD.changed_hash IS NOT NULL AND NEW.changed_hash IS NULL) THEN
    RAISE EXCEPTION 'e-invoice % keeps what was recorded, its acknowledgement and its change of bytes', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_fixed';
  END IF;
  RETURN NEW;
END $$;
REVOKE UPDATE (status, processed_at, processed_fetch_operation_id, validated_against, problem_code, problem_detail, document_number, document_kind,
  issue_date, currency, gross_amount, specification) ON einvoicing.einvoice FROM erp_app;
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_read, DROP CONSTRAINT einvoice_problem, DROP CONSTRAINT einvoice_processed, DROP CONSTRAINT einvoice_read_lengths,
  DROP COLUMN specification, DROP COLUMN gross_amount, DROP COLUMN currency, DROP COLUMN issue_date, DROP COLUMN document_kind,
  DROP COLUMN document_number, DROP COLUMN problem_detail, DROP COLUMN problem_code, DROP COLUMN validated_against,
  DROP COLUMN processed_fetch_operation_id, DROP COLUMN processed_at,
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received'));
