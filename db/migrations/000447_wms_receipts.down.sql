-- Messages of the kinds the previous version cannot hold are refused
-- rather than dropped (06 §6.4: a down migration loses no data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM integration.wms_message WHERE kind IN ('inbound_receipt', 'goods_receipt')) THEN
    RAISE EXCEPTION 'wms receipt messages exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check CHECK (kind IN ('outbound_delivery', 'goods_issue')),
  DROP CONSTRAINT wms_message_direction,
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind = 'outbound_delivery')
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed')));
