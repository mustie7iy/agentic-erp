-- migrate:no-transaction
-- The transmitter's queue (T44e; ADR-0038): files to send and files in
-- flight, by when they are due or their claim lapses. Partial, so the
-- files transmitted long ago are not in it. Built concurrently (06 §6.4); no IF NOT EXISTS, so an invalid index a failed build left is dropped by hand.
CREATE INDEX CONCURRENTLY bank_file_due ON payments.bank_file (tenant_id, next_attempt_at) WHERE status IN ('to_send', 'sending');
