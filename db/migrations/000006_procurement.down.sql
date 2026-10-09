DROP TABLE IF EXISTS procurement.inbox, procurement.outbox, procurement.goods_movement_item, procurement.goods_movement,
  procurement.contract_release, procurement.purchase_order_item, procurement.purchase_order, procurement.contract_item,
  procurement.contract, procurement.requisition_item, procurement.requisition CASCADE;
DROP FUNCTION IF EXISTS procurement.check_receipt();
