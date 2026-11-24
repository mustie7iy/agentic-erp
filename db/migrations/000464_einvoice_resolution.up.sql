-- Held e-invoices resolved by people (T65f; ADR-0061 §7, §11, §13;
-- 03-data-model §3.25): a person retries a held document's entering
-- (held to entered, or to held with the problem of this attempt), links it
-- to an invoice they entered by hand (held to entered, the invoice's
-- document the e-invoice's bytes), or dismisses it with a reason (held to
-- dismissed, final). The last resolution is kept with when it was made
-- (resolution, resolved_at; who made it is updated_by and the audit
-- record); the fetch's entry_at and entry_fetch_operation_id stay as the
-- fetch decided them. At most one document names an invoice (the partial
-- unique index einvoice_invoice_once, mapped to 409
-- einvoicing.invoice_already_linked). A supplier invoice entered by hand
-- may now carry a document: the e-invoice a person linked to it, its hash
-- kept as a captured one's is (supplier_invoice_document_hash); a captured
-- or e-invoice's still always does, and an EDI or migrated one never.
-- Expand only: nullable columns the previous version neither reads nor
-- writes, the checks widened NOT VALID (000465 validates them, 06 §6.4),
-- the index, and the trigger replaced to allow a held document's
-- resolution.
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_document_hash,
  ADD CONSTRAINT supplier_invoice_document_hash CHECK ((source NOT IN ('capture', 'einvoice') OR capture_sha256 IS NOT NULL)
    AND (source IN ('capture', 'einvoice', 'manual') OR capture_sha256 IS NULL)) NOT VALID;

ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused', 'entered', 'held', 'dismissed')) NOT VALID,
  ADD COLUMN resolution text,
  ADD COLUMN resolved_at timestamptz,
  ADD COLUMN dismissal_reason text,
  -- A dismissed document was held, so it keeps when and by which fetch.
  DROP CONSTRAINT einvoice_entry,
  ADD CONSTRAINT einvoice_entry CHECK ((entry_at IS NOT NULL) = (status IN ('entered', 'held', 'dismissed'))
    AND (entry_at IS NULL) = (entry_fetch_operation_id IS NULL) AND (entry_at IS NULL OR entry_at >= processed_at)) NOT VALID,
  -- A dismissed document keeps the problem it was held with.
  DROP CONSTRAINT einvoice_problem,
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL)
    AND (problem_code IS NOT NULL) = (status IN ('refused', 'held', 'dismissed'))) NOT VALID,
  -- A resolution is a person's, of a document the fetch held, after it:
  -- retried (now entered or held), linked (entered) or dismissed, with its
  -- reason exactly then.
  ADD CONSTRAINT einvoice_resolved CHECK ((resolution IS NULL) = (resolved_at IS NULL)
    AND (resolved_at IS NULL OR (entry_at IS NOT NULL AND resolved_at >= entry_at))
    AND (resolution IS NULL OR (resolution = 'retried' AND status IN ('entered', 'held')) OR (resolution = 'linked' AND status = 'entered')
      OR (resolution = 'dismissed' AND status = 'dismissed'))
    AND (status <> 'dismissed' OR resolution = 'dismissed')) NOT VALID,
  ADD CONSTRAINT einvoice_dismissal_reason CHECK ((dismissal_reason IS NOT NULL) = (status = 'dismissed')
    AND (dismissal_reason IS NULL OR (length(btrim(dismissal_reason)) > 0 AND length(dismissal_reason) <= 500))) NOT VALID;
GRANT UPDATE (resolution, resolved_at, dismissal_reason) ON einvoicing.einvoice TO erp_app;

-- At most one document names an invoice (§7): the fetch enters each
-- document as an invoice of its own, and a link refuses an invoice
-- already named, so the existing rows hold it.
CREATE UNIQUE INDEX einvoice_invoice_once ON einvoicing.einvoice (tenant_id, supplier_invoice_id) WHERE supplier_invoice_id IS NOT NULL;

-- As 000423's, and: a held document moves, by a person's resolution (a
-- new resolved_at), to entered, to held again (a retry, with the problem
-- of that attempt) or to dismissed; what the fetch decided (when and by
-- which fetch) stays; refused, entered and dismissed are final, and a
-- held document changes only by a resolution.
CREATE OR REPLACE FUNCTION einvoicing.check_einvoice() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  resolving boolean;
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
  resolving := OLD.status = 'held' AND NEW.resolved_at IS DISTINCT FROM OLD.resolved_at;
  IF NEW.status <> OLD.status AND NOT (OLD.status = 'received' AND ((NEW.status = 'refused' AND OLD.processed_at IS NULL)
      OR (NEW.status IN ('entered', 'held') AND OLD.processed_at IS NOT NULL)))
      AND NOT (resolving AND NEW.status IN ('entered', 'dismissed')) THEN
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
      OR (OLD.status = 'received' AND NEW.resolved_at IS NOT NULL)
      OR (OLD.status <> 'received' AND (NEW.entry_at, NEW.entry_fetch_operation_id) IS DISTINCT FROM (OLD.entry_at, OLD.entry_fetch_operation_id))
      OR (OLD.status <> 'received' AND NOT resolving AND (NEW.status, NEW.problem_code, NEW.problem_detail, NEW.supplier_invoice_id, NEW.supplier_id,
        NEW.resolution, NEW.resolved_at, NEW.dismissal_reason) IS DISTINCT FROM (OLD.status, OLD.problem_code, OLD.problem_detail,
        OLD.supplier_invoice_id, OLD.supplier_id, OLD.resolution, OLD.resolved_at, OLD.dismissal_reason))) THEN
    RAISE EXCEPTION 'e-invoice % keeps what its processing read and decided', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_fixed';
  END IF;
  RETURN NEW;
END $$;
