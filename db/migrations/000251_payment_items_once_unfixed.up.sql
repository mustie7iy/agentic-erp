-- migrate:no-transaction
-- One payment per run and supplier for its items that fix no payment
-- method, what payment_items_once (000083) held for all items: built
-- concurrently (06 §6.4) before 000253 drops that index.
CREATE UNIQUE INDEX CONCURRENTLY payment_items_once_unfixed ON payments.payment (tenant_id, payment_run_id, supplier_id)
  WHERE kind = 'items' AND NOT method_fixed;
