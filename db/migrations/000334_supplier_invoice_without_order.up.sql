-- migrate:no-transaction
-- The purchase-to-pay event log (T103a, ADR-0080) pages a company's
-- supplier documents without an order of a period, each a case of its
-- own, by invoice date and id (payables.ProcessLog). Built concurrently
-- (06 §6.4); a failed build leaves an invalid index, dropped by hand
-- before the retry.
CREATE INDEX CONCURRENTLY supplier_invoice_without_order ON payables.supplier_invoice (tenant_id, legal_entity_id, invoice_date, id)
  WHERE purchase_order_id IS NULL;
