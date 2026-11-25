-- Goods issue reversal of an outbound delivery not billed (S2b3; ADR-0083
-- As built S2b3; 03-data-model §3.36, §3.29). Shipping posts the 601's
-- reversal, a 602 naming it (000410 admits it), and the delivery ends
-- goods_issue_reversed, naming the 602 and its day; sales records the
-- reversal of the goods issue's delivery, which takes what it delivered
-- off the order's items. Expand only (06 §6.4): checks widened or
-- replaced NOT VALID (validated by 000454), nullable columns and a table
-- the previous version neither reads nor writes.

-- A delivery whose goods issue is reversed keeps its goods issue's day,
-- material document and cost's currency, and names its reversal's day
-- and material document (602); one not reversed names none.
ALTER TABLE shipping.outbound_delivery
  ADD COLUMN goods_issue_reversal_date          date,
  ADD COLUMN reversal_material_document_id      uuid,  -- inventory context: the 602
  ADD COLUMN reversal_material_document_number  text,
  DROP CONSTRAINT outbound_delivery_status_check,
  ADD CONSTRAINT outbound_delivery_status_check CHECK (status IN ('created', 'goods_issued', 'goods_issue_reversed')) NOT VALID,
  DROP CONSTRAINT outbound_delivery_goods_issue,
  ADD CONSTRAINT outbound_delivery_goods_issue CHECK ((status IN ('goods_issued', 'goods_issue_reversed')) = (goods_issue_date IS NOT NULL)
    AND (status IN ('goods_issued', 'goods_issue_reversed')) = (material_document_id IS NOT NULL)
    AND (status IN ('goods_issued', 'goods_issue_reversed')) = (material_document_number IS NOT NULL)
    AND (status IN ('goods_issued', 'goods_issue_reversed')) = (currency IS NOT NULL)) NOT VALID,
  ADD CONSTRAINT outbound_delivery_reversal_material_document_number_check CHECK (length(reversal_material_document_number) BETWEEN 1 AND 40) NOT VALID,
  -- A reversal is posted on or after the goods issue it reverses.
  ADD CONSTRAINT outbound_delivery_goods_issue_reversal CHECK ((status = 'goods_issue_reversed') = (goods_issue_reversal_date IS NOT NULL)
    AND (status = 'goods_issue_reversed') = (reversal_material_document_id IS NOT NULL)
    AND (status = 'goods_issue_reversed') = (reversal_material_document_number IS NOT NULL)
    AND (goods_issue_reversal_date IS NULL OR goods_issue_reversal_date >= goods_issue_date)) NOT VALID;

-- A delivery changes while created; once its goods issue is posted, only
-- by recording its shipment once (S2r) or by the reversal of its goods
-- issue, which changes its status and names the reversal and nothing
-- else; a reversed one changes no more but for its shipment.
CREATE OR REPLACE FUNCTION shipping.outbound_delivery_open() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  shipment CONSTANT text[] := ARRAY['shipment_number', 'carrier_code', 'freight_amount', 'freight_currency', 'shipment_recorded_at', 'version',
    'updated_by', 'updated_at'];
  reversal CONSTANT text[] := ARRAY['status', 'goods_issue_reversal_date', 'reversal_material_document_id', 'reversal_material_document_number',
    'version', 'updated_by', 'updated_at'];
BEGIN
  IF OLD.shipment_number IS NOT NULL AND (NEW.shipment_number, NEW.carrier_code, NEW.freight_amount, NEW.freight_currency, NEW.shipment_recorded_at)
       IS DISTINCT FROM (OLD.shipment_number, OLD.carrier_code, OLD.freight_amount, OLD.freight_currency, OLD.shipment_recorded_at) THEN
    RAISE EXCEPTION 'outbound delivery %: its shipment is recorded, so it changes no more', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_shipment_kept';
  END IF;
  IF OLD.status <> 'created' AND NOT (OLD.shipment_number IS NULL AND NEW.shipment_number IS NOT NULL
       AND to_jsonb(NEW) - shipment = to_jsonb(OLD) - shipment)
     AND NOT (OLD.status = 'goods_issued' AND NEW.status = 'goods_issue_reversed' AND to_jsonb(NEW) - reversal = to_jsonb(OLD) - reversal) THEN
    RAISE EXCEPTION 'outbound delivery %: its goods issue is posted, so it changes no more', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_issued_fixed';
  END IF;
  RETURN NEW;
END $$;
GRANT UPDATE (goods_issue_reversal_date, reversal_material_document_id, reversal_material_document_number) ON shipping.outbound_delivery TO erp_app;

-- The reversal of a goods issue's sales delivery (S2b3): written once,
-- under the order's row lock, on or after the delivery's day, naming the
-- 602 that reversed it; never changed or deleted. It takes each of the
-- delivery's items' quantities off the order item's delivered quantity,
-- which never falls below what is billed (sales_order_item_quantities),
-- and moves the order's version; a delivery confirmed by hand (T78c)
-- moved no stock and is not reversed here.
CREATE TABLE sales.delivery_reversal (
  tenant_id                 uuid NOT NULL,
  delivery_id               uuid NOT NULL,
  sales_order_id            uuid NOT NULL,
  reversal_date             date NOT NULL,
  material_document_id      uuid NOT NULL,  -- inventory context: the 602
  material_document_number  text NOT NULL CONSTRAINT delivery_reversal_material_document_number_check
                              CHECK (length(material_document_number) BETWEEN 1 AND 40),
  created_at                timestamptz NOT NULL DEFAULT now(),
  created_by                uuid NOT NULL,
  -- A delivery is reversed once.
  PRIMARY KEY (tenant_id, delivery_id),
  CONSTRAINT delivery_reversal_delivery_fkey FOREIGN KEY (tenant_id, delivery_id, sales_order_id)
    REFERENCES sales.delivery (tenant_id, id, sales_order_id)
);
CREATE TRIGGER delivery_reversal_immutable BEFORE UPDATE OR DELETE ON sales.delivery_reversal
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.delivery_reversal'::regclass);

-- Runs as the tables' owner, as sales.record_delivery does (000292): the
-- application updates neither a confirmed order nor its items; row-level
-- security still applies to that owner.
CREATE FUNCTION sales.record_delivery_reversal() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  delivered date;
BEGIN
  SELECT d.delivery_date INTO delivered FROM sales.delivery d
  WHERE d.tenant_id = NEW.tenant_id AND d.id = NEW.delivery_id AND d.outbound_delivery_id IS NOT NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'delivery reversal %: only a goods issue''s delivery is reversed', NEW.delivery_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'delivery_reversal_of_goods_issue';
  END IF;
  IF NEW.reversal_date < delivered THEN
    RAISE EXCEPTION 'delivery reversal %: dated before its delivery', NEW.delivery_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'delivery_reversal_date';
  END IF;
  UPDATE sales.sales_order_item t SET delivered_quantity = t.delivered_quantity - i.quantity
  FROM sales.delivery_item i
  WHERE i.tenant_id = NEW.tenant_id AND i.delivery_id = NEW.delivery_id
    AND t.tenant_id = i.tenant_id AND t.sales_order_id = i.sales_order_id AND t.item_no = i.order_item_no;
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id AND status = 'confirmed';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'delivery reversal %: its sales order is not confirmed', NEW.delivery_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'delivery_order_confirmed';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_delivery_reversal() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_delivery_reversal() FROM PUBLIC;
CREATE TRIGGER delivery_reversal_delivered AFTER INSERT ON sales.delivery_reversal
  FOR EACH ROW EXECUTE FUNCTION sales.record_delivery_reversal();
