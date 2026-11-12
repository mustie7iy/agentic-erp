ALTER TABLE procurement.requisition_item
  DROP CONSTRAINT requisition_item_catalog_item_fkey,
  DROP COLUMN catalog_item_id;
