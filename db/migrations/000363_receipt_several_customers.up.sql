-- One receipt for several customers (T75h; ADR-0063 §6 As built;
-- 03-data-model §3.26): an incoming payment may apply its receipt to the
-- open items of customers other than its own, the payer, as a parent pays
-- its subsidiaries' invoices. customer_id is the customer whose item it
-- is (the partner context's business partner); NULL on a row written
-- before this migration, or by the previous version during a rolling
-- upgrade, where it is always the payment's customer, so readers take
-- coalesce(customer_id, the payment's customer_id).
--
-- Expand only (06 §6.4): a nullable column and an index, which the
-- previous version neither reads nor writes.
ALTER TABLE receivables.incoming_payment_item ADD COLUMN customer_id uuid;
CREATE INDEX incoming_payment_item_by_customer ON receivables.incoming_payment_item (tenant_id, customer_id, incoming_payment_id)
  WHERE customer_id IS NOT NULL;
