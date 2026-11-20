-- E-invoice validation and refusals (T65c; ADR-0061 §4, §5, §7; 03-data-model
-- §3.25): a fetch processes each received document once, reading it as PINT
-- AE (no DTD, no entity), and either refuses it, status refused with the
-- problem's code and detail, final, or leaves it received for entering
-- (T65d), with what it read: the document's number, kind, issue date,
-- currency and gross, its specification identifier (IBT-024) and the release
-- of the rules it was validated against. processed_at, the release and the
-- processing fetch are set together, once, and never before the document
-- was received; what was read and the problem never change after. Expand
-- only: nullable columns the previous version neither reads nor writes, the
-- status check widened NOT VALID (000417 validates it and the new checks,
-- 06 §6.4) and the trigger replaced to allow received to refused.
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused')) NOT VALID,
  ADD COLUMN processed_at timestamptz,
  ADD COLUMN processed_fetch_operation_id uuid,  -- platform.operation: the fetch that processed it
  ADD COLUMN validated_against text,
  ADD COLUMN problem_code text,
  ADD COLUMN problem_detail text,
  ADD COLUMN document_number text,
  ADD COLUMN document_kind text,
  ADD COLUMN issue_date date,
  ADD COLUMN currency char(3) REFERENCES ref.currency,
  ADD COLUMN gross_amount bigint,
  ADD COLUMN specification text,
  ADD CONSTRAINT einvoice_read_lengths CHECK (length(validated_against) BETWEEN 1 AND 100 AND length(problem_code) BETWEEN 1 AND 100
    AND length(problem_detail) BETWEEN 1 AND 2000 AND length(document_number) BETWEEN 1 AND 200 AND length(specification) BETWEEN 1 AND 200
    AND document_kind IN ('invoice', 'credit_note')) NOT VALID,
  -- Processed once: when, by which fetch and against which rules, together,
  -- never before receipt; a refused document was processed.
  ADD CONSTRAINT einvoice_processed CHECK ((processed_at IS NULL) = (validated_against IS NULL)
    AND (processed_at IS NULL) = (processed_fetch_operation_id IS NULL) AND (processed_at IS NULL OR processed_at >= received_at) AND (status = 'received' OR processed_at IS NOT NULL)) NOT VALID,
  -- A refused document names its problem, code and detail; a received one none.
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL)
    AND (problem_code IS NOT NULL) = (status = 'refused')) NOT VALID,
  -- What was read is read when processed, the currency with its amount.
  ADD CONSTRAINT einvoice_read CHECK ((currency IS NULL) = (gross_amount IS NULL)
    AND (processed_at IS NOT NULL OR (document_number IS NULL AND document_kind IS NULL AND issue_date IS NULL AND currency IS NULL
      AND specification IS NULL))) NOT VALID;
GRANT UPDATE (status, processed_at, processed_fetch_operation_id, validated_against, problem_code, problem_detail, document_number, document_kind,
  issue_date, currency, gross_amount, specification) ON einvoicing.einvoice TO erp_app;

-- As 000233's, and: a document is recorded unprocessed; its status moves
-- from received to refused only; once processed, what was read, the
-- problem and the processing itself are fixed; a refused document is final.
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

-- A fetch processes its connector's documents not yet processed, oldest
-- first.
CREATE INDEX einvoice_unprocessed ON einvoicing.einvoice (tenant_id, connector_id, received_at, id) WHERE processed_at IS NULL;
