REVOKE UPDATE (valid_to, ended_by, ended_at) ON ledger.tax_code FROM erp_app;
DROP TRIGGER tax_code_end ON ledger.tax_code;
ALTER TABLE ledger.tax_code DROP COLUMN ended_at, DROP COLUMN ended_by;

DROP INDEX procurement.contract_item_material_key;
ALTER TABLE procurement.contract_item ADD CONSTRAINT contract_item_tenant_id_contract_id_material_key_key UNIQUE (tenant_id, contract_id, material_key);
REVOKE UPDATE ON procurement.requisition_item, procurement.contract_item, procurement.purchase_order_item, payables.supplier_invoice_item FROM erp_app;
DROP TRIGGER requisition_item_editable ON procurement.requisition_item;
DROP TRIGGER contract_item_editable ON procurement.contract_item;
DROP TRIGGER purchase_order_item_editable ON procurement.purchase_order_item;
DROP TRIGGER supplier_invoice_item_editable ON payables.supplier_invoice_item;
ALTER TABLE procurement.requisition_item DROP COLUMN is_removed;
ALTER TABLE procurement.contract_item DROP COLUMN is_removed;
ALTER TABLE procurement.purchase_order_item DROP COLUMN is_removed;
ALTER TABLE payables.supplier_invoice_item DROP COLUMN is_removed;
DROP FUNCTION platform.check_editable_parent();
