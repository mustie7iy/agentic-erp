-- Validates the checks 000395 added NOT VALID (06 §6.4).
ALTER TABLE sales.quotation_item VALIDATE CONSTRAINT quotation_item_material_key;
ALTER TABLE sales.sales_order_item VALIDATE CONSTRAINT sales_order_item_material_key;
