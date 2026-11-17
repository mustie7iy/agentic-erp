-- A billing document cancelled (T79c; ADR-0067, As built T79c;
-- 03-data-model §3.29). As SAP's VF11 cancels a billing document, a
-- cancellation is a document of its own, numbered from the company's
-- billing_document range, dated, with a reason; a posted document's
-- cancellation names the customer credit memo that reversed its invoice
-- in receivables, by id and number, with that credit memo's journal
-- entry; a created one's names none, nothing having been posted. One
-- cancellation per billing document. Inserting it cancels the document,
-- takes the document's quantities off what is billed of its order's
-- items, so they are billed again, and moves the order's version.
--
-- A billing document's item now keeps the pricing it was billed at:
-- the order item's unit price, price unit, price source, conditions and
-- net value, filled from the order item when the item is inserted. An
-- order item's price changes once nothing of it is billed (T78d), which
-- a cancellation makes so again, and a billed document reads as it was
-- billed. The amounts billed of an order item are those of its documents
-- that are not cancelled.
--
-- Expand only (06 §6.4): columns the database fills, a status more, a
-- table, triggers; the previous version creates and posts billing
-- documents as before, and neither reads nor writes the new columns.
ALTER TABLE sales.billing_document_item
  ADD COLUMN unit_price       bigint,
  ADD COLUMN price_unit       integer,
  ADD COLUMN price_source     text,
  ADD COLUMN conditions       jsonb,
  ADD COLUMN order_net_amount bigint;
ALTER TABLE sales.billing_document_item DISABLE TRIGGER billing_document_item_immutable;
UPDATE sales.billing_document_item i SET unit_price = t.unit_price, price_unit = t.price_unit, price_source = t.price_source,
  conditions = t.conditions, order_net_amount = t.net_amount
FROM sales.sales_order_item t
WHERE t.tenant_id = i.tenant_id AND t.sales_order_id = i.sales_order_id AND t.item_no = i.order_item_no;
ALTER TABLE sales.billing_document_item ENABLE TRIGGER billing_document_item_immutable;
ALTER TABLE sales.billing_document_item
  ALTER COLUMN unit_price SET NOT NULL,
  ALTER COLUMN price_unit SET NOT NULL,
  ALTER COLUMN price_source SET NOT NULL,
  ALTER COLUMN conditions SET NOT NULL,
  ALTER COLUMN order_net_amount SET NOT NULL;

-- The pricing an item is billed at is its order item's when it is
-- billed, whatever the insert gives.
CREATE FUNCTION sales.price_billing_document_item() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  SELECT t.unit_price, t.price_unit, t.price_source, t.conditions, t.net_amount
  INTO NEW.unit_price, NEW.price_unit, NEW.price_source, NEW.conditions, NEW.order_net_amount
  FROM sales.sales_order_item t
  WHERE t.tenant_id = NEW.tenant_id AND t.sales_order_id = NEW.sales_order_id AND t.item_no = NEW.order_item_no;
  IF NOT FOUND THEN -- the foreign key's refusal, before the columns' NOT NULL meets it
    RAISE EXCEPTION 'billing document %: its order has no item % (billing_document_item_order_item_fkey)', NEW.billing_document_id, NEW.order_item_no
      USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'billing_document_item_order_item_fkey';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.price_billing_document_item() OWNER TO erp_owner;
CREATE TRIGGER billing_document_item_pricing BEFORE INSERT ON sales.billing_document_item
  FOR EACH ROW EXECUTE FUNCTION sales.price_billing_document_item();

-- What an item adds to its order item (000293), the amounts counted
-- over the documents that are not cancelled.
CREATE OR REPLACE FUNCTION sales.record_billing_document_item() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE ordered numeric; billed numeric; net bigint; amounts numeric;
BEGIN
  UPDATE sales.sales_order_item SET billed_quantity = billed_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND item_no = NEW.order_item_no
  RETURNING quantity, billed_quantity, net_amount INTO ordered, billed, net;
  SELECT sum(i.net_amount) INTO amounts FROM sales.billing_document_item i
  JOIN sales.billing_document b ON b.tenant_id = i.tenant_id AND b.id = i.billing_document_id
  WHERE i.tenant_id = NEW.tenant_id AND i.sales_order_id = NEW.sales_order_id AND i.order_item_no = NEW.order_item_no AND b.status <> 'cancelled';
  IF amounts > net OR (billed = ordered AND amounts <> net) THEN
    RAISE EXCEPTION 'billing document %: item % of its order would be billed % of its net value %', NEW.billing_document_id, NEW.order_item_no, amounts, net
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_item_amounts';
  END IF;
  RETURN NULL;
END $$;

