DROP TRIGGER requisition_item_unlink_catalog ON procurement.requisition_item;
DROP FUNCTION procurement.requisition_item_unlink_catalog();
ALTER TABLE procurement.requisition_item
  DROP CONSTRAINT requisition_item_catalog_item_fkey,
  DROP COLUMN catalog_item_id;
