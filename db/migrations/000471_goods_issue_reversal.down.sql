-- Reversed goods issues, which the previous version cannot hold, are
-- refused rather than dropped (06 §6.4: a down migration loses no data).
-- The guard reads every tenant's rows (as 000435's down does).
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM sales.delivery_reversal) OR EXISTS (SELECT FROM shipping.outbound_delivery WHERE status = 'goods_issue_reversed') THEN
    RAISE EXCEPTION 'reversed goods issues of outbound deliveries exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE sales.delivery_reversal;
DROP FUNCTION sales.record_delivery_reversal();
-- 000462's function (tms_connector), which knows no reversal.
CREATE OR REPLACE FUNCTION shipping.outbound_delivery_open() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  shipment CONSTANT text[] := ARRAY['shipment_number', 'carrier_code', 'freight_amount', 'freight_currency', 'shipment_recorded_at', 'version',
    'updated_by', 'updated_at'];
BEGIN
  IF OLD.shipment_number IS NOT NULL AND (NEW.shipment_number, NEW.carrier_code, NEW.freight_amount, NEW.freight_currency, NEW.shipment_recorded_at)
       IS DISTINCT FROM (OLD.shipment_number, OLD.carrier_code, OLD.freight_amount, OLD.freight_currency, OLD.shipment_recorded_at) THEN
    RAISE EXCEPTION 'outbound delivery %: its shipment is recorded, so it changes no more', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_shipment_kept';
  END IF;
  IF OLD.status <> 'created' AND NOT (OLD.shipment_number IS NULL AND NEW.shipment_number IS NOT NULL
       AND to_jsonb(NEW) - shipment = to_jsonb(OLD) - shipment) THEN
    RAISE EXCEPTION 'outbound delivery %: its goods issue is posted, so it changes no more', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_issued_fixed';
  END IF;
  RETURN NEW;
END $$;
REVOKE UPDATE (goods_issue_reversal_date, reversal_material_document_id, reversal_material_document_number) ON shipping.outbound_delivery FROM erp_app;
-- The checks go back to 000409's state, validated.
ALTER TABLE shipping.outbound_delivery
  DROP CONSTRAINT outbound_delivery_goods_issue_reversal,
  DROP CONSTRAINT outbound_delivery_reversal_material_document_number_check,
  DROP CONSTRAINT outbound_delivery_goods_issue,
  ADD CONSTRAINT outbound_delivery_goods_issue CHECK ((status = 'goods_issued') = (goods_issue_date IS NOT NULL)
    AND (status = 'goods_issued') = (material_document_id IS NOT NULL) AND (status = 'goods_issued') = (material_document_number IS NOT NULL)
    AND (status = 'goods_issued') = (currency IS NOT NULL)),
  DROP CONSTRAINT outbound_delivery_status_check,
  ADD CONSTRAINT outbound_delivery_status_check CHECK (status IN ('created', 'goods_issued')),
  DROP COLUMN reversal_material_document_number,
  DROP COLUMN reversal_material_document_id,
  DROP COLUMN goods_issue_reversal_date;
