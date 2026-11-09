-- An e-invoice's changed bytes and the fetch's gauge (T65b; ADR-0061 §4,
-- §8; 03-data-model §3.25). A provider that delivers other bytes under a
-- message id already recorded does not replace the document: the row
-- keeps the first bytes, the legal original, and changed_hash records the
-- SHA-256 of the latest different bytes, with when and by which fetch, set
-- by an update guarded on changed_hash IS DISTINCT FROM the new hash, so a
-- redelivery of the same changed bytes changes nothing again. Expand only:
-- three nullable columns the previous version neither reads nor writes,
-- the trigger replaced to let them change, and the worker role's read of
-- the two columns the unacknowledged gauge counts (000234 validates the
-- checks, 000235 and 000236 add the indexes).
ALTER TABLE einvoicing.einvoice
  ADD COLUMN changed_hash bytea,
  ADD COLUMN changed_at timestamptz,
  ADD COLUMN changed_fetch_operation_id uuid,  -- platform.operation
  ADD CONSTRAINT einvoice_changed_hash CHECK (length(changed_hash) = 32) NOT VALID,
  -- Set together, never to the bytes kept, and never before the document
  -- was received. NOT VALID here, validated by 000234 (06 §6.4), so adding
  -- them scans nothing under the table's lock; new rows and updates are
  -- checked at once.
  ADD CONSTRAINT einvoice_changed CHECK ((changed_hash IS NULL) = (changed_at IS NULL)
    AND (changed_hash IS NULL) = (changed_fetch_operation_id IS NULL)
    AND changed_hash IS DISTINCT FROM sha256 AND (changed_at IS NULL OR changed_at >= received_at)) NOT VALID;
GRANT UPDATE (changed_hash, changed_at, changed_fetch_operation_id) ON einvoicing.einvoice TO erp_app;

-- As 000231's, and a change of bytes, once recorded, is never undone.
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

-- erp_einvoices_unacknowledged (ADR-0043's business gauges): the worker
-- role counts, across tenants, the documents recorded over an hour ago
-- and not yet acknowledged, and reads nothing else of them.
GRANT USAGE ON SCHEMA einvoicing TO erp_worker;
GRANT SELECT (received_at, acknowledged_at) ON einvoicing.einvoice TO erp_worker;
