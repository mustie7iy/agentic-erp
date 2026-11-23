-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000453's state by adding them back NOT VALID.
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt')) NOT VALID,
  DROP CONSTRAINT wms_message_direction,
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))) NOT VALID;
