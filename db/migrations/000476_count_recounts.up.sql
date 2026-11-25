-- Recounts with history (S2d2; ADR-0082 As built S2d2; 03-data-model
-- §3.35): a counted item of a count document whose difference is not
-- posted is sent for a recount (SAP MI11, from memory): its count is kept
-- as an earlier count, numbered from 1, with who counted it and when and
-- who sent it for the recount and when, and the item is open to be
-- counted again; only its latest count is posted. An item takes at most 99
-- counts. Its document goes back from counted to counting while an item
-- waits for its recount. Expand only: a new insert-only table, a column
-- with a default the previous version neither reads nor writes, and two
-- triggers that refuse nothing the previous version writes (06 §6.4).
ALTER TABLE inventory.count_item
  ADD COLUMN recounts smallint NOT NULL DEFAULT 0 CONSTRAINT count_item_recounts_check CHECK (recounts BETWEEN 0 AND 98);
GRANT UPDATE (recounts) ON inventory.count_item TO erp_app;

-- An earlier count of an item: its number (the item's first count is 1),
-- the quantity counted in the base unit with the quantity and unit it was
-- entered in, when and by whom, and when and by whom it was sent for a
-- recount. Insert-only: what was counted is never changed.
CREATE TABLE inventory.count_item_recount (
  tenant_id          uuid NOT NULL,
  count_document_id  uuid NOT NULL,
  item_no            smallint NOT NULL,
  count_no           smallint NOT NULL CONSTRAINT count_item_recount_count_no_check CHECK (count_no BETWEEN 1 AND 98),
  counted_quantity   numeric(20,6) NOT NULL CONSTRAINT count_item_recount_counted_quantity_check CHECK (counted_quantity >= 0),
  entry_quantity     numeric(20,6) NOT NULL CONSTRAINT count_item_recount_entry_quantity_check CHECK (entry_quantity >= 0),
  entry_uom_code     text NOT NULL REFERENCES ref.unit_of_measure,
  counted_at         timestamptz NOT NULL,
  counted_by         uuid NOT NULL,
  recounted_at       timestamptz NOT NULL DEFAULT now(),
  recounted_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, count_document_id, item_no, count_no),
  CONSTRAINT count_item_recount_item_fkey FOREIGN KEY (tenant_id, count_document_id, item_no)
    REFERENCES inventory.count_item (tenant_id, count_document_id, item_no)
);
CREATE TRIGGER count_item_recount_insert_only BEFORE UPDATE OR DELETE ON inventory.count_item_recount
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('inventory.count_item_recount'::regclass);

-- An earlier count is the item's count as it is now: the item open,
-- counted and not posted, and the count numbered after its earlier ones.
CREATE FUNCTION inventory.count_item_recount_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT FROM inventory.count_item i WHERE i.tenant_id = NEW.tenant_id AND i.count_document_id = NEW.count_document_id
      AND i.item_no = NEW.item_no AND i.open AND i.difference IS NULL AND i.recounts + 1 = NEW.count_no
      AND (i.counted_quantity, i.entry_quantity, i.entry_uom_code, i.counted_at, i.counted_by)
        = (NEW.counted_quantity, NEW.entry_quantity, NEW.entry_uom_code, NEW.counted_at, NEW.counted_by)) THEN
    RAISE EXCEPTION 'count_item_recount_check: count document % item %: count % is not the open item''s count', NEW.count_document_id, NEW.item_no,
      NEW.count_no USING ERRCODE = 'check_violation', CONSTRAINT = 'count_item_recount_check';
  END IF;
  RETURN NEW;
END $$;
-- After the row's own checks, so a value they refuse is refused by them.
CREATE TRIGGER count_item_recount_check AFTER INSERT ON inventory.count_item_recount
  FOR EACH ROW EXECUTE FUNCTION inventory.count_item_recount_check();

-- An item is counted once per count, closed once and posted once; what it
-- counts and its book quantity never change. A count is taken off an item
-- only by a recount: the item open and not posted, its recounts one more,
-- and its count kept as that earlier count.
CREATE OR REPLACE FUNCTION inventory.count_item_once() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  recounted boolean := OLD.counted_quantity IS NOT NULL AND NEW.counted_quantity IS NULL AND OLD.open AND OLD.difference IS NULL
    AND NEW.recounts = OLD.recounts + 1
    AND EXISTS (SELECT FROM inventory.count_item_recount r WHERE r.tenant_id = OLD.tenant_id AND r.count_document_id = OLD.count_document_id
      AND r.item_no = OLD.item_no AND r.count_no = NEW.recounts);
BEGIN
  IF (OLD.counted_quantity IS NOT NULL AND NOT recounted AND (NEW.counted_quantity, NEW.entry_quantity, NEW.entry_uom_code, NEW.counted_at,
        NEW.counted_by) IS DISTINCT FROM (OLD.counted_quantity, OLD.entry_quantity, OLD.entry_uom_code, OLD.counted_at, OLD.counted_by))
      OR (NEW.recounts <> OLD.recounts AND NOT recounted)
      OR (NOT OLD.open AND NEW.open)
      OR (OLD.difference IS NOT NULL AND (NEW.difference, NEW.amount, NEW.material_document_id)
        IS DISTINCT FROM (OLD.difference, OLD.amount, OLD.material_document_id))
      OR (NEW.material_id, NEW.plant_id, NEW.storage_location_id, NEW.book_quantity, NEW.item_no, NEW.count_document_id)
        IS DISTINCT FROM (OLD.material_id, OLD.plant_id, OLD.storage_location_id, OLD.book_quantity, OLD.item_no, OLD.count_document_id) THEN
    RAISE EXCEPTION 'count_item_once: count document % item %: a count is entered once, taken off only by a recount that keeps it, and an item closed and posted once',
      OLD.count_document_id, OLD.item_no USING ERRCODE = 'check_violation', CONSTRAINT = 'count_item_once';
  END IF;
  RETURN NEW;
END $$;

-- A document moves forward only, except that a counted one goes back to
-- counting while an item of it waits for its recount.
CREATE OR REPLACE FUNCTION inventory.count_document_status_flow() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status <> OLD.status AND NOT ((OLD.status, NEW.status) IN (('counting', 'counted'), ('counted', 'pending_approval'), ('counted', 'posted'),
      ('pending_approval', 'posted'), ('pending_approval', 'rejected'), ('pending_approval', 'withdrawn'))
      OR ((OLD.status, NEW.status) = ('counted', 'counting') AND EXISTS (SELECT FROM inventory.count_item i WHERE i.tenant_id = NEW.tenant_id
        AND i.count_document_id = NEW.id AND i.counted_quantity IS NULL))) THEN
    RAISE EXCEPTION 'count_document_status_flow: count document %: % does not become %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'count_document_status_flow';
  END IF;
  RETURN NEW;
END $$;
