-- migrate:no-transaction
-- One payment per down payment request and run (T46c, 000080), built
-- concurrently (06 §6.4).
CREATE UNIQUE INDEX CONCURRENTLY payment_request_once ON payments.payment (tenant_id, payment_run_id, down_payment_request_id) WHERE kind = 'down_payment';
