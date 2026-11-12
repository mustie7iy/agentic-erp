-- A requisition item taken from the catalog (T84b; ADR-0071 §6;
-- plan-breadth-week2 §T84b): the catalog item it took its description,
-- unit and price from, none for an item the requester described. Expand
-- only (06 §6.4): the column is nullable without a default, rewriting
-- nothing, and its foreign key is added NOT VALID, reading none of the
-- rows under the lock; 000278 validates it. The previous release writes
-- items without the column.
ALTER TABLE procurement.requisition_item
  ADD COLUMN catalog_item_id uuid,
  ADD CONSTRAINT requisition_item_catalog_item_fkey FOREIGN KEY (tenant_id, catalog_item_id)
    REFERENCES procurement.catalog_item (tenant_id, id) NOT VALID;
