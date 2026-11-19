DROP TRIGGER catalog_item_material_fixed ON procurement.catalog_item;
DROP FUNCTION procurement.check_catalog_item_material();
REVOKE UPDATE (material_id) ON procurement.catalog_item FROM erp_app;
ALTER TABLE procurement.catalog_item DROP COLUMN material_id;
ALTER TABLE procurement.rfq_item DROP COLUMN material_id;
