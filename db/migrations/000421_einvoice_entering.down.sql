-- E-invoices entered or held, and supplier invoices entered from them,
-- that the previous version cannot hold are refused rather than dropped
-- (06 §6.4: a down migration loses no data). The guard reads every
-- tenant's rows, under a role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM einvoicing.einvoice WHERE status IN ('entered', 'held'))
      OR EXISTS (SELECT FROM payables.supplier_invoice WHERE source = 'einvoice') THEN
    RAISE EXCEPTION 'entered or held e-invoices exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
REVOKE SELECT (status) ON einvoicing.einvoice FROM erp_worker;
CREATE OR REPLACE FUNCTION einvoicing.check_einvoice() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'received' THEN
      RAISE EXCEPTION 'an e-invoice is recorded received, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_transition';
    END IF;
    IF NEW.processed_at IS NOT NULL THEN
      RAISE EXCEPTION 'an e-invoice is recorded unprocessed' USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status AND NOT (OLD.status = 'received' AND NEW.status = 'refused') THEN
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
  IF OLD.processed_at IS NOT NULL AND (NEW.status, NEW.processed_at, NEW.processed_fetch_operation_id, NEW.validated_against, NEW.problem_code,
      NEW.problem_detail, NEW.document_number, NEW.document_kind, NEW.issue_date, NEW.currency, NEW.gross_amount, NEW.specification)
      IS DISTINCT FROM (OLD.status, OLD.processed_at, OLD.processed_fetch_operation_id, OLD.validated_against, OLD.problem_code, OLD.problem_detail,
      OLD.document_number, OLD.document_kind, OLD.issue_date, OLD.currency, OLD.gross_amount, OLD.specification) THEN
    RAISE EXCEPTION 'e-invoice % keeps what its processing read and decided', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_fixed';
  END IF;
  RETURN NEW;
END $$;
REVOKE UPDATE (entry_at, entry_fetch_operation_id, supplier_invoice_id, supplier_id) ON einvoicing.einvoice FROM erp_app;
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_problem,
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL) AND (problem_code IS NOT NULL) = (status = 'refused')),
  DROP CONSTRAINT einvoice_entered, DROP CONSTRAINT einvoice_entry,
  DROP COLUMN supplier_id, DROP COLUMN supplier_invoice_id, DROP COLUMN entry_fetch_operation_id, DROP COLUMN entry_at,
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused'));
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_document_hash,
  ADD CONSTRAINT supplier_invoice_check5 CHECK ((source = 'capture') = (capture_sha256 IS NOT NULL)),
  DROP CONSTRAINT supplier_invoice_source_check,
  ADD CONSTRAINT supplier_invoice_source_check CHECK (source IN ('manual', 'capture', 'edi', 'migration'));
