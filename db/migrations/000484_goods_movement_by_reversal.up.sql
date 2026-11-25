-- migrate:no-transaction
-- S2l1 (ADR-0082 As built S2l1; 03-data-model §3.8): a goods receipt
-- counts as a delivery unless a reversal (102) names it, which the
-- suppliers' delivery performance and the receipt's own read
-- (reversed_by) look up by the movement reversed. Partial: only a
-- reversal names one. Built concurrently (06 §6.4); a failed build
-- leaves an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY goods_movement_by_reversal ON procurement.goods_movement (tenant_id, reversal_of_id)
  WHERE reversal_of_id IS NOT NULL;
