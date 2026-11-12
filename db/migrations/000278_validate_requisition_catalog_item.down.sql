-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000277's state by adding it back NOT VALID.
ALTER TABLE procurement.requisition_item DROP CONSTRAINT requisition_item_catalog_item_fkey;
ALTER TABLE procurement.requisition_item ADD CONSTRAINT requisition_item_catalog_item_fkey FOREIGN KEY (tenant_id, catalog_item_id)
  REFERENCES procurement.catalog_item (tenant_id, id) NOT VALID;
