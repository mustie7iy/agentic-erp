-- The TMS connector (S2r; ADR-0082 As built S2r; 03-data-model §3.13,
-- §3.36): an external transport management system a tenant connects as an
-- integration connector of the kind tms, for the plants its settings name,
-- as a wms connector (000445). Each outbound delivery created at such a
-- plant is sent to it to plan (kind transport_request, outbound), once per
-- delivery (wms_message_once), and the shipment it reports back (kind
-- shipment, inbound) is recorded on the delivery once per message id:
-- integration.wms_message holds the messages of both logistics
-- connectors, on one queue, exchange and inbox (S2q2's precedent of a kind
-- each way). A recorded shipment posts nothing, so its status is
-- recorded, not posted. The delivery gains the shipment: its number and
-- carrier at the transport system, and its freight cost in minor units of
-- its currency (ADR-0005), set once, also after the goods issue. Expand
-- only: the checks naming the kinds and statuses are widened, and the new
-- columns' checks and foreign key added, NOT VALID (validated by 000450);
-- the trigger keeping a goods-issued delivery fixed is replaced by one
-- letting its shipment be set once, which the previous version never
-- writes.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms', 'tms')) NOT VALID;

ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check
    CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt', 'transport_request', 'shipment')) NOT VALID,
  DROP CONSTRAINT wms_message_status_check,
  ADD CONSTRAINT wms_message_status_check CHECK (status IN ('pending', 'sent', 'failed', 'posted', 'refused', 'recorded')) NOT VALID,
  DROP CONSTRAINT wms_message_direction,
  -- Each direction has its own kinds and statuses; a shipment is recorded,
  -- never posted, and only a shipment is recorded.
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt', 'transport_request'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))
    AND (status <> 'recorded' OR kind = 'shipment') AND (status <> 'posted' OR kind <> 'shipment')) NOT VALID;

-- The shipment a transport system planned the delivery in (S2r): its
-- number and carrier as the transport system names them, and the freight
-- cost it reports, all or none, recorded once. Nothing is posted (freight
-- settlement is Later).
ALTER TABLE shipping.outbound_delivery
  ADD COLUMN shipment_number      text,
  ADD COLUMN carrier_code         text,
  ADD COLUMN freight_amount       bigint,
  ADD COLUMN freight_currency     text,
  ADD COLUMN shipment_recorded_at timestamptz,
  ADD CONSTRAINT outbound_delivery_shipment_number_check CHECK (length(shipment_number) BETWEEN 1 AND 40) NOT VALID,
  ADD CONSTRAINT outbound_delivery_carrier_code_check CHECK (length(carrier_code) BETWEEN 1 AND 40) NOT VALID,
  ADD CONSTRAINT outbound_delivery_freight_amount_check CHECK (freight_amount >= 0) NOT VALID,
  ADD CONSTRAINT outbound_delivery_freight_currency_fkey FOREIGN KEY (freight_currency) REFERENCES ref.currency NOT VALID,
  ADD CONSTRAINT outbound_delivery_shipment CHECK ((shipment_number IS NULL) = (carrier_code IS NULL) AND (shipment_number IS NULL) = (freight_amount IS NULL)
    AND (shipment_number IS NULL) = (freight_currency IS NULL) AND (shipment_number IS NULL) = (shipment_recorded_at IS NULL)) NOT VALID;

-- A delivery changes only while created, and then only by its pick and
-- goods issue (the grant leaves its other columns out); one whose goods
-- issue posted changes no more, but by its shipment set (S2r). A recorded
-- shipment changes no more, whatever the delivery's status.
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
GRANT UPDATE (shipment_number, carrier_code, freight_amount, freight_currency, shipment_recorded_at) ON shipping.outbound_delivery TO erp_app;
