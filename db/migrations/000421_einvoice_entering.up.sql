-- E-invoices entered as supplier invoices (T65d; ADR-0061 §6, §7;
-- 03-data-model §3.9, §3.25): a fetch enters each received tax invoice it
-- validated through payables' entering, sharing capture's rules. Entered,
-- the document names the supplier invoice and its supplier; held, it
-- names the problem of the entering (any refusal of it, for a person to
-- resolve, T65f). Either is decided once, by a fetch, after processing:
-- entry_at and the fetch together. supplier_invoice_id is the payables
-- context's and supplier_id the partner context's: no foreign key leaves
-- the schema (02 §2.1). A supplier invoice entered from an e-invoice has
-- the source einvoice and keeps the document's hash, as a captured one
-- does. Expand only: nullable columns the previous version neither reads
-- nor writes, the checks widened NOT VALID (000422 validates them, 06
-- §6.4), the trigger replaced to allow received to entered or held, and
-- the worker role's read of the status the held gauge counts (000423 adds
-- the indexes).
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_source_check,
  ADD CONSTRAINT supplier_invoice_source_check CHECK (source IN ('manual', 'capture', 'edi', 'migration', 'einvoice')) NOT VALID,
  DROP CONSTRAINT supplier_invoice_check5,
  -- A captured invoice and one entered from an e-invoice keep their
  -- document's hash; no other does.
  ADD CONSTRAINT supplier_invoice_document_hash CHECK ((source IN ('capture', 'einvoice')) = (capture_sha256 IS NOT NULL)) NOT VALID;

ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused', 'entered', 'held')) NOT VALID,
  ADD COLUMN entry_at timestamptz,
  ADD COLUMN entry_fetch_operation_id uuid,  -- platform.operation: the fetch that entered or held it
  ADD COLUMN supplier_invoice_id uuid,       -- payables.supplier_invoice
  ADD COLUMN supplier_id uuid,               -- partner.business_partner: the entered invoice's supplier
  -- Entered or held once, by a fetch, after processing.
  ADD CONSTRAINT einvoice_entry CHECK ((entry_at IS NOT NULL) = (status IN ('entered', 'held'))
    AND (entry_at IS NULL) = (entry_fetch_operation_id IS NULL) AND (entry_at IS NULL OR entry_at >= processed_at)) NOT VALID,
  -- An entered document names its invoice and the invoice's supplier; no
  -- other does.
  ADD CONSTRAINT einvoice_entered CHECK ((supplier_invoice_id IS NOT NULL) = (status = 'entered')
    AND (supplier_id IS NOT NULL) = (status = 'entered')) NOT VALID,
  -- A refused or held document names its problem, code and detail; no
  -- other does.
  DROP CONSTRAINT einvoice_problem,
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL)
    AND (problem_code IS NOT NULL) = (status IN ('refused', 'held'))) NOT VALID;
GRANT UPDATE (entry_at, entry_fetch_operation_id, supplier_invoice_id, supplier_id) ON einvoicing.einvoice TO erp_app;

-- As 000416's, and: a processed document moves from received to entered
-- or held (an unprocessed one alone to refused, as before); what the
-- entering decided (the status, the problem, the invoice and the fetch)
-- is fixed once decided; refused, entered and held documents are final
-- (T65f lets people retry, link or dismiss a held one).
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
  IF NEW.status <> OLD.status AND NOT (OLD.status = 'received' AND ((NEW.status = 'refused' AND OLD.processed_at IS NULL)
      OR (NEW.status IN ('entered', 'held') AND OLD.processed_at IS NOT NULL))) THEN
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
  IF OLD.processed_at IS NOT NULL AND ((NEW.processed_at, NEW.processed_fetch_operation_id, NEW.validated_against, NEW.document_number,
      NEW.document_kind, NEW.issue_date, NEW.currency, NEW.gross_amount, NEW.specification) IS DISTINCT FROM (OLD.processed_at,
      OLD.processed_fetch_operation_id, OLD.validated_against, OLD.document_number, OLD.document_kind, OLD.issue_date, OLD.currency,
      OLD.gross_amount, OLD.specification)
      OR (OLD.status <> 'received' AND (NEW.status, NEW.problem_code, NEW.problem_detail, NEW.entry_at, NEW.entry_fetch_operation_id,
        NEW.supplier_invoice_id, NEW.supplier_id) IS DISTINCT FROM (OLD.status, OLD.problem_code, OLD.problem_detail, OLD.entry_at,
        OLD.entry_fetch_operation_id, OLD.supplier_invoice_id, OLD.supplier_id))) THEN
    RAISE EXCEPTION 'e-invoice % keeps what its processing read and decided', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_fixed';
  END IF;
  RETURN NEW;
END $$;

-- erp_einvoices_held (ADR-0061 §4): the worker role counts, across
-- tenants, the documents held for a person, reading their status alone.
GRANT SELECT (status) ON einvoicing.einvoice TO erp_worker;
