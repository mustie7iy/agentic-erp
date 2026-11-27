-- migrate:no-transaction
-- S2l1 (ADR-0082 As built S2l1; 03-data-model §3.8): the suppliers'
-- delivery performance of a material at a plant (GET
-- /v1/supplier-delivery-performance) starts from the order items of the
-- material, as listPurchaseOrders' material_id filter does; without it
-- both read every order item of the tenant. Partial: an item of no
-- material is never looked up by one. Built concurrently (06 §6.4); a
-- failed build leaves an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY purchase_order_item_by_material ON procurement.purchase_order_item (tenant_id, material_id)
  WHERE material_id IS NOT NULL;
