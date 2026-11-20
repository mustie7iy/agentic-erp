-- What the previous version cannot hold is refused rather than dropped (06
-- §6.4: a down migration loses no data): a completed submission's
-- delivery status, tax data document status or request. The guard reads
-- every tenant's rows, under a role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM einvoicing.outgoing_submission WHERE delivery_status IS NOT NULL OR tax_data_status IS NOT NULL OR tax_data_request IS NOT NULL) THEN
    RAISE EXCEPTION 'e-invoice delivery statuses exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
CREATE OR REPLACE FUNCTION einvoicing.check_outgoing_submission() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) NOT IN (('queued', 'sending'), ('queued', 'failed'), ('sending', 'queued'), ('sending', 'sending'), ('sending', 'submitted'),
      ('sending', 'failed'), ('submitted', 'submitted'), ('submitted', 'queued'), ('submitted', 'completed'), ('submitted', 'failed'),
      ('failed', 'queued')) THEN
    RAISE EXCEPTION 'an e-invoice submission does not move from % to %', OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_transition';
  END IF;
  IF (NEW.tenant_id, NEW.id, NEW.outgoing_einvoice_id, NEW.legal_entity_id, NEW.filename, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.outgoing_einvoice_id, OLD.legal_entity_id, OLD.filename, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'e-invoice submission % keeps its e-invoice', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION einvoicing.check_outgoing_submission() OWNER TO erp_owner;
DROP INDEX einvoicing.outgoing_einvoice_by_reference;
DROP INDEX einvoicing.outgoing_submission_followed_due;
ALTER TABLE einvoicing.outgoing_submission
  DROP CONSTRAINT outgoing_submission_tax_data,
  DROP CONSTRAINT outgoing_submission_delivery,
  DROP CONSTRAINT outgoing_submission_request_due,
  DROP CONSTRAINT outgoing_submission_followed,
  DROP COLUMN follow_check_at,
  DROP COLUMN follow_polls,
  DROP COLUMN tax_data_attempts,
  DROP COLUMN tax_data_request,
  DROP COLUMN tax_data_at,
  DROP COLUMN tax_data_status,
  DROP COLUMN delivered_at,
  DROP COLUMN delivery_detail,
  DROP COLUMN delivery_status;
