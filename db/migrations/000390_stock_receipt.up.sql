-- Purchase order goods receipt into stock, GR/IR and the invoice against
-- valued receipts (S1e; ADR-0082 As built S1e; 03-data-model §3.9 and
-- §3.35). A purchase order item that names a storage location of a plant
-- is a stock item of its material: its goods receipts (101) and their
-- reversals (102) are material documents of the inventory context too,
-- posted in the receipt's transaction, and its supplier invoices put a
-- price difference on the stock of a moving-priced material as far as
-- the stock still holds the invoiced quantity (inventory.invoice_difference).
-- plant_id and storage_location_id are tenancy's org units and the
-- material document inventory's, by id: no foreign key leaves a schema
-- but ref's (02 §2.1). Expand only (06 §6.4): nullable columns the
-- previous version neither reads nor writes, a table it never reaches,
-- checks widened or added NOT VALID (validated by 000391), and an item
-- rule that the previous version, which writes neither column, always
-- passes.
ALTER TABLE procurement.purchase_order_item
  ADD COLUMN plant_id uuid,             -- tenancy context: an org unit of kind plant
  ADD COLUMN storage_location_id uuid,  -- tenancy context: an org unit of kind storage_location under the plant
  -- A stock item names its material, plant and storage location, and is
  -- assigned to no cost center: its receipts post to the stock.
  ADD CONSTRAINT purchase_order_item_stock CHECK ((plant_id IS NULL) = (storage_location_id IS NULL)
    AND (storage_location_id IS NULL OR (material_id IS NOT NULL AND account_assignment = 'none'))) NOT VALID;

-- A released order's item keeps its stock location as it keeps its
-- material (000065, 000161): its receipts are in that location's stock.
-- 000161's rule otherwise unchanged.
CREATE OR REPLACE FUNCTION procurement.check_order_item_editable() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text; applying uuid; received numeric;
BEGIN
  SELECT status, applying_change_id INTO st, applying FROM procurement.purchase_order
  WHERE tenant_id = NEW.tenant_id AND id = NEW.purchase_order_id;
  IF st = 'draft' THEN
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' AND st = 'pending_approval' AND OLD.valuation_price IS NULL AND NEW.valuation_price IS NOT NULL
     AND to_jsonb(NEW) - 'valuation_price' - 'valuation_price_unit' = to_jsonb(OLD) - 'valuation_price' - 'valuation_price_unit' THEN
    RETURN NEW;
  END IF;
  IF st <> 'released' OR applying IS NULL THEN
    RAISE EXCEPTION 'items of a % procurement.purchase_order cannot change', st USING ERRCODE = 'check_violation';
  END IF;
  IF NOT NEW.is_removed AND NEW.valuation_price IS NULL THEN
    RAISE EXCEPTION 'item % is added to released purchase order % with its valuation price', NEW.item_no, NEW.purchase_order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_valued';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.account_id, NEW.uom_code, NEW.material_id, NEW.contract_item_no, NEW.account_assignment, NEW.cost_center_id,
       NEW.plant_id, NEW.storage_location_id)
     IS DISTINCT FROM (OLD.account_id, OLD.uom_code, OLD.material_id, OLD.contract_item_no, OLD.account_assignment, OLD.cost_center_id,
       OLD.plant_id, OLD.storage_location_id) THEN
    RAISE EXCEPTION 'item % of released purchase order % keeps its account, assignment, unit, material, contract item and stock location', NEW.item_no,
      NEW.purchase_order_id USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_identity';
  END IF;
  SELECT coalesce(sum(quantity), 0) INTO received FROM procurement.goods_movement_item
  WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.purchase_order_id AND po_item_no = NEW.item_no;
  IF (NEW.is_removed AND received <> 0) OR NEW.quantity * (1 + NEW.over_delivery_pct / 100) < received THEN
    RAISE EXCEPTION 'item % of purchase order % would be below the % received', NEW.item_no, NEW.purchase_order_id, trim_scale(received)
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_received';
  END IF;
  RETURN NEW;
END $$;

-- A goods movement of stock items names the material document that moved
-- their stock (inventory context), by id and number.
ALTER TABLE procurement.goods_movement
  ADD COLUMN material_document_id uuid,
  ADD COLUMN material_document_number text,
  ADD CONSTRAINT goods_movement_material_document CHECK ((material_document_id IS NULL) = (material_document_number IS NULL));

-- A purchase order's goods receipt (101) and its reversal (102) are
-- material documents too, posted by procurement's goods movement, whose
-- number the document keeps as its reference.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302')) NOT VALID,
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('102', '502', '562', '202', '552', '312', '302'))) NOT VALID,
  ADD COLUMN reference text CONSTRAINT material_document_reference_check CHECK (length(reference) BETWEEN 1 AND 40),
  -- A goods receipt's document and only it names the goods movement.
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102'))) NOT VALID;

-- What a supplier invoice's price difference put on a moving-priced
-- material's stock in a plant (SAP's invoice posting to BSX, from
-- memory): the invoice, the order item, the amount and the journal entry
-- that posted it. Insert-only; it adds its amount to the valuation's
-- value and sets the moving price from it
-- (inventory.invoice_difference_valuation), so the stock account's balance
-- stays the valuation's value.
CREATE TABLE inventory.invoice_difference (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  valuation_id          uuid NOT NULL,
  supplier_invoice_id   uuid NOT NULL,    -- payables context
  purchase_order_id     uuid NOT NULL,    -- procurement context
  po_item_no            integer NOT NULL,
  amount                bigint NOT NULL CONSTRAINT invoice_difference_amount_check CHECK (amount <> 0),
  journal_entry_id      uuid NOT NULL,    -- ledger context
  journal_entry_year    smallint NOT NULL,
  journal_entry_number  text NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT invoice_difference_valuation_fkey FOREIGN KEY (tenant_id, valuation_id) REFERENCES inventory.valuation (tenant_id, id)
);
CREATE TRIGGER invoice_difference_insert_only BEFORE UPDATE OR DELETE ON inventory.invoice_difference
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- An invoice's differences, which its reversal reads.
CREATE INDEX invoice_difference_by_invoice ON inventory.invoice_difference (tenant_id, supplier_invoice_id);
SELECT platform.secure_table('inventory.invoice_difference'::regclass);
-- The difference is added to a moving-priced valuation holding stock, its
-- price following as movement_stock sets it: exactly, by integer
-- division of numerics. A value below zero is refused by
-- valuation_value_check.
CREATE FUNCTION inventory.invoice_difference_valuation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE inventory.valuation SET value = value + NEW.amount, version = version + 1, updated_by = NEW.created_by,
    price = div(2 * (value::numeric + NEW.amount) * price_unit + quantity, 2 * quantity)::bigint
  WHERE tenant_id = NEW.tenant_id AND id = NEW.valuation_id AND price_control = 'moving_average' AND quantity > 0;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'an invoice''s price difference goes to the stock of a moving-priced valuation holding some'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'invoice_difference_valuation';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION inventory.invoice_difference_valuation() OWNER TO erp_owner;
CREATE TRIGGER invoice_difference_valuation AFTER INSERT ON inventory.invoice_difference
  FOR EACH ROW EXECUTE FUNCTION inventory.invoice_difference_valuation();
