-- The trade compliance connector (S2s; ADR-0082 As built S2s;
-- 03-data-model §3.13, §3.36): an external trade compliance service a
-- tenant connects as an integration connector of the kind gts, for the
-- plants its settings name, as a wms or tms connector (000449, 000462).
-- Each outbound delivery created at such a plant is screened: created
-- pending, it is sent to the service once (kind screening_request,
-- outbound; wms_message_once), and the service's answer (kind
-- screening_result, inbound) is recorded on the delivery once per
-- message id: integration.wms_message holds the messages of every
-- logistics connector, on one queue, exchange and inbox. A recorded
-- answer posts nothing, so its status is recorded, as a shipment's is.
-- The delivery gains its trade compliance: pending until the service
-- answers, then released or blocked, the service's reason code and when
-- it answered, and who released it by hand and when, which a pending or
-- blocked delivery's goods issue waits for. It changes only while the
-- delivery is created (outbound_delivery_issued_fixed, unchanged). Expand
-- only: the checks naming the kinds are widened, and the new columns'
-- checks added, NOT VALID (validated by 000468); the previous version
-- writes none of them.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms', 'tms', 'gts')) NOT VALID;

ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check
    CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt', 'transport_request', 'shipment', 'screening_request',
      'screening_result')) NOT VALID,
  DROP CONSTRAINT wms_message_direction,
  -- Each direction has its own kinds and statuses; a shipment and a
  -- screening result are recorded, never posted, and only they are.
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt', 'transport_request',
      'screening_request'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))
    AND (status <> 'recorded' OR kind IN ('shipment', 'screening_result')) AND (status <> 'posted' OR kind NOT IN ('shipment', 'screening_result'))) NOT VALID;

-- A delivery's trade compliance (S2s): none when no trade compliance
-- service screens its plant; pending from its creation until the service
-- answers; then released or blocked by the service's latest answer, with
-- its reason code and when it answered; released by hand, by whom (named
-- as they were then) and
-- when. A pending delivery has no answer; a blocked one has the
-- service's; a released one the service's or a person's release.
ALTER TABLE shipping.outbound_delivery
  ADD COLUMN trade_compliance_status      text,
  ADD COLUMN trade_compliance_reason      text,
  ADD COLUMN trade_compliance_screened_at timestamptz,
  ADD COLUMN trade_compliance_released_by uuid, -- identity context
  ADD COLUMN trade_compliance_released_by_name text, -- the principal's display name then, as sales' credit release keeps it
  ADD COLUMN trade_compliance_released_at timestamptz,
  ADD CONSTRAINT outbound_delivery_trade_compliance_status_check CHECK (trade_compliance_status IN ('pending', 'released', 'blocked')) NOT VALID,
  ADD CONSTRAINT outbound_delivery_trade_compliance_reason_check CHECK (length(trade_compliance_reason) BETWEEN 1 AND 40) NOT VALID,
  ADD CONSTRAINT outbound_delivery_trade_compliance CHECK (
    (trade_compliance_status IS NOT NULL OR (trade_compliance_screened_at IS NULL AND trade_compliance_released_by IS NULL))
    AND (trade_compliance_reason IS NULL OR trade_compliance_screened_at IS NOT NULL)
    AND (trade_compliance_released_by IS NULL) = (trade_compliance_released_at IS NULL)
    AND (trade_compliance_released_by IS NULL) = (trade_compliance_released_by_name IS NULL)
    AND (trade_compliance_released_by IS NULL OR trade_compliance_status = 'released')
    AND (trade_compliance_status <> 'pending' OR trade_compliance_screened_at IS NULL)
    AND (trade_compliance_status <> 'blocked' OR trade_compliance_screened_at IS NOT NULL)
    AND (trade_compliance_status <> 'released' OR trade_compliance_screened_at IS NOT NULL OR trade_compliance_released_by IS NOT NULL)) NOT VALID;
GRANT UPDATE (trade_compliance_status, trade_compliance_reason, trade_compliance_screened_at, trade_compliance_released_by,
  trade_compliance_released_by_name, trade_compliance_released_at) ON shipping.outbound_delivery TO erp_app;

-- Releasing a delivery's trade compliance block by hand (S2s): a trade
-- compliance officer's, apart from posting its goods issue.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('shipping.trade_compliance.release', 'Release the trade compliance block of an outbound delivery', false);
