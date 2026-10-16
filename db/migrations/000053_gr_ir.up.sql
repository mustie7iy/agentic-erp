-- GR/IR clearing (T33a, 03-data-model §3.7-3.9). The schema the later T33
-- steps post with: the price difference account of the account
-- determination; the order item a journal line belongs to, so the ledger
-- keeps each order item's GR/IR position on its own lines (as SAP's ACDOCA
-- carries EBELN/EBELP) and the positions add up to the GR/IR account's
-- balance; an order's valuation, fixed when it is released; and the entry
-- a goods movement posted. References to another context are by id, without
-- a foreign key (02-architecture, boundary rule 1).
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding'));

ALTER TABLE ledger.journal_line ADD COLUMN purchase_order_id uuid, -- procurement context
  ADD COLUMN po_item_no integer;
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_order_item_pair
  CHECK ((purchase_order_id IS NULL) = (po_item_no IS NULL));
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_order_item_quantity
  CHECK (purchase_order_id IS NULL OR quantity IS NOT NULL);
CREATE INDEX journal_line_order_item ON ledger.journal_line (tenant_id, legal_entity_id, purchase_order_id, po_item_no)
  WHERE purchase_order_id IS NOT NULL;

-- A line on the legal entity's GR/IR account names its order item, so the
-- GR/IR account is posted only by goods receipts and invoices. Entries a
-- migration run loaded (integration) are exempt: SAP's GR/IR lines arrive
-- without their order items until those are migrated too.
CREATE FUNCTION ledger.check_gr_ir_line() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.purchase_order_id IS NULL
     AND EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = NEW.tenant_id
                 AND d.legal_entity_id = NEW.legal_entity_id AND d.key = 'gr_ir' AND d.account_id = NEW.account_id)
     AND NOT EXISTS (SELECT FROM ledger.journal_entry e WHERE e.tenant_id = NEW.tenant_id AND e.fiscal_year = NEW.fiscal_year
                     AND e.id = NEW.journal_entry_id AND e.source_context = 'integration') THEN
    RAISE EXCEPTION 'line % of entry % is on the GR/IR account without its order item', NEW.line_no, NEW.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_gr_ir_order_item';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER journal_line_gr_ir AFTER INSERT ON ledger.journal_line
  FOR EACH ROW EXECUTE FUNCTION ledger.check_gr_ir_line();

-- How an order's receipts and invoices are valued: directly on the order
-- item's account, or through GR/IR. Decided when the order is released
-- (from the account determination's gr_ir) and fixed from then on, so
-- orders released before GR/IR was configured keep the direct scheme.
ALTER TABLE procurement.purchase_order ADD COLUMN valuation text NOT NULL DEFAULT 'direct'
  CONSTRAINT purchase_order_valuation_check CHECK (valuation IN ('direct', 'gr_ir'));
ALTER TABLE procurement.purchase_order ADD CONSTRAINT purchase_order_draft_direct
  CHECK (status <> 'draft' OR valuation = 'direct');
CREATE FUNCTION procurement.check_valuation_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.valuation IS DISTINCT FROM OLD.valuation AND NOT (OLD.status = 'draft' AND NEW.status = 'released') THEN
    RAISE EXCEPTION 'the valuation of purchase order % is fixed when it is released', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_valuation_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER purchase_order_valuation_fixed BEFORE UPDATE OF valuation ON procurement.purchase_order
  FOR EACH ROW EXECUTE FUNCTION procurement.check_valuation_fixed();

-- The journal entry a goods movement posted (ledger context), on orders
-- valued through GR/IR.
ALTER TABLE procurement.goods_movement ADD COLUMN journal_entry_year smallint,
  ADD COLUMN journal_entry_id uuid;
ALTER TABLE procurement.goods_movement ADD CONSTRAINT goods_movement_entry_pair
  CHECK ((journal_entry_id IS NULL) = (journal_entry_year IS NULL));
