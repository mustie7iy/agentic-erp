-- A return receipt (T85b; ADR-0067, As built T85b; 03-data-model §3.29):
-- a person records that quantities of a confirmed returns order's items
-- arrived on a date. Like a delivery confirmation (000292) it is kept in
-- the sales context: it moves no stock and posts nothing (the return
-- delivery, inspection and stock posting are Supply chain's, outside the
-- required scope); the credit memo request reads what was received
-- (T85c). A receipt is written once and never changed or deleted; it is
-- numbered within its returns order. Each of its items adds its quantity
-- to the returns order item's received_quantity, which never exceeds what
-- is returned, and the receipt moves the returns order's version, since
-- the returns order's representation shows what is received. The sales
-- order is not touched: its delivered, billed and returned quantities
-- stay.
-- Expand only (06 §6.4): a column with a default, two tables and a
-- permission, which the previous version neither reads nor writes; the
-- returns order's change rule admits what the previous version never
-- does.
ALTER TABLE sales.returns_order_item
  ADD COLUMN received_quantity numeric(20,6) NOT NULL DEFAULT 0,
  ADD CONSTRAINT returns_order_item_received CHECK (received_quantity >= 0 AND received_quantity <= quantity);

CREATE TABLE sales.return_receipt (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  returns_order_id  uuid NOT NULL,
  receipt_no        integer NOT NULL CONSTRAINT return_receipt_no_positive CHECK (receipt_no > 0),
  receipt_date      date NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT return_receipt_no_once UNIQUE (tenant_id, returns_order_id, receipt_no),
  -- What a receipt's items reference, so an item is of its receipt's returns order.
  CONSTRAINT return_receipt_of_order UNIQUE (tenant_id, id, returns_order_id),
  CONSTRAINT return_receipt_returns_order_fkey FOREIGN KEY (tenant_id, returns_order_id) REFERENCES sales.returns_order (tenant_id, id)
);
-- The list: a returns order's receipts by date.
CREATE INDEX return_receipt_by_date ON sales.return_receipt (tenant_id, returns_order_id, receipt_date, id);
CREATE TRIGGER return_receipt_immutable BEFORE UPDATE OR DELETE ON sales.return_receipt
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.return_receipt'::regclass);

-- An item of a receipt: a quantity more than zero of one item of the
-- receipt's returns order, in that item's unit; a returns order item at
-- most once per receipt.
CREATE TABLE sales.return_receipt_item (
  tenant_id         uuid NOT NULL,
  receipt_id        uuid NOT NULL,
  returns_order_id  uuid NOT NULL,
  item_no           integer NOT NULL,
  quantity          numeric(20,6) NOT NULL CONSTRAINT return_receipt_item_quantity_positive CHECK (quantity > 0),
  PRIMARY KEY (tenant_id, receipt_id, item_no),
  CONSTRAINT return_receipt_item_receipt_fkey FOREIGN KEY (tenant_id, receipt_id, returns_order_id)
    REFERENCES sales.return_receipt (tenant_id, id, returns_order_id),
  CONSTRAINT return_receipt_item_order_item_fkey FOREIGN KEY (tenant_id, returns_order_id, item_no)
    REFERENCES sales.returns_order_item (tenant_id, returns_order_id, item_no)
);
CREATE TRIGGER return_receipt_item_immutable BEFORE UPDATE OR DELETE ON sales.return_receipt_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.return_receipt_item'::regclass);

-- A confirmed returns order keeps its status; its version moves when a
-- receipt changes what its items show. The application's own update
-- stays as 000306 left it: a draft is confirmed, once. An update made
-- from within a trigger is admitted for a confirmed returns order that
-- stays confirmed, as the sales order's rule admits one (000292); the
-- receipt's trigger below is the only one that makes such an update, and
-- it moves the version and sets updated_by alone.
CREATE OR REPLACE FUNCTION sales.check_returns_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND pg_trigger_depth() > 1 THEN
    RETURN NEW;
  END IF;
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'returns order %: only a draft changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'returns_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;

-- Only a confirmed returns order takes a receipt, and each receipt moves
-- its version once. The application may update neither a confirmed
-- returns order nor its items, so the two functions run as the tables'
-- owner (000011); row-level security still applies to that owner. The
-- application reads the returns order FOR UPDATE first, which serializes
-- the receipts of one returns order (ADR-0008).
CREATE FUNCTION sales.record_return_receipt() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.returns_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.returns_order_id AND status = 'confirmed';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'return receipt %: its returns order is not confirmed', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'return_receipt_order_confirmed';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_return_receipt() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_return_receipt() FROM PUBLIC;
CREATE TRIGGER return_receipt_order_confirmed AFTER INSERT ON sales.return_receipt
  FOR EACH ROW EXECUTE FUNCTION sales.record_return_receipt();

-- A receipt's item adds its quantity to what is received of the returns
-- order item, so that quantity is always the sum of the item's receipts;
-- more than is returned is refused by returns_order_item_received.
CREATE FUNCTION sales.record_return_receipt_item() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.returns_order_item SET received_quantity = received_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND returns_order_id = NEW.returns_order_id AND item_no = NEW.item_no;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_return_receipt_item() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_return_receipt_item() FROM PUBLIC;
CREATE TRIGGER return_receipt_item_received AFTER INSERT ON sales.return_receipt_item
  FOR EACH ROW EXECUTE FUNCTION sales.record_return_receipt_item();

-- Confirming receipts (ADR-0067 §7); they are read with the returns
-- order (sales.return.read).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.return.receive', 'Confirm that quantities of a confirmed returns order arrived', false);
