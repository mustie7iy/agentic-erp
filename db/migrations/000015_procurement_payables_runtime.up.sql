-- Runtime support for procurement, payables and workflow (T4).

-- Drafts are edited through the API, which replaces their items as a whole.
-- Items are never deleted: an edit updates items in place, adds new ones and
-- marks the ones beyond the new list removed (they come back if the list
-- grows again), so item numbers stay 1..n. Only items of a document that is
-- still being edited may change.
CREATE FUNCTION platform.check_editable_parent() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text;
BEGIN
  -- TG_ARGV: parent table, column referring to it, editable statuses...
  EXECUTE format('SELECT status FROM %s WHERE tenant_id = $1 AND id = $2', TG_ARGV[0])
    INTO st USING NEW.tenant_id, (to_jsonb(NEW) ->> TG_ARGV[1])::uuid;
  IF NOT st = ANY (TG_ARGV[2:]) THEN
    RAISE EXCEPTION 'items of a % % cannot change', st, TG_ARGV[0] USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

ALTER TABLE procurement.requisition_item ADD COLUMN is_removed boolean NOT NULL DEFAULT false;
ALTER TABLE procurement.contract_item ADD COLUMN is_removed boolean NOT NULL DEFAULT false;
ALTER TABLE procurement.purchase_order_item ADD COLUMN is_removed boolean NOT NULL DEFAULT false;
ALTER TABLE payables.supplier_invoice_item ADD COLUMN is_removed boolean NOT NULL DEFAULT false;
CREATE TRIGGER requisition_item_editable BEFORE UPDATE ON procurement.requisition_item
  FOR EACH ROW EXECUTE FUNCTION platform.check_editable_parent('procurement.requisition', 'requisition_id', 'draft');
CREATE TRIGGER contract_item_editable BEFORE UPDATE ON procurement.contract_item
  FOR EACH ROW EXECUTE FUNCTION platform.check_editable_parent('procurement.contract', 'contract_id', 'draft');
CREATE TRIGGER purchase_order_item_editable BEFORE UPDATE ON procurement.purchase_order_item
  FOR EACH ROW EXECUTE FUNCTION platform.check_editable_parent('procurement.purchase_order', 'purchase_order_id', 'draft');
CREATE TRIGGER supplier_invoice_item_editable BEFORE UPDATE ON payables.supplier_invoice_item
  FOR EACH ROW EXECUTE FUNCTION platform.check_editable_parent('payables.supplier_invoice', 'supplier_invoice_id', 'parked');
GRANT UPDATE ON procurement.requisition_item, procurement.contract_item, procurement.purchase_order_item, payables.supplier_invoice_item TO erp_app;
-- A contract item's material key is unique among the live items only.
ALTER TABLE procurement.contract_item DROP CONSTRAINT contract_item_tenant_id_contract_id_material_key_key;
CREATE UNIQUE INDEX contract_item_material_key ON procurement.contract_item (tenant_id, contract_id, material_key) WHERE NOT is_removed;

-- Tax codes come from the configuration document; a changed rate ends the
-- current one and starts the new one on the activation day.
ALTER TABLE ledger.tax_code ADD COLUMN ended_at timestamptz, ADD COLUMN ended_by uuid;
CREATE TRIGGER tax_code_end BEFORE UPDATE ON ledger.tax_code FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
GRANT UPDATE (valid_to, ended_by, ended_at) ON ledger.tax_code TO erp_app;
