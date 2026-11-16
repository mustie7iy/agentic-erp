-- migrate:no-transaction
-- The purchase-to-pay event log (T103a, ADR-0080) pages a company's
-- orders of a period by order date and id (procurement.ProcessLog): without
-- an index on them it read every order of the tenant for each page.
-- Built concurrently (06 §6.4); a failed build leaves an invalid index,
-- dropped by hand before the retry.
CREATE INDEX CONCURRENTLY purchase_order_by_date ON procurement.purchase_order (tenant_id, legal_entity_id, order_date, id);