-- A billing document is created, posted, or cancelled from either. A
-- posted one names its invoice, posting date and entry, a created one
-- none of them, a cancelled one what it had (000296's rule, widened).
ALTER TABLE sales.billing_document
  DROP CONSTRAINT billing_document_status_check,
  ADD CONSTRAINT billing_document_status_check CHECK (status IN ('created', 'posted', 'cancelled')),
  DROP CONSTRAINT billing_document_posted,
  ADD CONSTRAINT billing_document_posted CHECK (
    (status <> 'posted' OR customer_invoice_id IS NOT NULL)
    AND (status <> 'created' OR customer_invoice_id IS NULL)
    AND (customer_invoice_id IS NULL) = (customer_invoice_number IS NULL)
    AND (customer_invoice_id IS NULL) = (posting_date IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_id IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_year IS NULL));

-- The cancellation of a billing document: one per document, numbered
-- once per company from the billing_document range, dated, with a
-- reason; for a posted document the customer credit memo that reversed
-- its invoice and the credit memo's entry, all of them or none.
CREATE TABLE sales.billing_cancellation (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  document_number          text NOT NULL CONSTRAINT billing_cancellation_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id          uuid NOT NULL,     -- tenancy context
  billing_document_id      uuid NOT NULL,
  sales_order_id           uuid NOT NULL,
  cancellation_date        date NOT NULL,
  reason                   text NOT NULL CONSTRAINT billing_cancellation_reason_check CHECK (btrim(reason) <> '' AND length(reason) <= 500),
  customer_invoice_id      uuid,              -- receivables context: the credit memo
  customer_invoice_number  text CONSTRAINT billing_cancellation_invoice_number_check CHECK (length(customer_invoice_number) BETWEEN 1 AND 40),
  journal_entry_year       smallint,
  journal_entry_id         uuid,              -- ledger context
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT billing_cancellation_once UNIQUE (tenant_id, billing_document_id),
  CONSTRAINT billing_cancellation_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  CONSTRAINT billing_cancellation_document_fkey FOREIGN KEY (tenant_id, billing_document_id, sales_order_id)
    REFERENCES sales.billing_document (tenant_id, id, sales_order_id),
  CONSTRAINT billing_cancellation_credit_memo CHECK ((customer_invoice_id IS NULL) = (customer_invoice_number IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_id IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_year IS NULL))
);
CREATE TRIGGER billing_cancellation_immutable BEFORE UPDATE OR DELETE ON sales.billing_cancellation
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.billing_cancellation'::regclass);

-- A cancellation cancels its document, which is created or posted, of
-- its company: a posted one with a credit memo, a created one without.
-- It takes the document's quantities off what is billed of the order's
-- items and moves the order's version once. It runs as the tables'
-- owner, since the application may update neither a confirmed order nor
-- its billed quantities; row-level security still applies to that
-- owner. The application reads the document and then the order FOR
-- UPDATE first (ADR-0008).
CREATE FUNCTION sales.record_billing_cancellation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.billing_document SET status = 'cancelled', version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.billing_document_id AND legal_entity_id = NEW.legal_entity_id
    AND ((status = 'posted' AND NEW.customer_invoice_id IS NOT NULL) OR (status = 'created' AND NEW.customer_invoice_id IS NULL));
  IF NOT FOUND THEN
    RAISE EXCEPTION 'billing cancellation %: its document is not a created one, or a posted one with its credit memo, of its company', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_cancellation_document_open';
  END IF;
  UPDATE sales.sales_order_item t SET billed_quantity = t.billed_quantity - i.quantity
  FROM sales.billing_document_item i
  WHERE i.tenant_id = NEW.tenant_id AND i.billing_document_id = NEW.billing_document_id
    AND t.tenant_id = i.tenant_id AND t.sales_order_id = i.sales_order_id AND t.item_no = i.order_item_no;
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_billing_cancellation() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_billing_cancellation() FROM PUBLIC;
CREATE TRIGGER billing_cancellation_recorded AFTER INSERT ON sales.billing_cancellation
  FOR EACH ROW EXECUTE FUNCTION sales.record_billing_cancellation();

-- A created document is posted once (000296); a created or posted one
-- is cancelled once, by its cancellation's trigger alone, which changes
-- its status, version and who changed it and nothing it was posted with.
CREATE OR REPLACE FUNCTION sales.check_billing_document_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'created' AND NEW.status = 'posted' THEN
    RETURN NEW;
  END IF;
  IF OLD.status IN ('created', 'posted') AND NEW.status = 'cancelled' AND pg_trigger_depth() > 1
      AND (NEW.posting_date, NEW.customer_invoice_id, NEW.customer_invoice_number, NEW.journal_entry_year, NEW.journal_entry_id)
        IS NOT DISTINCT FROM (OLD.posting_date, OLD.customer_invoice_id, OLD.customer_invoice_number, OLD.journal_entry_year, OLD.journal_entry_id) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'billing document %: only a created document changes, and only by being posted, or a created or posted one by its cancellation', OLD.id
    USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_posted_once';
END $$;
