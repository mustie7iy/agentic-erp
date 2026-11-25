-- Earlier counts, which the previous version cannot hold, are refused
-- rather than dropped (06 §6.4: a down migration loses no data). An item
-- recounted has one (count_item_recount_check), so none means no item was
-- recounted either.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.count_item_recount) THEN
    RAISE EXCEPTION 'earlier counts of count items exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- 000406's status flow and 000407's count_item_once.
CREATE OR REPLACE FUNCTION inventory.count_document_status_flow() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status <> OLD.status AND NOT ((OLD.status, NEW.status) IN (('counting', 'counted'), ('counted', 'pending_approval'), ('counted', 'posted'),
      ('pending_approval', 'posted'), ('pending_approval', 'rejected'), ('pending_approval', 'withdrawn'))) THEN
    RAISE EXCEPTION 'count_document_status_flow: count document %: % does not become %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'count_document_status_flow';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION inventory.count_item_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.counted_quantity IS NOT NULL AND (NEW.counted_quantity, NEW.entry_quantity, NEW.entry_uom_code, NEW.counted_at, NEW.counted_by)
        IS DISTINCT FROM (OLD.counted_quantity, OLD.entry_quantity, OLD.entry_uom_code, OLD.counted_at, OLD.counted_by))
      OR (NOT OLD.open AND NEW.open)
      OR (OLD.difference IS NOT NULL AND (NEW.difference, NEW.amount, NEW.material_document_id)
        IS DISTINCT FROM (OLD.difference, OLD.amount, OLD.material_document_id))
      OR (NEW.material_id, NEW.plant_id, NEW.storage_location_id, NEW.book_quantity, NEW.item_no, NEW.count_document_id)
        IS DISTINCT FROM (OLD.material_id, OLD.plant_id, OLD.storage_location_id, OLD.book_quantity, OLD.item_no, OLD.count_document_id) THEN
    RAISE EXCEPTION 'count_item_once: count document % item %: a count is entered once and an item closed and posted once', OLD.count_document_id,
      OLD.item_no USING ERRCODE = 'check_violation', CONSTRAINT = 'count_item_once';
  END IF;
  RETURN NEW;
END $$;
DROP TABLE inventory.count_item_recount;
DROP FUNCTION inventory.count_item_recount_check();
ALTER TABLE inventory.count_item DROP COLUMN recounts;
