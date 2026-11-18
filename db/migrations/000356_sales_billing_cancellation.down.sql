-- The change rule as 000296 made it.
CREATE OR REPLACE FUNCTION sales.check_billing_document_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'created' OR NEW.status <> 'posted' THEN
    RAISE EXCEPTION 'billing document %: only a created document changes, and only by being posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_posted_once';
  END IF;
  RETURN NEW;
END $$;
DROP TABLE sales.billing_cancellation;
DROP FUNCTION sales.record_billing_cancellation();
-- The status and posting checks restored refuse the revert while a
-- cancelled document exists: the previous version knows none, and its
-- credit memo and entry stay where they are.
ALTER TABLE sales.billing_document
  DROP CONSTRAINT billing_document_posted,
  ADD CONSTRAINT billing_document_posted CHECK (
    (status = 'posted') = (customer_invoice_id IS NOT NULL)
    AND (customer_invoice_id IS NULL) = (customer_invoice_number IS NULL)
    AND (customer_invoice_id IS NULL) = (posting_date IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_id IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_year IS NULL)),
  DROP CONSTRAINT billing_document_status_check,
  ADD CONSTRAINT billing_document_status_check CHECK (status IN ('created', 'posted'));
-- The amounts rule as 000293 made it.
CREATE OR REPLACE FUNCTION sales.record_billing_document_item() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE ordered numeric; billed numeric; net bigint; amounts numeric;
BEGIN
  UPDATE sales.sales_order_item SET billed_quantity = billed_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND item_no = NEW.order_item_no
  RETURNING quantity, billed_quantity, net_amount INTO ordered, billed, net;
  SELECT sum(net_amount) INTO amounts FROM sales.billing_document_item
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND order_item_no = NEW.order_item_no;
  IF amounts > net OR (billed = ordered AND amounts <> net) THEN
    RAISE EXCEPTION 'billing document %: item % of its order would be billed % of its net value %', NEW.billing_document_id, NEW.order_item_no, amounts, net
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_item_amounts';
  END IF;
  RETURN NULL;
END $$;
DROP TRIGGER billing_document_item_pricing ON sales.billing_document_item;
DROP FUNCTION sales.price_billing_document_item();
ALTER TABLE sales.billing_document_item
  DROP COLUMN order_net_amount,
  DROP COLUMN conditions,
  DROP COLUMN price_source,
  DROP COLUMN price_unit,
  DROP COLUMN unit_price;
