-- migrate:no-transaction
-- A supplier's items that fix a payment method are paid in a payment of
-- their own beside its other items (ADR-0038, product owner 2026-11-10):
-- one items payment per supplier and run (000083) gives way to
-- payment_items_once_unfixed and payment_items_once_per_method (000250,
-- 000251), built already. The version before writes no fixed method, so
-- its payments are held by payment_items_once_unfixed exactly as before.
DROP INDEX CONCURRENTLY IF EXISTS payments.payment_items_once;
