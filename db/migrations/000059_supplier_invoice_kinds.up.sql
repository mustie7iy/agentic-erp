-- Credit memos and subsequent debits and credits (T34a, 03-data-model
-- §3.9). A supplier invoice is of a kind, as SAP's MIRO transactions are:
-- an invoice, a credit memo, a subsequent debit or a subsequent credit,
-- fixed once entered. Its amounts are not negative whatever the kind, which
-- gives the direction when it posts. A credit memo or subsequent
-- debit/credit may name the invoice it corrects, an invoice none; a
-- subsequent debit or credit revalues order items without quantity, so it
-- is on an order and its items are order items with quantity 0, where
-- every other kind's have a positive quantity.
--
-- The checks on existing rows are added NOT VALID, so adding them scans
-- nothing; a later migration validates them (06-engineering-process §6.4).
-- Those on the new columns hold for every row already: they are NULL or
-- 'invoice'.
ALTER TABLE payables.supplier_invoice
  ADD COLUMN kind text NOT NULL DEFAULT 'invoice'
    CONSTRAINT supplier_invoice_kind_check CHECK (kind IN ('invoice', 'credit_memo', 'subsequent_debit', 'subsequent_credit')),
  ADD COLUMN reference_invoice_id uuid;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_reference_fkey
  FOREIGN KEY (tenant_id, reference_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id) NOT VALID;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_not_its_own_reference CHECK (reference_invoice_id <> id) NOT VALID;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_reference_corrects
  CHECK (kind <> 'invoice' OR reference_invoice_id IS NULL) NOT VALID;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_subsequent_on_order
  CHECK (kind NOT IN ('subsequent_debit', 'subsequent_credit') OR purchase_order_id IS NOT NULL) NOT VALID;
-- Gross and tax were not checked for sign before; entry refuses negative
-- ones from this release on (payables.negative_amount), and the later
-- validation reports any row that remains.
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_amounts_not_negative
  CHECK (gross_amount >= 0 AND tax_amount >= 0) NOT VALID;
-- Payables' invoices are few next to journal lines; the index is built in
-- the migration's transaction.
CREATE INDEX supplier_invoice_by_reference ON payables.supplier_invoice (tenant_id, reference_invoice_id) WHERE reference_invoice_id IS NOT NULL;

CREATE FUNCTION payables.check_kind_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind IS DISTINCT FROM OLD.kind THEN
    RAISE EXCEPTION 'the kind of supplier invoice % is fixed once entered', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'supplier_invoice_kind_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER supplier_invoice_kind_fixed BEFORE UPDATE OF kind ON payables.supplier_invoice
  FOR EACH ROW EXECUTE FUNCTION payables.check_kind_fixed();

-- An item's quantity is 0 exactly on a subsequent debit or credit, whose
-- items are order items; a removed item no longer counts. The header's
-- kind is fixed, so checking the item when it is written is enough. The
-- quantity's own check is relaxed to >= 0 (a weaker rule every row meets).
ALTER TABLE payables.supplier_invoice_item DROP CONSTRAINT supplier_invoice_item_quantity_check;
ALTER TABLE payables.supplier_invoice_item ADD CONSTRAINT supplier_invoice_item_quantity_check CHECK (quantity >= 0) NOT VALID;
CREATE FUNCTION payables.check_item_quantity() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE subsequent boolean;
BEGIN
  IF NEW.is_removed THEN
    RETURN NEW;
  END IF;
  SELECT i.kind IN ('subsequent_debit', 'subsequent_credit') INTO subsequent FROM payables.supplier_invoice i
  WHERE i.tenant_id = NEW.tenant_id AND i.id = NEW.supplier_invoice_id;
  IF (NEW.quantity = 0) <> subsequent OR (subsequent AND NEW.purchase_order_id IS NULL) THEN
    RAISE EXCEPTION 'item % of supplier invoice %: a subsequent debit or credit has order items of quantity 0, every other kind items of a positive quantity',
      NEW.item_no, NEW.supplier_invoice_id USING ERRCODE = 'check_violation', CONSTRAINT = 'supplier_invoice_item_quantity_kind';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER supplier_invoice_item_quantity_kind BEFORE INSERT OR UPDATE ON payables.supplier_invoice_item
  FOR EACH ROW EXECUTE FUNCTION payables.check_item_quantity();
