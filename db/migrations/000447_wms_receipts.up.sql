-- Inbound receipts to the WMS (S2q2; ADR-0082 As built S2q2; 03-data-model
-- §3.13): a released purchase order's stock items at a plant a wms
-- connector manages are sent to its warehouse system as receipts expected
-- to put away (kind inbound_receipt, outbound), once per order
-- (wms_message_once, its document the purchase order), and the goods
-- receipt it reports back (kind goods_receipt, inbound) is posted once
-- per message id, as 000445's goods issues are. document_id is the
-- procurement context's purchase order on these kinds: no foreign key
-- leaves the schema (02 §2.1). Expand only: the two checks naming the
-- kinds are widened NOT VALID (validated by 000448); the previous version
-- writes only the kinds they allowed before.
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt')) NOT VALID,
  DROP CONSTRAINT wms_message_direction,
  -- Each direction has its own kinds and statuses.
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))) NOT VALID;
