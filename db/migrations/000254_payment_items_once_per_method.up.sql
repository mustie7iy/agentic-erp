-- migrate:no-transaction
-- One payment per run, supplier and fixed payment method for the items
-- that fix it (ADR-0038, product owner 2026-11-10), built concurrently
-- (06 §6.4) before 000257 drops payment_items_once, which allowed one
-- items payment per supplier and run.
CREATE UNIQUE INDEX CONCURRENTLY payment_items_once_per_method ON payments.payment (tenant_id, payment_run_id, supplier_id, payment_method)
  WHERE kind = 'items' AND method_fixed;
