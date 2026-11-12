-- A requisition item taken from the catalog (T84b; ADR-0071 §6;
-- plan-breadth-week2 §T84b): the catalog item it took its description,
-- unit and price from, none for an item the requester described. Expand
-- only (06 §6.4): the column is nullable without a default, rewriting
-- nothing, and its foreign key is added NOT VALID, reading none of the
-- rows under the lock; 000289 validates it.
ALTER TABLE procurement.requisition_item
  ADD COLUMN catalog_item_id uuid,
  ADD CONSTRAINT requisition_item_catalog_item_fkey FOREIGN KEY (tenant_id, catalog_item_id)
    REFERENCES procurement.catalog_item (tenant_id, id) NOT VALID;

-- Releases are rolling (06 §6.4). The previous release inserts items
-- without the column, and replaces a draft's items by an upsert that does
-- not set it: an item it rewrites in place would keep the catalog item of
-- the row it replaced, with a description or a price the requester gave.
-- So an update that changes what the item took from the catalog (its
-- description, unit, price or price unit) and leaves catalog_item_id as
-- it was ends the link. This release is not touched by it: a catalog item
-- never changes, so writing the same catalog item again writes the same
-- values, and another catalog item, or none, changes the column itself.
CREATE FUNCTION procurement.requisition_item_unlink_catalog() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.catalog_item_id IS NOT NULL AND NEW.catalog_item_id = OLD.catalog_item_id
     AND (NEW.description, NEW.uom_code, NEW.unit_price, NEW.price_unit)
         IS DISTINCT FROM (OLD.description, OLD.uom_code, OLD.unit_price, OLD.price_unit) THEN
    NEW.catalog_item_id := NULL;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER requisition_item_unlink_catalog BEFORE UPDATE ON procurement.requisition_item
  FOR EACH ROW EXECUTE FUNCTION procurement.requisition_item_unlink_catalog();
